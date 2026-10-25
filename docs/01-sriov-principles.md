# 01 · How SR-IOV Works

## 1. The One-Sentence Summary

**SR-IOV (Single Root I/O Virtualization) lets one physical NIC "split itself" into multiple independent NICs at the PCIe level.** A full PF (Physical Function) handles management, while a number of lightweight VFs (Virtual Functions) each get their own PCI address, configuration space, queues, and interrupts. A VF can be **assigned directly to the network namespace of a container or virtual machine**, and the data path bypasses the host kernel network stack and virtual switch entirely.

> The keywords are **"directly"** and **"independent"**: this is not a NIC emulated in software (veth/tap). A small slice of real hardware capability is handed to a tenant for exclusive use.

---

## 2. Why We Need It: Where the Traditional Approach Gets Stuck

On a machine running containers, the traditional path looks like this:

```
                    ┌───────────────────── Host kernel ────────────────────┐
   ┌──────┐   DMA   │  ┌────────┐   ┌─────────┐   ┌───────┐   ┌──────────┐ │
   │ NIC  │ ──────► │  │  NIC   │──►│ vswitch │──►│  veth │──►│ Pod stack│ │
   └──────┘         │  │ driver │   │(OVS/br) │   │  pair │   │          │ │
      ▲             │  └────────┘   └─────────┘   └───────┘   └──────────┘ │
      │             │      ①           ②             ③            ④      │
      │             └────────────────────────────────────────────────────┘
      │                                                                   
   MSI-X interrupt                                                         
```

Every hop has a cost:

| Source of overhead | Description |
|---|---|
| **Memory copies** | A packet is copied between host and guest at least 1-2 times |
| **Context switches / system calls** | vhost-net and tap read/write calls cause user-space <-> kernel-space transitions |
| **vswitch processing** | OVS/bridge lookups, netfilter, and conntrack consume a lot of CPU |
| **Interrupts and softirqs** | Every packet triggers an interrupt -> softirq -> ksoftirqd, which preempts application cores |
| **Lock contention** | Multiple queues share queue locks and the global conntrack lock |

The result: **saturating a 10G NIC eats several cores, 40G/100G can hardly be saturated at all, and latency jitter is high**.

### After SR-IOV

```
   ┌──────────────────────────────────────────────────┐
   │                Physical NIC (NIC)                │
   │   ┌──────────────────────────────────────────┐   │
   │   │ Internal L2 classifier / switch fabric   │   │
   │   │ (MAC, VLAN filtering)                    │   │
   │   └───┬──────────┬──────────┬──────────┬─────┘   │
   │       │          │          │          │         │
   │    ┌──▼──┐    ┌──▼──┐    ┌──▼──┐    ┌──▼──┐      │
   │    │ VF0 │    │ VF1 │    │ VF2 │    │ VF3 │      │  ← Each has its own
   │    │queue│    │queue│    │queue│    │queue│      │    queues/MSI-X/BAR
   │    └──┬──┘    └──┬──┘    └──┬──┘    └──┬──┘      │
   └───────┼──────────┼──────────┼──────────┼───────────┘
           │          │          │          │
         DMA        DMA        DMA        DMA        ← Target memory is
           │          │          │          │           pre-mapped by the driver
      ┌────▼───┐ ┌───▼────┐ ┌───▼────┐ ┌───▼────┐
      │ Pod A  │ │ Pod B  │ │ VM C   │ │ DPDK  │
      │ netns  │ │ netns  │ │        │ │ proc  │
      └────────┘ └────────┘ └────────┘ └───────┘
           Host kernel network stack: not involved at all (data path)
```

The payoff: **near line-rate throughput, microsecond-level latency, and almost zero CPU usage** (interrupts can be replaced by polling mode).

---

## 3. PCIe Basics: 4 Concepts You Must Understand First

SR-IOV is an extension of the PCIe specification, not a networking technology. These 4 concepts are the foundation for understanding it.

### 3.1 BDF and Requester ID

Every PCIe function (Function) has a **BDF**:

```
   0000 : 03 : 02 . 1
   ──┬─   ─┬─  ─┬─  ─┬─
     │     │    │    └─ Function (0..7, or 0..255 with ARI enabled)
     │     │    └────── Device (0..31)
     │     └─────────── Bus (0..255)
     └───────────────── Domain / Segment (0..65535)
```

This BDF is the **Requester ID**, which the NIC attaches to the TLP whenever it initiates a DMA. **The IOMMU uses it to look up page tables and decide whether this DMA may access the target memory.** This is the hardware foundation of SR-IOV isolation.

