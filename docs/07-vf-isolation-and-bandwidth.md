# 07 · VF Isolation and Bandwidth Partitioning

# 07 · Do VFs Affect Each Other Under Heavy Traffic? Is the Bandwidth Partitioned?

## 1. The Short Answer

> **By default: no partitioning. All VFs compete for the bandwidth of the shared physical port.**
> **But: you can explicitly configure rate limits (mainly in the TX direction).**
> **RX direction: most NICs cannot enforce hard isolation.**

In one sentence:

> **Sum of all VF bandwidth ≤ min(physical port speed, PCIe link speed)**

---

## 2. Where the Sharing Happens

```
                     Physical port (100G)
                          │
                    ┌─────▼─────┐
                    │  MAC/PCS  │
                    └─────┬─────┘
                          │
                    ┌─────▼─────┐
                    │ HW packet  │  ← Hardware table lookup and steering (MAC/VLAN)
                    │ classifier │
                    └─────┬─────┘
          ┌───────┬───────┼───────┬───────┐
          ▼       ▼       ▼       ▼       ▼
        [VF0 Q] [VF1 Q] [VF2 Q] [VF3 Q] [PF Q]
          └───────┴───────┴───────┴───────┘
                        │
              ┌─────────▼─────────┐
              │  ⚠️ Shared: on-chip │  ← Contention point ①
              │  packet buffer      │
              └─────────┬─────────┘
                        │
              ┌─────────▼─────────┐
              │  ⚠️ Shared: PCIe    │  ← Contention point ② (hard limit)
              │  uplink             │
              └─────────┬─────────┘
                        │
              ┌─────────▼─────────┐
              │  ⚠️ Shared: memory  │  ← Contention point ③ (hidden bottleneck)
              │  bandwidth / LLC    │
              └─────────────────────┘
```

### Resource Ownership Table

| Layer | Independence | Description |
| --- | --- | --- |
| RX/TX queues / MSI-X vectors | ✅ **Independent** | Hardware resources owned exclusively by each VF |
| DMA address space | ✅ **Independent** | The IOMMU isolates translation per BDF |
| NIC on-chip packet buffer | ❌ **Shared** | Source of noisy neighbors under bursty traffic |
| PCIe uplink | ❌ **Shared (hard limit)** | Total bandwidth ceiling for all VFs |
| Memory bandwidth / LLC | ❌ **Shared** | Hidden bottleneck in small-packet, high-rate scenarios |

---

## 3. Concrete Interference Scenarios

### 3.1 Bandwidth Contention (the Most Direct Case)

```bash
# 100G NIC, 4 VFs
VF0 runs 40Gbps
VF1 runs 30Gbps
VF2 runs 20Gbps
VF3 runs 20Gbps
─────────────────
Total 110Gbps > 100Gbps  → everyone drops packets / slows down
```

**With no rate limit, whoever sends first gains the advantage** (this depends on the hardware scheduling policy: usually round-robin or priority-based).

### 3.2 Buffer Contention (the Hidden Killer)

The NIC's on-chip buffer is limited (a few MB to a few tens of MB).

```
VF0 bursts at 100Gbps (the application consumes slowly, so the queue backs up)
  → eats most of the shared buffer
  → VF1/2/3 start dropping packets even though they only run 5Gbps
```

This is **Head-of-Line Blocking** / the **noisy neighbor** problem.

**Typical symptom**: when one Pod sends packets aggressively, the P99 latency of other Pods suddenly spikes.

### 3.3 Interrupt / CPU Contention

```
VF0 queue interrupts → CPU 0-3
VF1 queue interrupts → CPU 0-3   ← Conflict!
VF2 queue interrupts → CPU 0-3
```

Lots of small packets → high interrupt rate → softirqs consume the CPU → the application doesn't get scheduled.

**Typical symptom**: `si` (softirq) utilization spikes, and the latency of all VFs rises together.

### 3.4 PCIe Transaction Overhead

Each VF queue has a doorbell register, which the application writes after sending packets to notify the NIC.

```
Many VFs × frequent doorbell writes = PCIe TLP overhead
```

This is especially noticeable in **multi-VF + small-packet** scenarios.

---

## 4. What Is the Actual Total Bandwidth?

It is **not an N-way equal split**. It is determined by the smaller of two bottlenecks:

