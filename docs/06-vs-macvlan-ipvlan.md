# 06 · SR-IOV vs macvlan vs ipvlan

## 1. The Core Difference in One Sentence

> **macvlan / ipvlan make "one NIC act as many" — they are virtualized in software, and packets still pass through the host kernel.**
> **SR-IOV makes "one NIC genuinely split into many" — the split is done by PCIe hardware, and packets bypass the host kernel.**

---

## 2. Data Path Comparison

```
   ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐
   │  VF0   │ │  VF1   │ │  VF2   │ │  VF3   │      What each VF owns independently:
   │ Queues │ │ Queues │ │ Queues │ │ Queues │      · Its own PCIe BDF
   │ MSI-X  │ │ MSI-X  │ │ MSI-X  │ │ MSI-X  │      · Its own RX/TX queues / MSI-X
   └───┬────┘ └───┬────┘ └───┬────┘ └───┬────┘      · Its own DMA address space
       │        │        │        │                    · IOMMU hardware isolation
       └────────┴────┬───┴────────┘
                     ▼
        ┌────────────────────────────┐
        │  ⚠️ Shared: NIC on-chip        │  ← Contention point ①
        │     packet buffer (all VFs     │
        │     use the same one)          │
        └─────────────┬──────────────┘
                      ▼
        ┌────────────────────────────┐
        │  ⚠️ Shared: PCIe uplink        │  ← Contention point ② (hard limit)
        │  e.g. PCIe 4.0 x16 ≈ 200Gbps   │
        └─────────────┬──────────────┘
                      ▼
        ┌────────────────────────────┐
        │  ⚠️ Shared: memory bandwidth   │  ← Contention point ③ (hidden bottleneck)
        │  / LLC                         │
        └─────────────┬──────────────┘
                      ▼
              Host / application memory
```

### Resource Ownership at Three Levels

| Level | Independent? | Notes |
| --- | --- | --- |
| **RX/TX queues / MSI-X vectors** | ✅ Independent | Each VF has its own queues and interrupts |
| **DMA address space** | ✅ Independent | The IOMMU performs isolated translation for each VF |
| **NIC on-chip packet buffer** | ❌ **Shared** | Used by all VFs |
| **PCIe uplink bandwidth** | ❌ **Shared (hard limit)** | The cap on the sum of all VF traffic |
| **Memory bandwidth / LLC** | ❌ **Shared** | The hidden bottleneck in small-packet, high-rate scenarios |

---

## 3. Overall Comparison Table

| Dimension | **SR-IOV VF** | **macvlan** | **ipvlan** |
| --- | --- | --- | --- |
| **Where it is implemented** | **NIC hardware** (PCIe layer) | Kernel software driver | Kernel software driver |
| **Is it a real device?** | Yes (an independent PCIe Function with BDF/BAR/MSI-X) | No (virtual interface) | No (virtual interface) |
| **Does the data path go through the kernel?** | ❌ **Bypassed** | ✅ Always | ✅ Always |
| **Isolation mechanism** | IOMMU DMA remapping (hardware-level) | Software isolation | Software isolation |
| **MAC address** | Independent MAC per VF | **Independent MAC per sub-interface** | **Shares the parent interface's MAC** |
| **Needs promiscuous mode?** | No (hardware classification) | **Usually yes** | Needed in L2 / **not needed in L3** |
| **Hardware dependency** | ⚠️ Must support SR-IOV | None | None |
| **Maximum count** | 8\~256 (hardware limit) | Practically unlimited | Practically unlimited |
| **Live migration** | ❌ Poor (bound to hardware) | ✅ Good | ✅ Good |
| **Packet capture on the host** | ❌ **Cannot capture** | ✅ Can capture | ✅ Can capture |
| **DPDK / RDMA** | ✅ Supported | ❌ Not supported | ❌ Not supported |
| **Relative throughput** | ★★★★★ | ★★★☆ | ★★★★ |
| **Relative latency** | Lowest | Medium | Medium-low |

---

## 4. Three Key Differences in Detail

### 4.1 MAC Address Handling — A Make-or-Break Issue in Cloud Environments

This is the **most practical** difference among the three, and the biggest reason ipvlan exists.

```
macvlan:
  Parent interface ens1f0   MAC: aa:aa:aa:aa:aa:aa
    ├─ macvlan0   MAC: 02:11:22:33:44:01   ← independent MAC
    ├─ macvlan1   MAC: 02:11:22:33:44:02   ← independent MAC
    └─ macvlan2   MAC: 02:11:22:33:44:03   ← independent MAC

  The switch port has to learn 4 MACs
    ↓
  ⚠️ Public cloud / virtual switch security policies (anti MAC hijacking) will drop the packets outright!


ipvlan L2 mode:
  Parent interface ens1f0   MAC: aa:aa:aa:aa:aa:aa  ← shared by all sub-interfaces
    ├─ ipvlan0   MAC: aa:aa:aa:aa:aa:aa
    ├─ ipvlan1   MAC: aa:aa:aa:aa:aa:aa
    └─ ipvlan2   MAC: aa:aa:aa:aa:aa

  The switch sees only 1 MAC → ✅ cloud-friendly
```

