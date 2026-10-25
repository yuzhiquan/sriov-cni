# 03 · Writing a SR-IOV CNI from Scratch

## 1. The "Contract" of a CNI Plugin

A CNI plugin is just an executable. The container runtime invokes it through **environment variables + stdin + stdout**:

**Environment variables**

| Variable | Meaning |
|---|---|
| `CNI_COMMAND` | `ADD` / `DEL` / `CHECK` / `VERSION` |
| `CNI_CONTAINERID` | Container ID |
| `CNI_NETNS` | Path of the target netns, e.g. `/proc/12345/ns/net` |
| `CNI_IFNAME` | Interface name inside the container, e.g. `net1` |
| `CNI_ARGS` | `K8S_POD_NAME=...;K8S_POD_NAMESPACE=...;...` |
| `CNI_PATH` | Plugin search path |

**stdin**: the network configuration JSON (for Multus, this is the content of the `config` field in the NetworkAttachmentDefinition).

**stdout**: `ADD` returns a `Result` JSON (containing interfaces / ips / routes / dns).

**Idempotency requirements**: if `ADD` fails, it must clean up after itself; `DEL` must not report an error when the thing to delete "no longer exists".

The `github.com/containernetworking/cni/pkg/skel` package wraps all of this boilerplate:

```go
func main() {
    skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, "CNI sriov plugin v0.1.0")
}
```

---

## 2. Project Structure

```
cmd/sriov/main.go      Plugin entry point: the three main flows ADD / DEL / CHECK
pkg/config/config.go   Config parsing and validation (deviceID / mac / vlan / spoofchk / trust ...)
pkg/config/cache.go    Caches the original VF state in /var/lib/cni/sriov so DEL can restore it
pkg/config/linkstate.go Link-state constants
pkg/sriov/sriov.go     ★ VF topology discovery (sysfs parsing)
pkg/sriov/configure.go ★ Applying / restoring VF hardware attributes / moving between netns
pkg/ipam/ipam.go       IPAM delegate + applying addresses / routes / policy routing
pkg/ns/ns.go           netns enter/exit wrapper (including thread locking)
pkg/logging/logging.go Minimal logging
```

---

## 3. Step 1: Find the VF from the deviceID (`pkg/sriov/sriov.go`)

This is the starting point of the whole plugin, and also the easiest place to get wrong. It boils down to three sysfs paths:

```go
const sysBusPci = "/sys/bus/pci/devices"

// 1) VF -> netdev
/sys/bus/pci/devices/<deviceID>/net/<netdev name>

// 2) VF -> PF (physfn is a symlink that only VFs have; PFs do not)
/sys/bus/pci/devices/<deviceID>/physfn   ->  ../0000:03:00.0

// 3) PF -> the N-th VF (reverse lookup of vfID)
/sys/bus/pci/devices/<pfBDF>/virtfn<N>   ->  ../0000:03:02.N
```

The corresponding implementation (`GetVFInfo`):

```go
func GetVFInfo(conf *config.NetConf) (*VFInfo, error) {
    vfDir := filepath.Join(sysBusPci, deviceID)

    netdev, err := netDevForPci(deviceID)      // ① The VF's netdev name
    if err != nil {
        return nil, err
    }

    pfBDF, err := filepath.EvalSymlinks(filepath.Join(vfDir, "physfn"))  // ② Find the PF
    if err != nil {
        return nil, fmt.Errorf("sriov: %s is not a VF (no physfn link): %v", deviceID, err)
    }
    pfBDF = filepath.Base(pfBDF)               // "0000:03:00.0"
    pfName, err := netDevForPci(pfBDF)         // "ens1f0"

    vfID, err := findVfID(pfBDF, deviceID)     // ③ Scan virtfnN to look up the index
    ...
}
```

**Two easy pitfalls**:

> **Pitfall 1**: The most common reason `netDevForPci` fails is that **the VF is not bound to a kernel driver** (for example, it is bound to `vfio-pci` for DPDK). In DPDK mode the VF has no netdev at all, and you must take the `"dpdk": true` branch.

> **Pitfall 2**: `vfID` and the Function number of the PCI address are **not necessarily equal**. On older NICs they are often equal (`03:02.3` ↔ `vf 3`), but on a lot of newer hardware (especially with ARI enabled) they are not. So you must scan `virtfn*` for the reverse lookup rather than parse the string.