| NIC | Physical port | PCIe link | Effective ceiling |
| --- | --- | --- | --- |
| Intel E810 | 100G | PCIe 4.0 x16 (≈200G) | **100G** (port-limited) |
| Intel X710 | 10G | PCIe 3.0 x8 (≈64G) | **10G** (port-limited) |
| Mellanox CX-6 | 100G | PCIe 4.0 x16 | **100G** |
| ⚠️ 100G NIC in a PCIe 3.0 x8 slot | 100G | ≈64G | **64G (PCIe-limited!)** |

> **Pitfall**: if you put a 100G NIC in a low-bandwidth slot, it can only reach 64G in practice, and **all VFs share that 64G**.

---

## 5. Available Isolation Mechanisms

### 5.1 TX Rate Limiting (Most Common, Best Supported)

```bash
# Limit VF 0 transmit rate to 25Gbps (unit: Mbps)
ip link set ens1f0 vf 0 max_tx_rate 25000

# Guaranteed minimum bandwidth (supported by some NICs)
ip link set ens1f0 vf 0 min_tx_rate 10000

# Inspect
ip link show ens1f0
```

These correspond to the `max_tx_rate` / `min_tx_rate` fields in this plugin's configuration.

### 5.2 DCB / ETS (Data Center Bridging)

If the NIC supports DCB:

```bash
# Configure priority → bandwidth mapping (2 priorities, 50% each)
dcbtool sc ens1f0 ets-up2tc 0:0 1:1
dcbtool sc ens1f0 ets-cfg 0:50 1:50
```

This requires cooperation from the switch side (DCBX negotiation).

### 5.3 Queue and Interrupt Isolation

```bash
# Specify the number of VFs when creating them
echo 4 > /sys/class/net/ens1f0/device/sriov_numvfs

# Pin interrupts to different CPUs
echo 0f > /proc/irq/<irq>/smp_affinity
```

### 5.4 Vendor Capability Comparison

| Vendor | TX rate limit | RX rate limit | Fine-grained QoS |
| --- | --- | --- | --- |
| **Intel (ice / i40e)** | ✅ | ❌ Basically unsupported | ⚠️ Limited DCB/ETS |
| **NVIDIA / Mellanox (mlx5)** | ✅ | ⚠️ Partially supported | ✅ Very good (`mlx_qos`) |
| **Broadcom (bnxt)** | ✅ | ⚠️ Partial | ✅ Fairly good |

---

## 6. Practical Recommendations

### 6.1 Configure Rate Limits Explicitly

```json
{
  "type": "sriov",
  "deviceID": "0000:03:02.1",
  "min_tx_rate": 5000,
  "max_tx_rate": 25000,
  "ipam": { "type": "whereabouts", "range": "192.168.100.0/24" }
}
```

### 6.2 Monitor Per-VF Statistics

```bash
# Per-VF packet / byte counters for RX and TX
ethtool -S ens1f0 | grep -E "vf[0-9]+_(rx|tx)_(packets|bytes)"

# or
ip -s link show ens1f0 | grep -A3 "vf 0"
```

### 6.3 NUMA and CPU Pinning

```bash
# Which NUMA node the NIC is on
cat /sys/class/net/ens1f0/device/numa_node

# Use Guaranteed QoS + CPU Manager to pin the Pod to CPUs on the same node
```

### 6.4 Architecture Recommendations for High-Traffic Scenarios

| Requirement | Solution |
| --- | --- |
| Strict bandwidth guarantees | TX rate limiting + switch-side QoS |
| Prevent bursts from affecting each other | NIC buffer management / flow control (PFC) |
| Maximum performance with isolation | **Consider multiple physical NICs** instead of multiple VFs |
| RDMA scenarios | **SR-IOV is required**, no alternative |

---

## 7. The One-Sentence Answer

> **VFs do affect each other. How much depends on where the bottleneck lies in the shared layers:**
>
> - **PCIe uplink bandwidth** → hard limit, shared by all VFs; once saturated, they all slow down together
> - **On-chip packet buffer** → noisy neighbor effect under bursty traffic, showing up as latency spikes
> - **CPU / memory bandwidth** → hidden bottleneck in small-packet, high-rate scenarios
>
> **The SR-IOV specification itself does not define bandwidth partitioning.** When you need it, rate-limit explicitly with `max_tx_rate`, or use DCB/ETS for priority scheduling.

---

**Previous**: [06 · Comparison with macvlan/ipvlan](./06-vs-macvlan-ipvlan.md) · [Back to README](../README.md)