### 3.2 Configuration Space

Each Function has a configuration space containing the Vendor ID, Device ID, Class Code, BARs, and so on. SR-IOV adds an **Extended Capability (ID = 0x0010)** to it.

### 3.3 BAR (Base Address Register)

A BAR describes "where in the host physical address space this device's registers/memory window is mapped." The driver operates the NIC by reading and writing the BAR through MMIO.

**Key point: a VF's BARs are carved out of the PF's SR-IOV BAR aperture.** When the PF driver creates VFs, the firmware/driver allocates an independent window for each VF. So every VF has its own register view and can send and receive packets independently.

### 3.4 MSI-X

A multi-vector interrupt table. Every VF has its own MSI-X table entries, so **each VF's queues can raise interrupts independently and route them to different CPUs**. This is the prerequisite for multi-queue + RSS to deliver performance.

---

## 4. The SR-IOV Specification: PF and VF

### 4.1 The Two Kinds of Function

| | PF (Physical Function) | VF (Virtual Function) |
|---|---|---|
| Capabilities | Full functionality, including the management plane | Only the minimal resources the data plane needs |
| Count | At least 1 per device | Determined by hardware; the spec limit is 256 |
| Configuration space | Full + SR-IOV Capability | Slimmed down; VF BARs come from the PF |
| Driver | ixgbe/i40e/ice/mlx5_core/... | ixgbevf/iavf/mlx5_core (same) |
| Can manage VFs | Yes (`ip link set <pf> vf N ...`) | No |
| Can be passed through | Can be used as PF passthrough | Can be passed through to a container/VM |

### 4.2 Structure of the SR-IOV Extended Capability

```
PCIe Config Space (Extended Capability, ID=0x0010)
├── SR-IOV Capabilities Register   Supported feature bits
├── SR-IOV Control / Status        VF Enable / VF Migration Enable ...
├── InitialVFs                     Initial number of VFs supported by hardware
├── TotalVFs                       Upper limit of total VFs supported by hardware
├── NumVFs                         ★ Number of currently enabled VFs (write it to enable VFs)
├── Function Dependency Link       PF number the VFs depend on
├── First VF Offset                Routing ID offset of the first VF
├── VF Stride                      Routing ID stride between adjacent VFs
├── VF Device ID                   Device ID exposed by the VFs
├── Supported Page Sizes
├── System Page Size
├── VF BAR0 .. VF BAR5             ★ VF BAR windows (carved from the PF aperture)
└── VF Migration State Array Offset
```

### 4.3 How to "Enable" VFs

Linux exports this as sysfs files; just write a number:

```bash
# Check the maximum number of VFs this PF can enable
cat /sys/class/net/ens1f0/device/sriov_totalvfs
# 63

# Enable 8 VFs
echo 8 > /sys/class/net/ens1f0/device/sriov_numvfs

# Now they are visible
lspci | grep -i "virtual function"
ip link show ens1f0
# 5: ens1f0: <BROADCAST,MULTICAST,UP,LOWER_UP> ...
#     vf 0     link/auto,  spoof checking on, link-state auto, trust off
#     vf 1     link/auto,  spoof checking on, link-state auto, trust off
```

**In essence, this step makes the PF driver register these VFs' Requester IDs with the IOMMU, and allocate BAR windows, queues, and MSI-X vectors for each VF.**

### 4.4 What a VF Looks Like in the Kernel

Once VFs are enabled, each VF is **a separate PCI device** with its own BDF. If a kernel driver is bound, it has a netdev:

```bash
$ ls /sys/bus/pci/devices/
0000:03:00.0      # PF
0000:03:02.0      # VF 0
0000:03:02.1      # VF 1

# The topology is expressed through symlinks
$ readlink /sys/bus/pci/devices/0000:03:02.1/physfn
../0000:03:00.0

$ readlink /sys/bus/pci/devices/0000:03:00.0/virtfn1
../0000:03:02.1

# netdev name
$ ls /sys/bus/pci/devices/0000:03:02.1/net/
ens1f0v1
```

> These three sysfs paths are everything the `GetVFInfo()` implementation in the CNI later relies on.

---

## 5. How Isolation Works: IOMMU / VT-d

Many people think "passthrough" just means handing a device fd to a process. Real isolation is guaranteed by the **IOMMU**.

### 5.1 DMA Remapping

Without an IOMMU, a device issues DMA using **host physical addresses** and can access **any memory**. A passed-through device then means a malicious device can read all of memory.

With an IOMMU (Intel VT-d / AMD-Vi / ARM SMMU):

