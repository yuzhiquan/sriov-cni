# 02 · How SR-IOV Integrates with Kubernetes CNI

## 1. The Core Conflict: CNI Was Never Designed for This

Getting SR-IOV into k8s runs into three "design assumptions":

| What CNI assumes | The SR-IOV reality |
|---|---|
| A NIC is **created in software** (veth/tap) | A NIC is **carved out of hardware** (VF) and cannot be created |
| A Pod has only **one** NIC (eth0) | We want to give the Pod a **second** one (net1) |
| The plugin **decides on its own** which device to use | Which VF to use is decided by **resource allocation on the kubelet side**, and CNI is unaware of it |

So extra components are needed to solve two problems:

1. **"Which VF belongs to this Pod"** → handled by Kubernetes' **device allocation framework** (Device Plugin or DRA)
2. **"Multiple NICs per Pod"** → orchestrated by a **meta CNI plugin** (Multus)

---

## 2. Overall Architecture

```
                          ┌───────────────────────────────────────────────┐
                          │              Control Plane                    │
                          │  ┌─────────────┐      ┌──────────────────┐   │
                          │  │  Scheduler  │◄────│ Node allocatable: │   │
                          │  └─────────────┘     │ intel.com/..._sriov│   │
                          │                      │ _netdevice: 8      │   │
                          │                      └──────────────────┘   │
                          └───────────────────────┬───────────────────────┘
                                                  │ Scheduling decision
┌───────────────────────────────────────────────── ▼ ──────────────────────────┐
│                              Worker Node                                   │
│                                                                             │
│  ┌────────────────────────── kubelet ─────────────────────────────────┐    │
│  │  ① Discover resources (Device Plugin registration / ResourceSlice) │    │
│  │  ② Pod declares limits → call Allocate() → get PCI address         │    │
│  │  ③ Call CRI to create sandbox → CRI calls CNI ADD                  │    │
│  └───────────────────────────────┬─────────────────────────────────────┘    │
│                                  │                                        │
│    ┌─────────────────────────────▼──────────────────────────────────┐       │
│    │                    Multus CNI (primary CNI)                    │       │
│    │   Reads Pod annotation k8s.v1.cni.cncf.io/networks             │       │
│    │   Queries PodResources API for the allocated deviceID          │       │
│    │   Injects {"deviceID":"0000:03:02.1"}, then delegates one by one│      │
│    └──────┬───────────────────────────────┬─────────────────────────┘       │
│           │ delegate 1                    │ delegate 2                     │
│      ┌────▼─────┐                    ┌────▼──────┐                        │
│      │ Calico   │                    │ sriov-cni │ ← implemented in this guide│
│      │ (eth0)   │                    │  (net1)   │                          │
│      └────┬─────┘                    └────┬──────┘                        │
│           │                               ├─→ IPAM (whereabouts/host-local)│
│           │                               ├─→ Program VF hardware attributes (PF netlink)│
│           │                               └─→ Move the VF netdev into the Pod netns│
│      ┌────▼───────────────────────────────▼──────┐                        │
│      │                   Pod                     │                        │
│      │  eth0: 10.244.1.5   (overlay, Calico)    │                        │
│      │  net1: 192.168.100.10 (underlay, SR-IOV) │                        │
│      └──────────────────┬───────────────────────┘                        │
│                         │                                                  │
│  ┌──────────────────────▼────────────────────────────────────────────────┐  │
│  │                        Physical NIC                                  │  │
│  │   PF(0000:03:00.0)  ──  VF0(03:02.0) VF1(03:02.1) VF2 ...           │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────────────────┘
```

---

## 3. Four Component Chains: Each With Its Own Job

| Component | Responsibility | Project |
|---|---|---|
| **Resource discovery/allocation** | Discover VFs on the node, report them as schedulable resources, and assign a concrete PCI address after the Pod is scheduled | `sriov-network-device-plugin` (Device Plugin)<br>or `dra-driver-sriov` (DRA) |
| **Multi-NIC orchestration** | Acts as the primary CNI, reads Pod annotations, and invokes the CNI plugin of each "network" in turn | `multus-cni` |
| **VF NIC configuration** | Take the PCI address → find the netdev → set MAC/VLAN/… → move it into the netns | `sriov-cni` (implemented in this guide) |
| **IP allocation** | Assign the NIC an IP, gateway, and routes | `whereabouts` / `host-local` / `static` / `nv-ipam` |

**The responsibility boundaries matter**: sriov-cni does not care where the IP comes from, and IPAM does not care whether the NIC is hardware or software. This is why the CNI ecosystem can be combined in dozens of ways.

---

## 4. The Full Sequence: From kubectl apply to net1 Inside the Pod

