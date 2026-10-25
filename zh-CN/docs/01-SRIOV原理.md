# 01 · SR-IOV 技术原理

## 1. 一句话概括

**SR-IOV（Single Root I/O Virtualization）让一块物理网卡在 PCIe 层面"分身"成多块独立的网卡**：一块完整的 PF（Physical Function）负责管理，若干块轻量的 VF（Virtual Function）各自拥有独立的 PCI 地址、配置空间、队列和中断，可以被**直接分配给某个容器或虚拟机的网络命名空间**，数据面完全绕开宿主机内核协议栈和虚拟交换机。

> 关键词是 **"直接"** 和 **"独立"**：不是软件模拟一张网卡（veth/tap），而是把真硬件的一小块能力交给租户独占。

---

## 2. 为什么要它：先看传统方案卡在哪

在一台跑容器的机器上，传统路径是这样的：

```
                    ┌───────────────────── 宿主机内核 ─────────────────────┐
   ┌──────┐   DMA   │  ┌────────┐   ┌─────────┐   ┌───────┐   ┌────────┐ │
   │ 网卡 │ ──────► │  │ 网卡驱动│──►│ vswitch │──►│  veth │──►│Pod 协议栈│ │
   └──────┘         │  │        │   │(OVS/br) │   │  pair │   │        │ │
      ▲             │  └────────┘   └─────────┘   └───────┘   └────────┘ │
      │             │      ①           ②             ③            ④     │
      │             └────────────────────────────────────────────────────┘
      │                                                                   
   MSI-X 中断                                                              
```

每一跳都是成本：

| 开销来源 | 说明 |
|---|---|
| **内存拷贝** | 报文至少要在 host 与 guest 之间搬 1~2 次 |
| **上下文切换 / 系统调用** | vhost-net、tap 的 read/write 引发用户态↔内核态切换 |
| **vswitch 处理** | OVS/bridge 的查表、netfilter、conntrack 消耗大量 CPU |
| **中断与软中断** | 每个包都触发中断 → softirq → ksoftirqd 抢占业务核 |
| **锁竞争** | 多队列共享队列锁、conntrack 全局锁 |

结果是：**10G 网卡跑满了要吃掉好几个核，40G/100G 基本跑不满，时延抖动大**。

### SR-IOV 之后

```
   ┌──────────────────────────────────────────────────┐
   │                   物理网卡 (NIC)                 │
   │   ┌──────────────────────────────────────────┐   │
   │   │  内部 L2 分类器 / 交换矩阵 (MAC,VLAN 过滤) │   │
   │   └───┬──────────┬──────────┬──────────┬─────┘   │
   │       │          │          │          │         │
   │    ┌──▼──┐    ┌──▼──┐    ┌──▼──┐    ┌──▼──┐      │
   │    │ VF0 │    │ VF1 │    │ VF2 │    │ VF3 │      │  ← 各自独立的
   │    │队列 │    │队列 │    │队列 │    │队列 │      │    队列/MSI-X/BAR
   │    └──┬──┘    └──┬──┘    └──┬──┘    └──┬──┘      │
   └───────┼──────────┼──────────┼──────────┼───────────┘
           │          │          │          │
         DMA        DMA        DMA        DMA        ← 目标内存由驱动
           │          │          │          │           预先映射好
      ┌────▼───┐ ┌───▼────┐ ┌───▼────┐ ┌───▼────┐
      │ Pod A  │ │ Pod B  │ │ VM C   │ │ DPDK  │
      │ netns  │ │ netns  │ │        │ │ 进程  │
      └────────┘ └────────┘ └────────┘ └───────┘
           宿主机内核协议栈：完全不参与（数据面）
```

收益：**接近线速的吞吐、微秒级时延、几乎为零的 CPU 占用**（中断可被轮询模式取代）。

---

## 3. PCIe 基础：要理解 SR-IOV 必须先懂的 4 个概念

SR-IOV 是 PCIe 规范的扩展能力，不是网络技术。以下 4 个概念是理解它的地基。

### 3.1 BDF 与 Requester ID

每个 PCIe 功能（Function）有一个 **BDF**：

```
   0000 : 03 : 02 . 1
   ──┬─   ─┬─  ─┬─  ─┬─
     │     │    │    └─ Function（0..7，开 ARI 后 0..255）
     │     │    └────── Device（0..31）
     │     └─────────── Bus（0..255）
     └───────────────── Domain / Segment（0..65535）
```

这个 BDF 就是 **Requester ID**，网卡发起 DMA 时会把它挂在 TLP 上。**IOMMU 正是根据它来查页表、决定这次 DMA 能不能访问目标内存** —— 这是 SR-IOV 隔离的硬件根基。