**Conclusion**:

| Environment | Choice |
| --- | --- |
| Bare metal / your own network | macvlan is fine |
| **Public cloud / VMware / virtual switches** | **Must use ipvlan** (macvlan will most likely not work) |

### 4.2 Where the Data Path Overhead Lies

**Overhead of macvlan / ipvlan**:

```
Receive path:
  NIC DMA → host memory (buffer allocated by the host driver)
    → kernel looks up a table in software to decide which sub-interface gets it
    → traverses the host protocol stack
    → copied into application memory

  ⚠️ One extra software forwarding step + one extra copy

SR-IOV:
  NIC DMA → application memory (the queues are mmap'd by the application)
    → the application sees the packet directly

  ✅ Zero-copy, no software forwarding
```

**Conclusion**:

- **Large-packet throughput**: little difference among the three (all can saturate line rate)
- **Small-packet PPS / latency**: SR-IOV has a clear advantage (software forwarding is the bottleneck)

### 4.3 Communicating with the Parent Interface (A Classic Pitfall)

A **limitation shared by all three**: sub-interfaces and the parent interface **cannot communicate directly by default**.

The reason is that traffic is designed to go "straight out of the NIC" without passing through the host protocol stack, so the host itself never receives it.

```
Pod (macvlan0) ──✗──> host ens1f0
```

**Solution**: create another macvlan/ipvlan interface for the host to use, and add routes.

```bash
# host side
ip link add host-mv link ens1f0 type macvlan mode bridge
ip addr add 192.168.1.2/24 dev host-mv
ip link set host-mv up
```

---

## 5. macvlan vs ipvlan Head to Head

|   | **macvlan** | **ipvlan** |
| --- | --- | --- |
| **MAC** | One MAC per sub-interface | Shares the parent interface's single MAC |
| **Traffic is distinguished by** | Destination MAC | IP address |
| **Pressure on the switch MAC table** | High (N MACs) | Low (1 MAC) |
| **Promiscuous mode** | Usually required | Required in L2, **not required in L3** |
| **DHCP** | Works normally (one per MAC) | ⚠️ Problematic in L2 mode (multiple IPs on the same MAC; DHCP client ID conflicts) |
| **Wireless NICs** | ❌ Basically unusable | ✅ Usable in L3 mode |
| **Bridged / virtualized environments** | ⚠️ Often blocked | ✅ Friendly |

### The Two ipvlan Modes

- **L2 mode**: sub-interfaces are in the same broadcast domain and behave much like macvlan, but share a MAC
- **L3 mode**: purely layer 3; traffic between sub-interfaces is routed, **no broadcast/ARP is needed**, and performance is better

> A simple way to remember it: **macvlan = multiple MACs; ipvlan = one MAC with multiple IPs**

---

## 6. Selection Decision Tree

```
Need RDMA / DPDK / extreme performance?
  ├─ Yes → SR-IOV (the only choice)
  └─ No ↓

Running on a public cloud / virtual switch?
  ├─ Yes → ipvlan (prefer L3 mode)
  └─ No ↓

Need live migration / elastic scaling?
  ├─ Yes → macvlan / ipvlan
  └─ No ↓

Have a NIC that supports SR-IOV, and running on bare metal?
  ├─ Yes → SR-IOV
  └─ No → macvlan / ipvlan
```

---

## 7. Recommendations for Typical Scenarios

| Scenario | First choice | Reason |
| --- | --- | --- |
| **Telecom NFV / 5G UPF** | SR-IOV + DPDK | Needs line-rate forwarding; binding to hardware is acceptable |
| **AI training / RDMA** | **SR-IOV (required)** | RDMA needs hardware queues |
| **High-frequency trading** | SR-IOV | Microsecond-level latency requirements |
| **Bare-metal K8s, Pods directly on the physical network** | macvlan | No cloud restrictions, simple configuration |
| **Public cloud containers** | ipvlan L3 | MAC restrictions + good performance |
| **Need NetworkPolicy** | **None of them work** | Go back to Calico / Cilium |

---

## 8. An Easily Overlooked Comparison: Observability

This is SR-IOV's biggest "hidden cost":

| Operation | macvlan / ipvlan | SR-IOV |
| --- | --- | --- |
| `tcpdump -i ens1f0` to capture Pod traffic | ✅ Can capture | ❌ **Cannot capture** (kernel bypassed) |
| `conntrack -L` | ✅ Available | ❌ Not available |
| iptables counters | ✅ Available | ❌ Not available |
| `ethtool -S` hardware statistics | ✅ | ✅ |
| Switch port mirroring | ✅ | ✅ |

**Troubleshooting tip**: with SR-IOV you can only rely on hardware statistics and mirror ports, which is far more troublesome than macvlan. If your team does not yet have the supporting observability tooling, macvlan / ipvlan is often the safer choice.

---

**Previous**: [05 · Troubleshooting and Tuning](./05-troubleshooting-and-tuning.md) · [Back to README](../README.md)