The unit test `pkg/sriov/sriov_test.go` covers this logic with a fake sysfs tree:

```bash
$ go test ./pkg/sriov/ -v
--- PASS: TestGetVFInfo (0.00s)
--- PASS: TestGetVFInfoNotAVF (0.00s)
--- PASS: TestFindVfIDNoVFs (0.00s)
```

`pkg/ns/ns_test.go` verifies the netns enter/exit of `Do()` against the real kernel: create a dummy NIC → move it into a new netns → confirm it disappears on the host side and is visible in the new netns, and that the thread really switched back to the host netns after the closure returned.

---

## 4. Step 2: Program the VF Hardware Attributes (`pkg/sriov/configure.go`)

### 4.1 Why changing only the netdev's MAC is not enough

A VF's MAC/VLAN/spoofchk/trust/link-state **are not netdev attributes; they are table entries inside the NIC hardware**. Changing them is equivalent to running:

```bash
ip link set ens1f0 vf 3 mac 02:11:22:33:44:55 vlan 1000 spoofchk off trust on
```

In Go, this maps to the netlink `IFLA_VF_*` family:

```go
netlink.LinkSetVfHardwareAddr(pfLink, vfID, hwaddr)  // IFLA_VF_MAC
netlink.LinkSetVfVlanQos(pfLink, vfID, vlan, qos)     // IFLA_VF_VLAN
netlink.LinkSetVfSpoofchk(pfLink, vfID, on)            // IFLA_VF_SPOOFCHK
netlink.LinkSetVfTrust(pfLink, vfID, on)               // IFLA_VF_TRUST
netlink.LinkSetVfState(pfLink, vfID, state)            // IFLA_VF_LINK_STATE
netlink.LinkSetVfRate(pfLink, vfID, minRate, maxRate)  // IFLA_VF_RATE
```

> **Mind the arguments**: the first argument of these functions is the **PF's link**, not the VF's. This is why PFName and vfID must be resolved first.

### 4.2 The order matters

```go
// 1) down -- most drivers (iavf/ice/i40e) do not allow changing the MAC while up
netlink.LinkSetDown(vfLink)

// 2) spoofchk -- turn it off first, otherwise the hardware rejects the MAC change in the next step
netlink.LinkSetVfSpoofchk(pfLink, vfID, spoofchkOn)

// 3) MAC
netlink.LinkSetVfHardwareAddr(pfLink, vfID, hwaddr)
//    On failure, fall back to netlink.LinkSetHardwareAddr(vfLink, hwaddr)

// 4) VLAN (the hardware adds/strips the tag)
netlink.LinkSetVfVlanQos(pfLink, vfID, vlan, qos)

// 5) trust / link-state / rate limit
```

### 4.3 Why configure **before** moving into the netns

Two reasons:

1. **Readability/diagnosability**: when something goes wrong, `ip link show ens1f0` on the host shows the VF state without needing `nsenter`.
2. **Driver compatibility**: some drivers return odd errors for VF attribute writes when the VF netdev sits in a non-init netns.

(Technically, VLAN/trust and similar settings still work through the PF after the move, because the operation targets the PF. The MAC, however, needs care at the netdev level.)

### 4.4 Saving the original state

```go
type VfState struct {
    HostIFName   string  // Original netdev name, to be restored on DEL
    EffectiveMAC string
    AdminMAC     string  // MAC recorded on the PF side
    Vlan         int
    VlanQoS      int
    SpoofChk     bool
    Trust        bool
    LinkState    string
    MinTxRate    int
    MaxTxRate    int
    MTU          int
    LinkUp       bool
}
```

Read it from the PF's `Attrs().Vfs`:

```go
for _, vf := range pfLink.Attrs().Vfs {
    if vf.ID != info.VFID { continue }
    state.AdminMAC  = vf.Mac.String()
    state.Vlan      = vf.Vlan
    state.VlanQoS   = vf.Qos
    state.SpoofChk  = vf.Spoofchk
    state.Trust     = vf.Trust != 0
    state.LinkState = config.LinkStateString(uint32(vf.LinkState))
    ...
}
```