### 3.2 配置空间（Configuration Space）

每个 Function 有一段配置空间，包含 Vendor ID、Device ID、Class Code、BAR 等。SR-IOV 在其中增加了一个 **Extended Capability（ID = 0x0010）**。

### 3.3 BAR（Base Address Register）

BAR 描述了"这个设备的寄存器/内存窗口映射到主机物理地址空间的哪里"。驱动通过 MMIO 读写 BAR 来操作网卡。

**关键点：VF 的 BAR 是从 PF 的 SR-IOV BAR aperture 里切出来的**。PF 驱动创建 VF 时，固件/驱动给每个 VF 分配一段独立的窗口。所以每个 VF 有自己的寄存器视图，能独立收发包。

### 3.4 MSI-X

多向量中断表。每个 VF 有自己的 MSI-X table 条目，因此**每个 VF 的队列可以独立产生中断，路由到不同的 CPU** —— 这是多队列 + RSS 发挥性能的前提。

---

## 4. SR-IOV 规范：PF 与 VF

### 4.1 两种 Function

| | PF（Physical Function） | VF（Virtual Function） |
|---|---|---|
| 能力 | 完整功能，含管理面 | 仅数据面必需的最小资源 |
| 数量 | 每个设备至少 1 个 | 由硬件决定，规范上限 256 |
| 配置空间 | 完整 + SR-IOV Capability | 精简，VF BAR 来自 PF |
| 驱动 | ixgbe/i40e/ice/mlx5_core/… | ixgbevf/iavf/mlx5_core(同) |
| 是否可管理 VF | 是（`ip link set <pf> vf N …`） | 否 |
| 是否可直通 | 可作 PF passthrough | 可直通给容器/VM |

### 4.2 SR-IOV Extended Capability 结构

```
PCIe Config Space (Extended Capability, ID=0x0010)
├── SR-IOV Capabilities Register   支持的功能位
├── SR-IOV Control / Status        VF Enable / VF Migration Enable ...
├── InitialVFs                     硬件支持的初始 VF 数
├── TotalVFs                       硬件支持的总 VF 数上限
├── NumVFs                         ★ 当前启用的 VF 数（写它来开启 VF）
├── Function Dependency Link       VF 依赖的 PF 编号
├── First VF Offset                第一个 VF 的 Routing ID 偏移
├── VF Stride                      相邻 VF 的 Routing ID 步长
├── VF Device ID                   VF 暴露的 Device ID
├── Supported Page Sizes
├── System Page Size
├── VF BAR0 .. VF BAR5             ★ VF 的 BAR 窗口（从 PF aperture 切分）
└── VF Migration State Array Offset
```

### 4.3 怎么"开启"VF

Linux 把它导出成 sysfs 文件，写数字即可：

```bash
# 查看这个 PF 最多能开多少 VF
cat /sys/class/net/ens1f0/device/sriov_totalvfs
# 63

# 开 8 个 VF
echo 8 > /sys/class/net/ens1f0/device/sriov_numvfs

# 现在能看到了
lspci | grep -i "virtual function"
ip link show ens1f0
# 5: ens1f0: <BROADCAST,MULTICAST,UP,LOWER_UP> ...
#     vf 0     link/auto,  spoof checking on, link-state auto, trust off
#     vf 1     link/auto,  spoof checking on, link-state auto, trust off
```

**这一步本质是：PF 驱动向 IOMMU 注册这些 VF 的 Requester ID，并为每个 VF 分配 BAR 窗口、队列、MSI-X 向量。**

### 4.4 VF 在内核里长什么样

开启 VF 后，每个 VF 是**一个独立的 PCI 设备**，有自己的 BDF。如果绑定了内核驱动，就有一个 netdev：

```bash
$ ls /sys/bus/pci/devices/
0000:03:00.0      # PF
0000:03:02.0      # VF 0
0000:03:02.1      # VF 1

# 拓扑关系是靠软链接表达的
$ readlink /sys/bus/pci/devices/0000:03:02.1/physfn
../0000:03:00.0

$ readlink /sys/bus/pci/devices/0000:03:00.0/virtfn1
../0000:03:02.1

# netdev 名
$ ls /sys/bus/pci/devices/0000:03:02.1/net/
ens1f0v1
```

> 这三条 sysfs 路径就是后面 CNI 里 `GetVFInfo()` 的全部实现依据。

---

## 5. 隔离是怎么做到的：IOMMU / VT-d

