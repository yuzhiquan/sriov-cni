# 02 · SR-IOV 如何与 Kubernetes CNI 结合

## 1. 核心矛盾：CNI 天生不支持这件事

SR-IOV 想塞进 k8s，会撞上三个"设计假设"：

| CNI 的假设 | SR-IOV 的现实 |
|---|---|
| 网卡是**软件造出来**的（veth/tap） | 网卡是**硬件分出来**的（VF），不能造 |
| 一个 Pod 只有**一张**网卡（eth0） | 我们要给 Pod 加**第二张**（net1） |
| 插件**自己决定**用哪个设备 | 用哪个 VF 由 **kubelet 侧的资源分配**决定，CNI 不知情 |

所以必须引入额外组件来解决两件事：

1. **"哪块 VF 归这个 Pod"** → 由 Kubernetes 的**设备分配框架**负责（Device Plugin 或 DRA）
2. **"一个 Pod 多张网卡"** → 由一个 **meta CNI 插件**（Multus）负责编排

---

## 2. 全景架构

```
                          ┌───────────────────────────────────────────────┐
                          │              Control Plane                    │
                          │  ┌─────────────┐      ┌──────────────────┐   │
                          │  │  Scheduler  │◄────│ Node allocatable: │   │
                          │  └─────────────┘     │ intel.com/..._sriov│   │
                          │                      │ _netdevice: 8      │   │
                          │                      └──────────────────┘   │
                          └───────────────────────┬───────────────────────┘
                                                  │ 调度决策
┌───────────────────────────────────────────────── ▼ ──────────────────────────┐
│                              Worker Node                                   │
│                                                                             │
│  ┌────────────────────────── kubelet ─────────────────────────────────┐    │
│  │  ① 发现资源（Device Plugin 注册 / ResourceSlice）                  │    │
│  │  ② Pod 声明 limits → 调 Allocate() → 得到 PCI 地址                 │    │
│  │  ③ 调 CRI 创建 sandbox → CRI 调 CNI ADD                            │    │
│  └───────────────────────────────┬─────────────────────────────────────┘    │
│                                  │                                        │
│    ┌─────────────────────────────▼──────────────────────────────────┐       │
│    │                    Multus CNI（主 CNI）                       │       │
│    │   读 Pod 注解 k8s.v1.cni.cncf.io/networks                     │       │
│    │   查 PodResources API 得到已分配的 deviceID                    │       │
│    │   注入 {"deviceID":"0000:03:02.1"} 后逐个 delegate             │       │
│    └──────┬───────────────────────────────┬─────────────────────────┘       │
│           │ delegate 1                    │ delegate 2                     │
│      ┌────▼─────┐                    ┌────▼──────┐                        │
│      │ Calico   │                    │ sriov-cni │ ← 本文实现              │
│      │ (eth0)   │                    │  (net1)   │                          │
│      └────┬─────┘                    └────┬──────┘                        │
│           │                               ├─→ IPAM (whereabouts/host-local)│
│           │                               ├─→ 刷 VF 硬件属性（PF netlink） │
│           │                               └─→ 把 VF netdev 搬进 Pod netns  │
│      ┌────▼───────────────────────────────▼──────┐                        │
│      │                   Pod                     │                        │
│      │  eth0: 10.244.1.5   (overlay, Calico)    │                        │
│      │  net1: 192.168.100.10 (underlay, SR-IOV) │                        │
│      └──────────────────┬───────────────────────┘                        │
│                         │                                                  │
│  ┌──────────────────────▼────────────────────────────────────────────────┐  │
│  │                        物理网卡                                      │  │
│  │   PF(0000:03:00.0)  ──  VF0(03:02.0) VF1(03:02.1) VF2 ...           │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────────────────┘
```

---

## 3. 四条组件链：各司其职

| 组件 | 职责 | 项目 |
|---|---|---|
| **资源发现/分配** | 发现节点上的 VF，上报为可调度资源，Pod 调度后分配具体 PCI 地址 | `sriov-network-device-plugin`（Device Plugin）<br>或 `dra-driver-sriov`（DRA） |
| **多网卡编排** | 作为主 CNI，读 Pod 注解，依次调用各个"网络"的 CNI 插件 | `multus-cni` |
| **VF 网卡配置** | 拿到 PCI 地址 → 找到 netdev → 刷 MAC/VLAN/… → 搬进 netns | `sriov-cni`（本文实现） |
| **IP 分配** | 给网卡分配 IP、网关、路由 | `whereabouts` / `host-local` / `static` / `nv-ipam` |

**职责边界很重要**：sriov-cni 不管 IP 从哪来，IPAM 不管网卡是硬件还是软件。这是 CNI 生态能组合出几十种玩法的原因。