> Fetch the PF with `LinkList()` rather than `LinkByName()`: only a full link dump carries `IFLA_VFINFO` (i.e. `Attrs().Vfs`). This is a very subtle pitfall.

---

## 5. Step 3: Move into the Pod netns

```go
netlink.LinkSetNsFd(vfLink, netnsFd)
```

One line of code, backed by the kernel's `dev_change_net_namespace()`. It:

- Closes the device (`dev_close`) -- so the VF is **down** when it arrives
- Points the device's netns pointer at the target netns
- Preserves all hardware configuration (MAC, VLAN, etc.)

**Important**: after the move, the netdev name stays the same, unless a device with the same name already exists in the target netns (in which case the kernel adds a suffix).

---

## 6. Step 4: Enter the netns and Finish Up (`pkg/ns/ns.go`)

### 6.1 Why the thread must be locked

`setns(2)` **only affects the one thread that calls it**. Go goroutines, however, are moved between OS threads by the scheduler. Without locking the thread, you would get:

> The first half of the code configures the NIC inside the Pod netns, and the second half suddenly runs in the host netns → `link not found`

So:

```go
func init() {
    runtime.LockOSThread()   // Pin the whole process to a single thread
}
```

### 6.2 Key points of `Do()`

```go
func (n *netNS) Do(toRun func(hostNS NetNS) error) error {
    // ① In the main goroutine, grab the "current netns" (i.e. the host's)
    hostNS, err := GetCurrentNS()

    // ② The real switch happens in a new goroutine, which calls LockOSThread first
    go func() {
        runtime.LockOSThread()
        innerError = containedCall(hostNS)
    }()
    wg.Wait()
}

func containedCall(hostNS NetNS) error {
    threadNS, _ := GetCurrentNS()          // Remember this thread's original netns
    netns.Set(n.handle)                    // Switch into the Pod

    defer func() {
        // ③ Only call UnlockOSThread if the switch back succeeded.
        //    If switching back fails, deliberately do not unlock -- let the Go runtime
        //    discard this "dirty thread" entirely so it is never reused by another goroutine.
        if err := netns.Set(threadNS); err == nil {
            runtime.UnlockOSThread()
        }
    }()

    return toRun(hostNS)   // Pass in the host handle, which makes moving the device back easy
}
```

### 6.3 Why renaming must happen while the device is down

The kernel's `dev_change_name()` returns `-EBUSY` for a device that is `IFF_UP`. Fortunately the device was already closed automatically when it crossed the netns boundary, so:

```go
if name != args.IfName {
    netlink.LinkSetDown(link)              // Down once more, to be safe
    netlink.LinkSetName(link, args.IfName)
}
netlink.LinkSetUp(link)                    // Bring it up after renaming
```

---

## 7. Step 5: IPAM (`pkg/ipam/ipam.go`)

### 7.1 The delegate model

sriov-cni does not allocate IPs itself. Instead it hands **the same config JSON** to an IPAM plugin to execute:

```go
r, err := invoke.DelegateAdd(context.TODO(), conf.IPAM.Type, args.StdinData, nil)
res, err := types100.GetResult(r)
```

The benefit: host-local / static / whereabouts / dhcp can be swapped freely without touching the plugin code.

### 7.2 The key flag when applying addresses

```go
addr := &netlink.Addr{
    IPNet: &ipc.Address,
    Flags: unix.IFA_F_NOPREFIXROUTE,
}
netlink.AddrAdd(link, addr)
```

`IFA_F_NOPREFIXROUTE` **stops the kernel from automatically adding the prefix route**.

- Without it: the kernel automatically inserts `192.168.100.0/24 dev net1 scope link` into the main table
- With it: the main table stays clean, and routing is controlled entirely by the IPAM `routes`

For multi-NIC Pods this is critical -- otherwise the main table ends up with two routes to the same subnet, and which one is used depends on the metric, so the behavior is unpredictable.

### 7.3 Policy routing (the proper solution for multiple NICs)

If IPAM returns routes carrying a `table`, the plugin automatically adds an `ip rule`:

```go
// Equivalent to: ip rule add from 192.168.100.10/32 table 100 pref 1100
rule := netlink.NewRule()
rule.Src      = &ipc.Address
rule.Table    = 100
rule.Priority = 1000 + 100
netlink.RuleAdd(rule)
```