```
 User                 kubelet            DevicePlugin         CRI            Multus        sriov-cni      IPAM
  │                     │                     │                │               │              │            │
  │ ① kubectl apply     │                     │                │               │              │            │
  ├────────────────────►│                     │                │               │              │            │
  │                     │                     │                │               │              │            │
  │       ② scheduler picks a node (looks at intel.com/xxx=8 in allocatable)     │              │            │
  │                     │                     │                │               │              │            │
  │                     │ ③ Allocate(1)       │                │               │              │            │
  │                     ├────────────────────►│                │               │              │            │
  │                     │◄────────────────────┤                │               │              │            │
  │                     │   {device: 0000:03:02.1,              │               │              │            │
  │                     │    envs: PCIDEVICE_...}              │               │              │            │
  │                     │                     │                │               │              │            │
  │                     │ ④ RunPodSandbox     │                │               │              │            │
  │                     ├─────────────────────────────────────►│               │              │            │
  │                     │                     │       ⑤ CNI ADD(CNI_ARGS=K8S_POD_NAME=..., │              │            │
  │                     │                     │                 CNI_NETNS=/proc/x/ns/net) │              │            │
  │                     │                     │                ├──────────────►│              │            │
  │                     │                     │                │   ⑥ Read annotation /networks │           │
  │                     │                     │                │   ⑦ PodResources API      │            │
  │                     │                     │                │      → 0000:03:02.1        │            │
  │                     │                     │                │   ⑧ delegate (inject deviceID)│         │
  │                     │                     │                │               ├───────────►│           │
  │                     │                     │                │               │   ⑨ delegate IPAM ADD
  │                     │                     │                │               │────────────────────────►
  │                     │                     │                │               │◄────────────────────────┤
  │                     │                     │                │               │   ⑩ ip link set PF vf N ...
  │                     │                     │                │               │   ⑪ LinkSetNsFd → Pod netns
  │                     │                     │                │               │   ⑫ rename net1 / up / configure IP
  │                     │                     │                │◄──────────────┤              │            │
  │                     │◄─────────────────────────────────────┤               │              │            │
  │◄────────────────────┤  Pod Running                          │               │              │            │
```

### The Key Step: How Does deviceID Get Passed into CNI?

CNI has only three inputs: the JSON on stdin, environment variables (CNI_COMMAND/CONTAINERID/NETNS/IFNAME/ARGS/PATH), and configuration files.

**Multus's approach (the current mainstream)**:

1. Put an annotation on the NetworkAttachmentDefinition to declare which extended resource this network uses:

   ```yaml
   annotations:
     k8s.v1.cni.cncf.io/resourceName: intel.com/intel_sriov_netdevice
   ```

2. The Pod's `resources.limits` **must** declare a resource of the same name. Otherwise the scheduler will not reserve it and kubelet will not allocate the device:

   ```yaml
   resources:
     limits:
       intel.com/intel_sriov_netdevice: 1
   ```

3. When Multus processes this network, it uses kubelet's **PodResources API** (`/var/lib/kubelet/pod-resources/kubelet.sock`) to query "which devices have already been allocated to this Pod", matches them by `resourceName`, and obtains one PCI address.

4. Multus puts this address into the `deviceID` field of the configuration JSON delegated to `sriov-cni`, then executes the plugin.

> **So a hand-written `deviceID` in the NAD is only a "fixed allocation for testing"; in production it is overridden by Multus.**
> A hard-coded value means two Pods may end up on the same VF.

**The new approach (DRA + CDI)**:

DRA became GA in k8s 1.35, and `dra-driver-sriov` (k8snetworkplumbingwg) and NVIDIA's DRA SR-IOV Driver appeared alongside it. In this model:

- Devices are reported through **ResourceSlice** (with attributes that can be filtered using CEL)
- Pods claim them through **ResourceClaim / ResourceClaimTemplate**
- The allocation result (including interface name, MAC, and IP) is written to the **ResourceClaim's `.status.devices`**
- Devices are injected into the container through **CDI (Container Device Interface)**

From CNI's perspective, the convention is that an allocated device must carry these two attributes, from which Multus passes `deviceID` to the CNI plugin:

```
k8s.cni.cncf.io/resourceName   # Must match the NAD annotation k8s.v1.cni.cncf.io/resourceName exactly
k8s.cni.cncf.io/deviceID       # The device identifier Multus passes to the CNI plugin
```

**In other words: whether you use Device Plugin or DRA, the sriov-cni layer needs almost no code changes -- it only looks at `deviceID`.** This is exactly why this plugin supports the top-level `deviceID`, `runtimeConfig.deviceID`, and the `PCIDEVICE_*` environment variables all at once.

---

## 5. Comparing the Three Technical Routes