```
Device (Requester ID = 0000:03:02.1)
   │  Issues DMA carrying address GPA/IOVA + BDF
   ▼
┌─────────────────────────────────────────────┐
│                   IOMMU                     │
│  1. Look up the Root Table by BDF           │
│  2. Look up the Context Table -> get the    │
│     page table base address                 │
│  3. Translate IOVA -> HPA using the         │
│     second-level page table                 │
│  4. Check the page's read/write permission  │
│     -> reject if out of bounds              │
└─────────────────────────────────────────────┘
   │
   ▼
Only memory assigned to this VF can be accessed
```

### 5.2 Enabling It

```bash
# /etc/default/grub
GRUB_CMDLINE_LINUX_DEFAULT="intel_iommu=on iommu=pt"
# AMD: amd_iommu=on iommu=pt
update-grub && reboot

# Verify
dmesg | grep -i -E "DMAR|IOMMU"
# DMAR: Intel-IOMMU enabled
find /sys/kernel/iommu_groups/ -maxdepth 1 -type d | wc -l
```

On the BIOS side, enable: `VT-d` (Intel) / `IOMMU` (AMD), `SR-IOV`, and `Above 4G Decoding` (required for large BARs).

### 5.3 IOMMU Group

**The IOMMU group is the smallest unit of isolation**, not an individual device. Devices within the same group cannot be isolated from one another (commonly seen with multi-Function devices, or when a PCIe switch/bridge does not support ACS). VFIO requires the entire group to be handed to vfio-pci, otherwise it refuses passthrough.

```bash
# See which group a VF belongs to and which devices are in that group
readlink /sys/bus/pci/devices/0000:03:02.1/iommu_group
ls /sys/kernel/iommu_groups/$(readlink /sys/bus/pci/devices/0000:03:02.1/iommu_group | xargs basename)/devices/
```

### 5.4 ⚠️ A Common Misconception: Containers Using SR-IOV Don't Necessarily Need an IOMMU

This is very easy to mix up:

| Mode | Needs vfio-pci? | Needs IOMMU? | Reason |
|---|---|---|---|
| **netdevice mode** (VF bound to a kernel driver, netdev moved into the Pod netns) | No | **No** | The device is always controlled by the host kernel driver. The Pod only gets the netdev's sockets/queues and has no DMA permission |
| **DPDK / vfio mode** (VF bound to vfio-pci, user-space PMD) | Yes | **Yes** (otherwise you must enable `enable_unsafe_noiommu_mode`, which is unsafe) | A user-space process directly programs the NIC registers and issues DMA |

**Conclusion: the most common Kubernetes usage, "SR-IOV CNI + a regular netdev", does not require configuring an IOMMU.** The IOMMU is only required for DPDK/vfio or VM passthrough scenarios.

---

## 6. VF Hardware Capabilities and the Attributes That "Must Be Set by the PF"

This is the biggest experiential difference between SR-IOV and software NICs: **many VF attributes do not live on the VF's own netdev but on the PF, and must be written into the NIC hardware through the PF driver.**

The corresponding commands:

```bash
ip link set <PF> vf <N> mac   <MAC>
ip link set <PF> vf <N> vlan  <VLAN> [qos <0-7>]
ip link set <PF> vf <N> spoofchk on|off
ip link set <PF> vf <N> trust on|off
ip link set <PF> vf <N> state auto|enable|disable
ip link set <PF> vf <N> max_tx_rate <Mbps>
ip link set <PF> vf <N> min_tx_rate <Mbps>
```

### 6.1 VLAN

- The NIC hardware performs **802.1Q tag insertion (TX) and stripping (RX)** for the VF
- Inside the Pod you see **untagged traffic**, so there is no need to create a VLAN sub-interface on `net1`
- The switch port must be **trunked to allow that VLAN**
- A VF can be configured with **only one VLAN** (some newer hardware supports QinQ)

### 6.2 MAC and Spoof Checking

- A VF is assigned a MAC by the PF by default, and `spoofchk` defaults to **on**
- **To change the MAC, you must first set `spoofchk off`**, otherwise the hardware drops frames whose source MAC does not match
- `spoofchk off` carries a security risk: a tenant can forge MAC/IP addresses. In production, it is recommended to keep it on, and have the CNI set the MAC before turning it on

> This is also why `ConfigureVFHardware()` in this plugin follows the order "down first -> then spoofchk -> then MAC".

### 6.3 trust

`trust on` allows the VF to enable promiscuous mode, join multicast groups, and change its MAC on its own. It is usually required when running DPDK, capturing packets, or running OVS-DPDK.

