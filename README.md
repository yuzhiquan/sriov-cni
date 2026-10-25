# sriov-cni · From Principles to a Hand-Written CNI, to a Second NIC in Your Pod
**English** | [简体中文](./zh-CN/README.md)

A minimal SR-IOV CNI plugin that **compiles, runs, and ships with unit tests**, accompanied by a complete explanation of the underlying principles and a hands-on guide to deploying it on K8s.

---

## Table of Contents

| Document | Contents |
|---|---|
| **[01 · SR-IOV Principles](./docs/01-sriov-principles.md)** | PF/VF, PCIe configuration space, BAR, MSI-X, IOMMU isolation, packet path comparison, three modes, costs and selection |
| **[02 · Integrating with K8s CNI](./docs/02-integrating-with-k8s-cni.md)** | The three conflicts, the four-component architecture, full sequence, deviceID injection mechanism, Device Plugin vs DRA, IPAM selection |
| **[03 · Writing an SR-IOV CNI](./docs/03-writing-a-sriov-cni.md)** | Line-by-line code walkthrough: sysfs topology discovery, applying hardware attributes via netlink, moving VFs into the netns, IPAM delegate, restoring the VF on DEL |
| **[04 · Hands-on: A Second NIC](./docs/04-hands-on-second-nic.md)** | Node preparation → cluster components → NAD → Pod → verification, **including three solutions to the default route conflict** |
| **[05 · Troubleshooting and Tuning](./docs/05-troubleshooting-and-tuning.md)** | Failure lookup table, diagnostic command set, NUMA/interrupt/hugepage tuning, production checklist |
| **[06 · vs macvlan / ipvlan](./docs/06-vs-macvlan-ipvlan.md)** | SR-IOV vs macvlan vs ipvlan comparison |
| **[07 · VF Isolation and Bandwidth](./docs/07-vf-isolation-and-bandwidth.md)** | Isolation between VFs and bandwidth partitioning |

**Code**

```
cmd/sriov/main.go     ADD / DEL / CHECK main flow
pkg/config/           Config parsing, validation, state cache
pkg/sriov/            ★ VF topology discovery + hardware attribute setup + netns move
pkg/ipam/             IPAM delegate + address/route/policy-route setup
pkg/ns/               netns enter/exit wrapper (thread locking)
```

---

## 30-Second Answers

**Q: How does SR-IOV work?**
A physical NIC "splits itself" at the PCIe layer: the PF handles management, while each VF has its own PCI address/BAR/queues/interrupts and can be handed directly to a container's or VM's netns, so the data plane bypasses the host kernel and virtual switch. Isolation is guaranteed by the IOMMU, which remaps DMA by Requester ID (BDF).

**Q: How does it integrate with K8s CNI?**
Four components: **Device Plugin (or DRA)** decides "which VF belongs to this Pod" → **Multus** orchestrates "multiple NICs per Pod" → **sriov-cni** configures the VF and moves it into the netns → **IPAM** allocates the IP.

**Q: How does a Pod get a second NIC and a second IP?**
A NAD defines the second network, the Pod annotation `k8s.v1.cni.cncf.io/networks` references it, and `resources.limits` declares the VF resource. The key is to **handle the default route conflict** (policy routing is recommended).

---

## Build

```bash
cd sriov-cni-guide
./build.sh
# or
make all
```

Output: `build/sriov`

```bash
# Install on the node
sudo cp build/sriov /opt/cni/bin/sriov && sudo chmod +x /opt/cni/bin/sriov
```

### Dependencies

```bash
go version        # requires 1.21+
export GOPROXY=https://goproxy.cn,direct   # for mainland China
```

| Module | Version |
|---|---|
| `github.com/containernetworking/cni` | v1.2.3 |
| `github.com/vishvananda/netlink` | v1.2.1 |
| `github.com/vishvananda/netns` | v0.0.4 |
| `golang.org/x/sys` | v0.17.0 |

---

## Smoke Test (No Cluster Required)

```bash
# 1. Check whether the hardware is supported
sudo ./deploy/setup-vfs.sh --check

# 2. Create VFs and persist them
sudo ./deploy/setup-vfs.sh --pf ens1f0 --numvfs 8

# 3. Invoke the plugin manually to walk through ADD/DEL
sudo ./deploy/test-cni.sh
```

### What You Can Still Verify Without SR-IOV Hardware

```bash
go test ./...      # sysfs topology parsing logic (using a fake sysfs tree)

# Parameter validation and error messages
echo '{"cniVersion":"1.0.0","name":"n","type":"sriov","ipam":{}}' \
  | CNI_COMMAND=ADD CNI_CONTAINERID=abc CNI_NETNS=/proc/self/ns/net \
    CNI_IFNAME=net1 CNI_PATH=/opt/cni/bin ./build/sriov
# => "sriov: missing \"deviceID\" (or \"pfName\"+\"vfID\"); it is normally
#     injected by Multus from the SR-IOV Device Plugin"
```