---

## 4. 完整时序：从 kubectl apply 到 Pod 里有 net1

```
 用户                 kubelet            DevicePlugin         CRI            Multus        sriov-cni      IPAM
  │                     │                     │                │               │              │            │
  │ ① kubectl apply     │                     │                │               │              │            │
  ├────────────────────►│                     │                │               │              │            │
  │                     │                     │                │               │              │            │
  │       ② scheduler 选中节点（看 allocatable 里 intel.com/xxx=8）            │               │              │
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
  │                     │                     │                │   ⑥ 读注解 /networks      │            │
  │                     │                     │                │   ⑦ PodResources API      │            │
  │                     │                     │                │      → 0000:03:02.1        │            │
  │                     │                     │                │   ⑧ delegate(注入 deviceID)│           │
  │                     │                     │                │               ├───────────►│           │
  │                     │                     │                │               │   ⑨ delegate IPAM ADD
  │                     │                     │                │               │────────────────────────►
  │                     │                     │                │               │◄────────────────────────┤
  │                     │                     │                │               │   ⑩ ip link set PF vf N ...
  │                     │                     │                │               │   ⑪ LinkSetNsFd → Pod netns
  │                     │                     │                │               │   ⑫ rename net1 / up / 配 IP
  │                     │                     │                │◄──────────────┤              │            │
  │                     │◄─────────────────────────────────────┤               │              │            │
  │◄────────────────────┤  Pod Running                          │               │              │            │
```

### 关键一步：deviceID 是怎么传进 CNI 的？

CNI 的输入只有三样东西：stdin 的 JSON、环境变量（CNI_COMMAND/CONTAINERID/NETNS/IFNAME/ARGS/PATH）、以及配置文件。

**Multus 的做法（当前主流）**：

1. NetworkAttachmentDefinition 上写注解，声明这个网络用的是哪个扩展资源：

   ```yaml
   annotations:
     k8s.v1.cni.cncf.io/resourceName: intel.com/intel_sriov_netdevice
   ```

2. Pod 的 `resources.limits` 里**必须**声明同名资源，否则调度器不会预留、kubelet 也不会分配设备：

   ```yaml
   resources:
     limits:
       intel.com/intel_sriov_netdevice: 1
   ```

3. Multus 在处理这个网络时，通过 kubelet 的 **PodResources API**（`/var/lib/kubelet/pod-resources/kubelet.sock`）查询"这个 Pod 已经分到了哪些设备"，按 `resourceName` 匹配，取到一个 PCI 地址。

4. Multus 把这个地址塞进委派给 `sriov-cni` 的配置 JSON 里的 `deviceID` 字段，然后执行插件。

> **因此 NAD 里手写 `deviceID` 只是"测试时的固定分配"，生产环境由 Multus 覆盖。**
> 手写固定值意味着两个 Pod 可能撞到同一个 VF。

**新一代做法（DRA + CDI）**：

k8s 1.35 起 DRA 正式 GA，配套出现了 `dra-driver-sriov`（k8snetworkplumbingwg）与 NVIDIA 的 DRA SR-IOV Driver。此时：

- 设备由 **ResourceSlice** 上报（带 CEL 可筛选的属性）
- Pod 通过 **ResourceClaim / ResourceClaimTemplate** 申领
- 分配结果（含接口名、MAC、IP）会写进 **ResourceClaim 的 `.status.devices`**
- 设备通过 **CDI（Container Device Interface）** 注入容器

对 CNI 而言，约定是：分配到的设备需要带上这两个属性，Multus 据此把 `deviceID` 传给 CNI 插件：

```
k8s.cni.cncf.io/resourceName   # 必须与 NAD 注解 k8s.v1.cni.cncf.io/resourceName 完全一致
k8s.cni.cncf.io/deviceID       # Multus 传给 CNI 插件的设备标识
```

**也就是说：无论走 Device Plugin 还是 DRA，sriov-cni 这层的代码几乎不用改 —— 它只认 `deviceID`。** 这正是本插件同时兼容 `顶层 deviceID`、`runtimeConfig.deviceID` 和 `PCIDEVICE_*` 环境变量的原因。

---

## 5. 三条技术路线对比

