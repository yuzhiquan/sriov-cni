# 05 · Troubleshooting and Performance Tuning

## 1. Overall Troubleshooting Flow

```
Pod stuck
  │
  ├─► kubectl describe pod  and check Events
  │     ├─ "no networks needed"          → annotation is wrong
  │     ├─ "Insufficient intel.com/..."  → resource pool exhausted / wrong name
  │     ├─ "failed to find plugin sriov" → binary not placed in /opt/cni/bin
  │     └─ "SR-IOV CNI failed to..."     → check the kubelet logs
  │
  ├─► kubectl logs -n kube-system <multus-pod>
  │
  └─► ssh to the node
        ├─ journalctl -u kubelet -f
        ├─ /var/log/multus.log / the logFile of sriov-cni
        └─ ip link show <pf>   check whether the VF state has been modified
```

---

## 2. Common Failure Reference Tables

### 2.1 Pod Stuck in ContainerCreating

| Message in Events | Cause | Fix |
|---|---|---|
| `failed to find plugin "sriov" in path [/opt/cni/bin]` | Binary is missing | Copy it to `/opt/cni/bin/sriov` and run `chmod +x` |
| `Insufficient intel.com/intel_sriov_netdevice` | Resource name mismatch / VFs exhausted | Verify the name in all three places: ConfigMap, NAD annotation, and Pod limits |
| `no networks needed` | The `k8s.v1.cni.cncf.io/networks` annotation is malformed | It must be a JSON array or the `default/netname` form |
| `error getting VF netdevice with name ""` | **`deviceID` is empty** | Check whether the thick version of Multus is installed and whether the PodResources socket is mounted |

### 2.2 Errors During CNI Execution

| Error | Cause | Fix |
|---|---|---|
| `sriov: VF 0000:03:02.1 not found` | The PCI address does not exist | Verify with `lspci`; confirm the VF has been created |
| `... is not a VF (no physfn link)` | A PF address was passed in | Check the selector of the device plugin |
| `no netdev for <bdf>` | **The VF is bound to vfio-pci** and has no netdev | To use a netdev, run `driverctl unset-override`; to use DPDK, configure `"dpdk": true` |
| `failed to set VF mac address: operation not supported` | spoofchk is not turned off / the driver does not support it | Run `spoofchk off` first; older i40e versions require a driver upgrade |
| `failed to set vlan: invalid argument` | VLAN is out of range, or vfID exceeds numvfs | vlan ∈ 1..4094; vfID < numvfs |
| `failed to move VF into netns: invalid argument` | The VF is already in another netns | Find it with `ip link show` and bring it back; confirm no other Pod is using it |
| `cannot find PF <name>` | `pfName` is wrong | Use the PF's **netdev name** (ens1f0), not the PCI address |

### 2.3 Pod Is Running but the Network Does Not Work

| Symptom | Troubleshooting |
|---|---|
| `net1` has an IP but cannot ping the gateway | ① Does the switch port trunk allow this VLAN?<br>② Is the VF's VLAN misconfigured?<br>③ Does port security / MAC limiting block the VF's MAC? |
| Can ping the gateway but cannot reach other subnets | Missing route; check `ip route` / the policy routing tables |
| Access to Service/DNS is intermittent | **Two default routes conflict** → see [Part 04 §4](./04-hands-on-second-nic.md) |
| Packets go out but replies never arrive | **rp_filter strict mode** drops packets → `sysctl net.ipv4.conf.all.rp_filter=2` |
| Other machines on the same subnet learn the wrong ARP entries | ARP flux → `arp_filter=1`, `arp_announce=2` |
| `tcpdump -i ens1f0` on the host cannot capture the Pod's packets | **Expected behavior**: the SR-IOV data plane bypasses the kernel, so packets cannot be captured.<br>Use NIC hardware statistics / switch port mirroring / ERSPAN instead |

---

## 3. Diagnostic Command Set

```bash
# ---------- Node side ----------
# VF summary
ip link show ens1f0

# Detailed attributes of a specific VF
ip -s link show ens1f0 | grep -A3 "vf 3"

# PCI topology
lspci -vvv -s 03:00.0 | grep -A20 "SR-IOV"
ls -l /sys/bus/pci/devices/0000:03:00.0/ | grep virtfn
ls -l /sys/bus/pci/devices/0000:03:02.1/ | grep physfn

# Driver binding status
driverctl list-devices | grep 03:02
# or
ls -l /sys/bus/pci/devices/0000:03:02.1/driver

# Hardware statistics (VF-level rx/tx packet counters)
ethtool -S ens1f0 | grep -i "vf3"

# Where the Pod's netns is
crictl inspectp <sandbox-id> | jq '.info.runtimeSpec.linux.namespaces[] | select(.type=="network")'

# Enter the Pod's netns and look around
nsenter -t <pid> -n -- ip addr

# ---------- Plugin cache ----------
ls -l /var/lib/cni/sriov/
cat /var/lib/cni/sriov/<containerID>-<deviceID>-<ifname>.json | jq

# ---------- Cluster side ----------
kubectl get sriovnetworknodestates -n openshift-sriov-network-operator
kubectl get pod test-sriov -o jsonpath='{.metadata.annotations}' | jq
```