---

## Configuration Field Reference

```json
{
  "cniVersion": "1.0.0",
  "name": "sriov-net",
  "type": "sriov",

  "deviceID": "0000:03:02.1",
  "vfID": 1,
  "pfName": "ens1f0",

  "mac": "02:11:22:33:44:55",
  "vlan": 1000,
  "vlanQoS": 0,
  "spoofchk": "off",
  "trust": "on",
  "link_state": "enable",
  "min_tx_rate": 0,
  "max_tx_rate": 0,
  "mtu": 1500,
  "dpdk": false,

  "logLevel": "info",
  "logFile": "/var/log/sriov-cni.log",

  "ipam": {
    "type": "whereabouts",
    "range": "192.168.100.0/24",
    "gateway": "",
    "routes": [
      { "dst": "192.168.100.0/24" },
      { "dst": "0.0.0.0/0", "gw": "192.168.100.1", "table": 100 }
    ]
  }
}
```

| Field | Default | Description |
|---|---|---|
| `deviceID` | — | PCI address of the VF; **injected by Multus in production** |
| `vfID` | Derived automatically | Index of the VF under its PF |
| `pfName` | Derived automatically | netdev name of the PF |
| `mac` | Keeps the original value | Requires `spoofchk: off` |
| `vlan` | Not set | 1..4094, tag applied by hardware |
| `vlanQoS` | 0 | 802.1p priority 0..7 |
| `spoofchk` | `on` | MAC spoofing protection |
| `trust` | `off` | Trusted VF |
| `link_state` | `auto` | `auto`/`enable`/`disable` |
| `min_tx_rate` / `max_tx_rate` | 0 | Rate limit in Mbps (hardware dependent) |
| `mtu` | Inherited | — |
| `dpdk` | `false` | When true, skips the netns move |
| `resetOnDel` | `true` | Whether to restore the VF on DEL |

The three sources of `deviceID` (by priority): top-level `deviceID` → `runtimeConfig.deviceID` → the `PCIDEVICE_*` environment variable.

---

## Deployment Manifests

| File | Description |
|---|---|
| [`deploy/setup-vfs.sh`](./deploy/setup-vfs.sh) | Node-side checks + create/persist VFs |
| [`deploy/01-device-plugin-cm.yaml`](./deploy/01-device-plugin-cm.yaml) | SR-IOV Device Plugin resource pool definition |
| [`deploy/02-nad-sriov-vlan1000.yaml`](./deploy/02-nad-sriov-vlan1000.yaml) | NetworkAttachmentDefinition (whereabouts / policy-routing variants) |
| [`deploy/03-pod-dual-nic.yaml`](./deploy/03-pod-dual-nic.yaml) | Dual-NIC Pod example |
| [`deploy/04-nad-sriov-static-ip.yaml`](./deploy/04-nad-sriov-static-ip.yaml) | NAD with a static IP |
| [`deploy/test-cni.sh`](./deploy/test-cni.sh) | ADD/DEL smoke test without a cluster |

---

## Scope

| Scenario | Recommendation |
|---|---|
| Learning and further development | ✅ This repository |
| Production deployment | Use upstream [`sriov-cni`](https://github.com/k8snetworkplumbingwg/sriov-cni) + [`multus-cni`](https://github.com/k8snetworkplumbingwg/multus-cni) |
| Fully automated operations | Use [`sriov-network-operator`](https://github.com/k8snetworkplumbingwg/sriov-network-operator) (includes VF creation, driver binding, NAD generation) |
| New clusters + fine-grained scheduling | Watch DRA (GA since k8s 1.35) + [`dra-driver-sriov`](https://github.com/k8snetworkplumbingwg/dra-driver-sriov) |

---

## Verification Status

```
$ gofmt -l .                     no output
$ go vet ./...                   clean
$ go build ./...                 OK  (statically linked ELF, CGO_ENABLED=0)
$ go test ./...
    ok  github.com/example/sriov-cni/pkg/sriov    # sysfs topology parsing (fake sysfs tree)
    ok  github.com/example/sriov-cni/pkg/ns       # netns enter/exit and restore (requires root)
$ CNI_COMMAND=VERSION ./build/sriov
    {"cniVersion":"1.1.0","supportedVersions":["0.1.0","0.2.0","0.3.0","0.3.1","0.4.0","1.0.0","1.1.0"]}
$ ./deploy/setup-vfs.sh --check       # correctly errors out and exits when no SR-IOV hardware is present
```

> A **real SR-IOV NIC** (Intel E810/X710, Mellanox ConnectX, etc.) is required to run the full end-to-end ADD/DEL;
> generic VMs / most cloud instances do not have this capability.