| | 路线 A：Device Plugin + Multus（经典） | 路线 B：DRA + CDI（现代） | 路线 C：单 CNI 内置多网卡 |
|---|---|---|---|
| 资源分配 | `sriov-network-device-plugin` | `dra-driver-sriov` | 自带 |
| 多网卡编排 | Multus | Multus | DANM / 自研 CNI |
| 成熟度 | **最成熟、资料最多** | 1.35 GA，生态在补齐 | 场景特定 |
| 资源粒度 | 数量（整数） | **结构化参数 + CEL 过滤** | — |
| 设备共享 | 不支持 | **支持**（多 Pod 共享一设备） | 视实现 |
| 推荐场景 | 生产首选、存量集群 | 新集群、需要精细调度 | 电信/NFV 定制 |

> ⚠️ 注意：**同一集群里不要同时跑 SR-IOV Device Plugin 和 DRA Driver**，二者会冲突（NVIDIA 文档明确说明：启用 DRA 后 device plugin 不会运行）。

---

## 6. IPAM 选型

| IPAM | 特点 | 适用 |
|---|---|---|
| `host-local` | 每节点独立分配，配置在宿主机本地文件里 | 单节点测试；多节点需按节点切分网段 |
| `static` | 在 NAD 里写死 IP | 固定 IP 场景（NFV、数据库） |
| `whereabouts` | **集群感知**，用 CRD 保证全局不冲突 | **生产推荐** |
| `nv-ipam` | NVIDIA Network Operator 自带，支持 per-node block | 用 NVIDIA Network Operator 时 |
| `spiderpool` | 国内常用，IPAM + 多网卡协调 | 裸金属、金融 |

---

## 7. 网络规划设计要点

给 Pod 第二张网卡，本质是**把 Pod 接入物理网络（underlay）**，所以要跟网络团队对齐：

```
  ┌──────────────────────────────────────────────────────────┐
  │                      交换机                             │
  │   端口 Gi0/1:  switchport mode trunk                    │
  │                switchport trunk allowed vlan 1000       │
  │                （端口安全 / MAC 限制要放开 VF 数量）     │
  └───────────────────────┬──────────────────────────────────┘
                          │
                    物理链路
                          │
  ┌───────────────────────▼──────────────────────────────────┐
  │                    服务器                                │
  │   PF ens1f0                                              │
  │     ├─ VF0 vlan 1000 → Pod A net1  192.168.100.10/24    │
  │     ├─ VF1 vlan 1000 → Pod B net1  192.168.100.11/24    │
  │     └─ VF2 vlan 1000 → Pod C net1  192.168.100.12/24    │
  └──────────────────────────────────────────────────────────┘
```

关键决策：

| 问题 | 选项 | 影响 |
|---|---|---|
| **VLAN 谁打？** | 网卡硬件（VF vlan）/ Pod 内建子接口 | 硬件打 tag 性能更好、Pod 无感知 |
| **IP 从哪来？** | 独立网段 / 与节点同网段 | 影响网关可达性与 ARP 行为 |
| **默认路由怎么办？** | 不加 / 策略路由 | 见 [04 篇](./04-实操-第二块网卡.md) |
| **NUMA 对齐？** | 网卡与 CPU 同节点 | 影响跨 NUMA 访存时延 |

---

## 8. 最小可用清单

实际落地需要的东西，按依赖顺序：

**① 节点层（一次性）**
```bash
# BIOS: SR-IOV / VT-d / Above 4G Decoding
# kernel: intel_iommu=on iommu=pt
echo 8 > /sys/class/net/ens1f0/device/sriov_numvfs   # 并做持久化
```

**② 集群层**

| 组件 | 安装方式 |
|---|---|
| Multus | `kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/master/deployments/multus-daemonset-thick.yml` |
| sriov-cni | 把编译好的二进制放到每个节点 `/opt/cni/bin/sriov`（或让 device plugin / operator 自动放置） |
| sriov-device-plugin | 官方 `deployments/` 下的 DaemonSet + 上面那个 ConfigMap |
| whereabouts | `kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/whereabouts/master/doc/crds/...`（按官方文档） |

**③ 业务层**
- NetworkAttachmentDefinition（[deploy/02-nad-sriov-vlan1000.yaml](../../deploy/02-nad-sriov-vlan1000.yaml)）
- Pod 注解 + resources.limits（[deploy/03-pod-dual-nic.yaml](../../deploy/03-pod-dual-nic.yaml)）

> 生产环境更推荐直接用 **`sriov-network-operator`**，它把上面 ②③④ 全自动化了（含 VF 创建、驱动绑定、device plugin 配置、NAD 生成）。

---

**上一篇**：[01 · SR-IOV 原理](./01-SRIOV原理.md) · **下一篇**：[03 · 手写一个 SR-IOV CNI](./03-编写SRIOV-CNI.md)

> **不想用 Multus？** 本插件也可以作为链式插件运行并自己选 VF：见 [08 · 不依赖 Multus](./08-不依赖Multus.md)。