| | Route A: Device Plugin + Multus (classic) | Route B: DRA + CDI (modern) | Route C: Multi-NIC built into a single CNI |
|---|---|---|---|
| Resource allocation | `sriov-network-device-plugin` | `dra-driver-sriov` | Built in |
| Multi-NIC orchestration | Multus | Multus | DANM / custom CNI |
| Maturity | **Most mature, most documentation** | GA in 1.35, ecosystem catching up | Scenario-specific |
| Resource granularity | Count (integer) | **Structured parameters + CEL filtering** | — |
| Device sharing | Not supported | **Supported** (multiple Pods share one device) | Depends on implementation |
| Recommended for | First choice for production, existing clusters | New clusters that need fine-grained scheduling | Telecom/NFV customization |

> ⚠️ Note: **Do not run the SR-IOV Device Plugin and the DRA Driver in the same cluster at the same time**. They conflict (NVIDIA's documentation states explicitly that the device plugin does not run once DRA is enabled).

---

## 6. Choosing an IPAM

| IPAM | Characteristics | Suitable for |
|---|---|---|
| `host-local` | Allocates independently per node, with configuration in a local file on the host | Single-node testing; for multiple nodes, the subnet must be split per node |
| `static` | Hard-codes the IP in the NAD | Fixed-IP scenarios (NFV, databases) |
| `whereabouts` | **Cluster-aware**, uses a CRD to guarantee no global conflicts | **Recommended for production** |
| `nv-ipam` | Ships with the NVIDIA Network Operator, supports per-node blocks | When using the NVIDIA Network Operator |
| `spiderpool` | Commonly used in China, IPAM + multi-NIC coordination | Bare metal, finance |

---

## 7. Network Planning and Design Notes

Giving a Pod a second NIC is essentially **attaching the Pod to the physical network (underlay)**, so you need to align with the network team:

```
  ┌──────────────────────────────────────────────────────────┐
  │                      Switch                             │
  │   Port Gi0/1:  switchport mode trunk                    │
  │                switchport trunk allowed vlan 1000       │
  │                (Port security / MAC limits must allow the VF count)│
  └───────────────────────┬──────────────────────────────────┘
                          │
                    Physical link
                          │
  ┌───────────────────────▼──────────────────────────────────┐
  │                    Server                                │
  │   PF ens1f0                                              │
  │     ├─ VF0 vlan 1000 → Pod A net1  192.168.100.10/24    │
  │     ├─ VF1 vlan 1000 → Pod B net1  192.168.100.11/24    │
  │     └─ VF2 vlan 1000 → Pod C net1  192.168.100.12/24    │
  └──────────────────────────────────────────────────────────┘
```

Key decisions:

| Question | Options | Impact |
|---|---|---|
| **Who tags the VLAN?** | NIC hardware (VF vlan) / a sub-interface created inside the Pod | Hardware tagging performs better and is transparent to the Pod |
| **Where does the IP come from?** | A separate subnet / the same subnet as the node | Affects gateway reachability and ARP behavior |
| **What about the default route?** | Don't add one / policy routing | See [Part 04](./04-hands-on-second-nic.md) |
| **NUMA alignment?** | NIC on the same node as the CPU | Affects cross-NUMA memory access latency |

---

## 8. Minimum Viable Checklist

What you need for a real deployment, in dependency order:

**① Node layer (one-time)**
```bash
# BIOS: SR-IOV / VT-d / Above 4G Decoding
# kernel: intel_iommu=on iommu=pt
echo 8 > /sys/class/net/ens1f0/device/sriov_numvfs   # and make it persistent
```

**② Cluster layer**

| Component | Installation |
|---|---|
| Multus | `kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/master/deployments/multus-daemonset-thick.yml` |
| sriov-cni | Put the compiled binary at `/opt/cni/bin/sriov` on every node (or let the device plugin / operator place it automatically) |
| sriov-device-plugin | The DaemonSet under the official `deployments/` plus the ConfigMap above |
| whereabouts | `kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/whereabouts/master/doc/crds/...` (follow the official documentation) |

**③ Workload layer**
- NetworkAttachmentDefinition ([deploy/02-nad-sriov-vlan1000.yaml](../deploy/02-nad-sriov-vlan1000.yaml))
- Pod annotation + resources.limits ([deploy/03-pod-dual-nic.yaml](../deploy/03-pod-dual-nic.yaml))

> For production, it is recommended to use **`sriov-network-operator`** directly. It automates ②③④ above (including VF creation, driver binding, device plugin configuration, and NAD generation).

---

**Previous**: [01 · SR-IOV Principles](./01-sriov-principles.md) · **Next**: [03 · Writing an SR-IOV CNI from Scratch](./03-writing-a-sriov-cni.md)