This way, "traffic whose source address is net1 → look up table 100" does not interfere with eth0's default route.

---

## 8. Step 6: The Restore Logic in DEL

DEL is easier to get wrong than ADD. Three key points:

### 8.1 Read the cache first

The config passed to DEL at runtime often **does not contain deviceID** (it comes from the main CNI config and does not include the values injected by Multus). So read back the cache that was written to disk during ADD:

```go
if cached, e := config.LoadCachedConf(containerID, deviceID, ifName); e == nil {
    mergeCached(conf, cached)
}
```

### 8.2 The netns may already be gone

When a Pod is force-killed, the kernel automatically returns the netdevs in its netns to the init netns. So:

```go
if _, statErr := os.Stat(args.Netns); statErr == nil {
    // The netns still exists: enter it and move the device back
    ...
}
// Whether or not the above succeeded, re-locate the device on the host by PCI address
netdev, err := sriov.GetNetdevByPci(vfInfo.DeviceID)
```

### 8.3 Restore must cover all attributes

```go
LinkSetDown(vfLink)
LinkSetVfHardwareAddr(pfLink, vfID, origMAC)     // MAC
LinkSetVfVlanQos(pfLink, vfID, orig.Vlan, orig.VlanQoS)  // VLAN cleared back to 0
LinkSetVfSpoofchk(pfLink, vfID, orig.SpoofChk)    // Anti-spoofing
LinkSetVfTrust(pfLink, vfID, orig.Trust)
LinkSetVfState(pfLink, vfID, origState)
LinkSetVfRate(pfLink, vfID, orig.MinTxRate, orig.MaxTxRate)
LinkSetName(vfLink, orig.HostIFName)             // ★ The name must be restored too
if orig.LinkUp { LinkSetUp(vfLink) }
```

> **Why must it be restored?** A VF is a scarce hardware resource. If the VLAN stays at 1000 and spoofchk stays off after a Pod is deleted, the next Pod that gets this VF comes up with a "dirty config" -- showing up as random connectivity failures that are extremely hard to troubleshoot.

**Restore failures only warn, never error**: every failure in DEL is logged with `logging.Warnf`. A failing DEL makes kubelet retry repeatedly and leaves the Pod stuck in Terminating, which costs far more than leaving a bit of dirty config behind.

---

## 9. Build and Verify

```bash
cd /workspace/sriov-cni
export GOPROXY=https://goproxy.cn,direct
go build -o /tmp/sriov ./cmd/sriov
go test ./...
```

Smoke test (verified in this repo):

```bash
# Missing deviceID
$ echo '{"cniVersion":"1.0.0","name":"n","type":"sriov","ipam":{}}' \
  | CNI_COMMAND=ADD CNI_CONTAINERID=abc CNI_NETNS=/proc/self/ns/net \
    CNI_IFNAME=net1 CNI_PATH=/opt/cni/bin /tmp/sriov
{
    "code": 999,
    "msg": "sriov: missing \"deviceID\" (or \"pfName\"+\"vfID\"); it is normally injected by Multus from the SR-IOV Device Plugin"
}
```

---

## 10. Differences from Upstream `sriov-cni`

| Capability | Upstream | This implementation |
|---|---|---|
| Locate the VF | ✅ | ✅ |
| MAC / VLAN / QoS | ✅ | ✅ |
| spoofchk / trust / link-state | ✅ | ✅ |
| netns move + rename | ✅ | ✅ |
| IPAM delegate | ✅ | ✅ |
| Automatic policy routing | ❌ (needs an external initContainer) | ✅ |
| DPDK mode | ✅ (including driver binding) | ⚠️ Only skips the move; binding is left to the device plugin |
| Rate limiting | ❌ | ✅ |
| RDMA / IB | ❌ | ❌ |

This implementation is a **minimal skeleton for teaching and further development**. For production, use the upstream `sriov-cni` or `sriov-network-operator` directly.

---

**Previous**: [02 · Integrating with K8s CNI](./02-integrating-with-k8s-cni.md) · **Next**: [04 · Hands-on: The Second NIC in a Pod](./04-hands-on-second-nic.md)