很多人以为"直通"就是把设备 fd 给进程。真正的隔离由 **IOMMU** 保证。

### 5.1 DMA 重映射

没有 IOMMU 时，设备发起 DMA 用的是**主机物理地址**，可以访问**任意内存** —— 直通设备 = 恶意设备可以读全部内存。

有了 IOMMU（Intel VT-d / AMD-Vi / ARM SMMU）后：

```
设备 (Requester ID = 0000:03:02.1)
   │  发起 DMA，携带地址 GPA/IOVA + BDF
   ▼
┌─────────────────────────────────────────┐
│                 IOMMU                   │
│  1. 按 BDF 查 Root Table                │
│  2. 查 Context Table → 得到页表基址      │
│  3. 用二级页表翻译 IOVA → HPA            │
│  4. 检查该页的读写权限 → 越权则拒绝      │
└─────────────────────────────────────────┘
   │
   ▼
只允许访问分配给该 VF 的内存
```

### 5.2 开启

```bash
# /etc/default/grub
GRUB_CMDLINE_LINUX_DEFAULT="intel_iommu=on iommu=pt"
# AMD: amd_iommu=on iommu=pt
update-grub && reboot

# 验证
dmesg | grep -i -E "DMAR|IOMMU"
# DMAR: Intel-IOMMU enabled
find /sys/kernel/iommu_groups/ -maxdepth 1 -type d | wc -l
```

BIOS 侧需要打开：`VT-d`（Intel）/ `IOMMU`（AMD）、`SR-IOV`、`Above 4G Decoding`（大 BAR 必需）。

### 5.3 IOMMU Group

**IOMMU group 是隔离的最小粒度**，不是单个设备。同一个 group 内的设备之间无法互相隔离（常见于多 Function 设备、或 PCIe 交换/桥不支持 ACS 的情况）。VFIO 要求把整个 group 都交给 vfio-pci，否则拒绝直通。

```bash
# 查看某个 VF 属于哪个 group 以及 group 里有哪些设备
readlink /sys/bus/pci/devices/0000:03:02.1/iommu_group
ls /sys/kernel/iommu_groups/$(readlink /sys/bus/pci/devices/0000:03:02.1/iommu_group | xargs basename)/devices/
```

### 5.4 ⚠️ 一个常见误区：容器用 SR-IOV 不一定需要 IOMMU

这点非常容易搞混：

| 模式 | 需要 vfio-pci？ | 需要 IOMMU？ | 原因 |
|---|---|---|---|
| **netdevice 模式**（VF 绑内核驱动，netdev 搬进 Pod netns） | 否 | **否** | 设备始终由宿主机内核驱动控制，Pod 只是拿到了 netdev 的 socket/队列，没有 DMA 权限 |
| **DPDK / vfio 模式**（VF 绑 vfio-pci，用户态 PMD） | 是 | **是**（否则要开 `enable_unsafe_noiommu_mode`，不安全） | 用户态进程要直接编程网卡寄存器并发起 DMA |

**结论：k8s 里最常见的 "SR-IOV CNI + 普通 netdev" 用法，不需要配 IOMMU。**IOMMU 只在 DPDK/vfio 或虚拟机直通场景才是必需的。

---

## 6. VF 的硬件能力与"必须由 PF 下发"的属性

这是 SR-IOV 与软件网卡最大的体验差异：**VF 的很多属性不在 VF 自己的 netdev 上，而在 PF 上，要通过 PF 驱动写进网卡硬件。**

对应命令：

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

- 网卡硬件为 VF 做 **802.1Q tag 的插入（TX）和剥离（RX）**
- Pod 里看到的是 **untagged 流量**，`net1` 上不需要建 vlan 子接口
- 交换机侧端口要 **trunk 放通该 VLAN**
- 一个 VF **只能配一个 VLAN**（部分新硬件支持 QinQ）

### 6.2 MAC 与 spoof checking

- VF 默认由 PF 分配一个 MAC，`spoofchk` 默认 **on**
- **要改 MAC，必须先 `spoofchk off`**，否则硬件会丢弃源 MAC 不匹配的帧
- `spoofchk off` 有安全风险：租户可伪造 MAC/IP → 生产环境建议保持 on，让 CNI 来设置 MAC 后再打开

> 这也是本插件 `ConfigureVFHardware()` 里"先 down → 再 spoofchk → 再 MAC"顺序的原因。

### 6.3 trust

`trust on` 允许 VF 自行开启混杂模式、加入多播组、修改 MAC。运行 DPDK、抓包、跑 OVS-DPDK 时通常需要。

### 6.4 限速与统计