### 6.4 Rate Limiting and Statistics

Rate limiting (min/max tx rate) and per-VF packet statistics are provided by the hardware:

```bash
ip -s link show ens1f0 | grep -A2 "vf 3"
ethtool -S ens1f0 | grep vf3
```

---

## 7. The Three Operating Modes

### 7.1 netdevice Mode (Kernel Driver): the Most Common for Kubernetes Containers

```
VF --bind--> iavf/mlx5_core driver --> produces a netdev --> moved into Pod netns --> has an IP, routable
```

- The Pod sees an ordinary NIC and can use standard sockets and the `ip` command
- Traffic goes through the kernel network stack (so conntrack/iptables apply, but **not** the host's instance of them)
- Good performance, though not as extreme as DPDK

### 7.2 vfio / DPDK Mode

```
VF --bind--> vfio-pci --> /dev/vfio/<group> mounted into the container --> DPDK PMD polls for send/receive in user space
```

- There is no netdev; `ip link` inside the Pod **cannot see** the NIC
- Requires hugepages, CPU pinning, and the same NUMA node
- Extreme performance (millions of PPS, microsecond latency)
- This plugin supports it with `"dpdk": true`; in that case it skips moving the device into the netns and only returns the PCI address

### 7.3 switchdev Mode (representor): Mellanox/NVIDIA

```
      VF ────────────────┐
                         │  Inside the NIC
      representor netdev ┘
                         │
                    TC flower / OVS offload
```

- Each VF has a **representor netdev** on the host
- The control plane (OVS/TC) can **offload flow tables to the NIC hardware**, while the data path is still passed through directly
- It gives you both "SDN programmability" and "hardware performance"
- It is how many cloud providers implement bare-metal SDN

---

## 8. The Costs and Limitations of SR-IOV

| Limitation | Description |
|---|---|
| **Poor mobility** | A passed-through device is tied to specific hardware; live migration is basically unavailable (requires hardware/vendor support for migration capabilities) |
| **Limited number of VFs** | Typically 8-256, constrained by PCIe bandwidth, BAR space, and NUMA |
| **Reduced observability** | Traffic bypasses the kernel -> host `tcpdump`, iptables counters, and conntrack all stop working |
| **No host SDN policy support** | NetworkPolicy and service mesh iptables rules do not apply to SR-IOV NICs |
| **Strong hardware/driver dependence** | Features differ greatly (i40e vs ice vs mlx5); be careful when upgrading drivers |
| **Larger attack surface** | DMA attack surface; tenants can forge addresses after `spoofchk off` |
| **Complex IPAM** | Requires coordination with the physical network/VLANs, and IP resources must be planned centrally |

---

## 9. Comparison with "Software Multi-NIC" Approaches

| Approach | Layer | Performance | Hardware dependence | Mobility | Typical use |
|---|---|---|---|---|---|
| **veth + bridge** | Pure software | Medium | None | Good | Default CNIs (Calico/Flannel) |
| **macvlan / ipvlan** | Software multiplexing of the PF | Fairly high | None | Good | Pods that need direct L2 connectivity |
| **SR-IOV VF** | Hardware passthrough | **Highest** | **Requires an SR-IOV NIC** | Poor | NFV, high-frequency trading, AI/RDMA |
| **AF_XDP** | Kernel bypass (software) | High | Driver must support XDP | Good | Medium-to-high performance with flexibility needs |
| **vhost-user / virtio** | Paravirtualization | Medium-high | None | Good | General-purpose VM solution |
| **switchdev + VF** | Hardware passthrough + offload | Highest | mlx5, etc. | Poor | Cloud provider SDN |

**Rule of thumb for choosing**: for extreme performance and you can accept being tied to hardware -> SR-IOV; for flexible migration -> macvlan or AF_XDP; for SDN policy + high performance -> switchdev.

---

## 10. Quick Command Reference

```bash
# Does the hardware support it?
lspci -vvv -s 03:00.0 | grep -A20 "Single Root I/O Virtualization"

# Enable VFs
echo 8 > /sys/class/net/ens1f0/device/sriov_numvfs

# Check VF status
ip link show ens1f0

# Configure a VF
ip link set ens1f0 vf 3 mac 02:11:22:33:44:55 vlan 1000 spoofchk off trust on

# Move a VF into a netns
ip link set ens1f0v3 netns /var/run/netns/pod1

# Driver binding (DPDK mode)
modprobe vfio-pci
driverctl set-override 0000:03:02.3 vfio-pci
```

---

**Next**: [02 · Integrating with Kubernetes CNI](./02-integrating-with-k8s-cni.md)