### Enabling Plugin Debug Logging

Add the following to the NAD's config:

```json
{
  "type": "sriov",
  "logLevel": "debug",
  "logFile": "/var/log/sriov-cni.log",
  ...
}
```

Then:

```bash
tail -f /var/log/sriov-cni.log
journalctl -u kubelet -f | grep sriov
```

---

## 4. Performance Tuning

### 4.1 NUMA Alignment

Cross-NUMA memory access is expensive:

```bash
# Which NUMA node the NIC is on
cat /sys/class/net/ens1f0/device/numa_node

# CPUs of that node
lscpu | grep -A2 "NUMA node"
```

On the Pod side: enable Topology Manager + CPU Manager and use `Guaranteed` QoS to pin cores:

```yaml
# kubelet configuration
topologyManagerPolicy: single-numa-node
cpuManagerPolicy: static
```

```yaml
resources:
  requests:
    cpu: "4"
    memory: 8Gi
    intel.com/intel_sriov_netdevice: 1
  limits:
    cpu: "4"                                  # integer and equal → Guaranteed
    memory: 8Gi
    intel.com/intel_sriov_netdevice: 1
```

### 4.2 Interrupt Affinity

```bash
# Look up the VF's interrupt numbers
grep ens1f0v3 /proc/interrupts

# Bind to CPUs on the local NUMA node
echo <cpumask> > /proc/irq/<irq>/smp_affinity
```

In production, it is recommended to use `irqbalance` together with `IRQBALANCE_BANNED_CPUS` to keep interrupts off the cores used by the workload.

### 4.3 Ring Buffer

```bash
ethtool -G ens1f0 rx 4096 tx 4096
ethtool -g ens1f0      # check the maximum
```

### 4.4 MTU

If you want to run jumbo frames or VXLAN, the MTU must be consistent across the entire path:

```bash
ip link set ens1f0 mtu 9000        # PF
ip link set ens1f0 vf 0 mtu 9000   # or leave it to the CNI's "mtu" field
# The switch port must also allow 9000
```

### 4.5 Hugepages + DPDK (Maximum Performance)

```bash
# Node
echo 1024 > /sys/devices/system/node/node0/hugepages/hugepages-2048kB/nr_hugepages
modprobe vfio-pci
driverctl set-override 0000:03:02.3 vfio-pci
```

```yaml
# Pod
resources:
  limits:
    hugepages-2Mi: 1Gi
    intel.com/intel_sriov_dpdk: 1
```

The corresponding NAD must have `"dpdk": true`.

---

## 5. Production Best-Practice Checklist

**Nodes**
- [ ] BIOS: SR-IOV / VT-d / Above 4G Decoding / ACS
- [ ] Kernel: `intel_iommu=on iommu=pt`
- [ ] VF creation is persisted (systemd / NM keyfile / operator)
- [ ] NetworkManager does not manage VF netdevs (`unmanaged-devices`)
- [ ] Driver versions have been validated (iavf/ice/ice/i40e/mlx5_core)

**Cluster**
- [ ] Use the **thick** version of Multus (requires the PodResources API)
- [ ] The kubelet's `/var/lib/kubelet` is mounted into the multus pod
- [ ] The sriov-cni binary is present on all relevant nodes
- [ ] IPAM is cluster-aware (whereabouts / nv-ipam / spiderpool)
- [ ] The NAD annotation `resourceName` **exactly matches** the Pod `limits`

**Network**
- [ ] The switch port trunk allows the corresponding VLANs
- [ ] The port-security MAC count is ≥ the number of VFs
- [ ] The IP subnet is aligned with the physical network plan and does not conflict
- [ ] `rp_filter=2` / `arp_filter=1` have been rolled out

**Application**
- [ ] The NIC name is fixed (`interface: "net1"`)
- [ ] The default route strategy is settled (no gateway / policy routing)
- [ ] The application explicitly calls `bind()` to the second IP or uses `SO_BINDTODEVICE`

**Observability**
- [ ] You know tcpdump cannot capture the traffic → alternatives are prepared (hardware statistics, mirror ports)
- [ ] You know NetworkPolicy does not apply to SR-IOV NICs → do isolation on the physical network side

---

## 6. One-Sentence Summary

> **SR-IOV gives you "performance" at the cost of "software-defined flexibility".**
> In k8s, use the **Device Plugin/DRA for resource allocation**, **Multus for multi-NIC orchestration**, **sriov-cni for hardware configuration and moving the device into the netns**, and **IPAM for addressing**. Only when the four each do their own job can a Pod cleanly get a second NIC and a second IP.

---

**Previous**: [04 · Hands-On: A Second NIC](./04-hands-on-second-nic.md) · [Back to README](../README.md)