速率限制（min/max tx rate）和每 VF 的收发包统计由硬件提供：

```bash
ip -s link show ens1f0 | grep -A2 "vf 3"
ethtool -S ens1f0 | grep vf3
```

---

## 7. 三种运行模式

### 7.1 netdevice 模式（内核驱动）—— k8s 容器最常见

```
VF --bind--> iavf/mlx5_core 驱动 --> 产生 netdev --> 搬进 Pod netns --> 有 IP、可路由
```

- Pod 里看到一张普通网卡，能用标准 socket / `ip` 命令
- 走内核协议栈（因此有 conntrack/iptables，但**不是**宿主机的那一份）
- 性能好，但不如 DPDK 极致

### 7.2 vfio / DPDK 模式

```
VF --bind--> vfio-pci --> /dev/vfio/<group> 挂进容器 --> DPDK PMD 用户态轮询收发
```

- 没有 netdev，Pod 里 `ip link` **看不到**网卡
- 需要 hugepage、绑核、同 NUMA
- 极致性能（百万 PPS、微秒时延）
- 本插件用 `"dpdk": true` 支持，此时跳过 netns 搬运，只回传 PCI 地址

### 7.3 switchdev 模式（representor）—— Mellanox/NVIDIA

```
      VF ────────────────┐
                         │  网卡内部
      representor netdev ┘
                         │
                    TC flower / OVS 卸载
```

- 每个 VF 在 host 上有一个 **representor netdev**
- 控制面（OVS/TC）可以把流表**卸载到网卡硬件**，数据面仍然直通
- 兼得"SDN 可编程"与"硬件性能"
- 是很多云厂商裸金属 SDN 的实现方式

---

## 8. SR-IOV 的代价与限制

| 限制 | 说明 |
|---|---|
| **可迁移性差** | 直通设备与具体硬件绑定，热迁移基本不可用（需硬件/厂商支持迁移 capability） |
| **VF 数量有限** | 通常 8~256 个，受 PCIe 带宽、BAR 空间、NUMA 约束 |
| **可观测性变差** | 流量绕过内核 → 宿主机 `tcpdump`、iptables 统计、conntrack 全部失效 |
| **不支持宿主机 SDN 策略** | NetworkPolicy、服务网格 iptables 规则对 SR-IOV 网卡不生效 |
| **硬件/驱动强依赖** | 特性差异大（i40e vs ice vs mlx5），升级驱动要谨慎 |
| **安全面变大** | DMA 攻击面、`spoofchk off` 后租户可伪造地址 |
| **IPAM 复杂** | 需要跟物理网络/VLAN 协同，IP 资源要统一规划 |

---

## 9. 与"软件多网卡"方案对比

| 方案 | 层级 | 性能 | 硬件依赖 | 可迁移 | 典型用途 |
|---|---|---|---|---|---|
| **veth + bridge** | 纯软件 | 中 | 无 | 好 | 默认 CNI（Calico/Flannel） |
| **macvlan / ipvlan** | 软件复用 PF | 较高 | 无 | 好 | 需要 Pod 直连二层网络 |
| **SR-IOV VF** | 硬件直通 | **最高** | **需要 SR-IOV 网卡** | 差 | NFV、高频交易、AI/RDMA |
| **AF_XDP** | 内核旁路（软件） | 高 | 驱动支持 XDP | 好 | 中高性能、需灵活性 |
| **vhost-user / virtio** | 半虚拟化 | 中高 | 无 | 好 | 虚拟机通用方案 |
| **switchdev + VF** | 硬件直通+卸载 | 最高 | mlx5 等 | 差 | 云厂商 SDN |

**选型口诀**：要极致性能且能接受绑定硬件 → SR-IOV；要灵活迁移 → macvlan 或 AF_XDP；要 SDN 策略 + 高性能 → switchdev。

---

## 10. 关键命令速查

```bash
# 硬件是否支持
lspci -vvv -s 03:00.0 | grep -A20 "Single Root I/O Virtualization"

# 开 VF
echo 8 > /sys/class/net/ens1f0/device/sriov_numvfs

# 看 VF 状态
ip link show ens1f0

# 配置 VF
ip link set ens1f0 vf 3 mac 02:11:22:33:44:55 vlan 1000 spoofchk off trust on

# 把 VF 搬到某个 netns
ip link set ens1f0v3 netns /var/run/netns/pod1

# 驱动绑定（DPDK 模式）
modprobe vfio-pci
driverctl set-override 0000:03:02.3 vfio-pci
```

---

**下一篇**：[02 · 如何与 Kubernetes CNI 结合](./02-与K8s-CNI结合.md)
