# 03 · 手写一个 SR-IOV CNI

## 1. CNI 插件的"契约"

一个 CNI 插件就是一个可执行文件，容器运行时通过 **环境变量 + stdin + stdout** 调它：

**环境变量**

| 变量 | 含义 |
| --- | --- |
| `CNI_COMMAND` | `ADD` / `DEL` / `CHECK` / `VERSION` |
| `CNI_CONTAINERID` | 容器 ID |
| `CNI_NETNS` | 目标 netns 路径，如 `/proc/12345/ns/net` |
| `CNI_IFNAME` | 容器内网卡名，如 `net1` |
| `CNI_ARGS` | `K8S_POD_NAME=...;K8S_POD_NAMESPACE=...;...` |
| `CNI_PATH` | 插件搜索路径 |

**stdin**：网络配置 JSON（对 Multus 而言，就是 NetworkAttachmentDefinition 里的 `config` 内容）。

**stdout**：`ADD` 返回 `Result` JSON（含 interfaces / ips / routes / dns）。

**幂等要求**：`ADD` 出错要自己清理干净；`DEL` 对"已经不存在"的情况要不报错。

用 `github.com/containernetworking/cni/pkg/skel` 可以把这些样板全包掉：

```go
func main() {
    skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, "CNI sriov plugin v0.1.0")
}
```

---

## 2. 本项目结构

```
cmd/sriov/main.go      插件入口：ADD / DEL / CHECK 三条主流程
pkg/config/config.go   配置解析与校验（deviceID / mac / vlan / spoofchk / trust ...）
pkg/config/cache.go    把 VF 原始状态缓存到 /var/lib/cni/sriov，供 DEL 还原
pkg/config/linkstate.go 链路状态常量
pkg/sriov/sriov.go     ★ VF 拓扑发现（sysfs 解析）
pkg/sriov/configure.go ★ VF 硬件属性下发 / 还原 / netns 搬运
pkg/ipam/ipam.go       IPAM delegate + 地址/路由/策略路由下发
pkg/ns/ns.go           netns 进出封装（含锁线程处理）
pkg/logging/logging.go 极简日志
```

---

## 3. 第一步：从 deviceID 找到 VF（`pkg/sriov/sriov.go`）

这是整个插件的起点，也是最容易写错的地方。核心就是三条 sysfs 路径：

```go
const sysBusPci = "/sys/bus/pci/devices"

// 1) VF -> netdev
/sys/bus/pci/devices/<deviceID>/net/<netdev名>

// 2) VF -> PF（physfn 是 VF 独有的软链接，PF 没有）
/sys/bus/pci/devices/<deviceID>/physfn   ->  ../0000:03:00.0

// 3) PF -> 第 N 个 VF（反查 vfID）
/sys/bus/pci/devices/<pfBDF>/virtfn<N>   ->  ../0000:03:02.N
```

对应实现（`GetVFInfo`）：

```go
func GetVFInfo(conf *config.NetConf) (*VFInfo, error) {
    vfDir := filepath.Join(sysBusPci, deviceID)

    netdev, err := netDevForPci(deviceID)      // ① VF 的 netdev 名
    if err != nil {
        return nil, err
    }

    pfBDF, err := filepath.EvalSymlinks(filepath.Join(vfDir, "physfn"))  // ② 找到 PF
    if err != nil {
        return nil, fmt.Errorf("sriov: %s is not a VF (no physfn link): %v", deviceID, err)
    }
    pfBDF = filepath.Base(pfBDF)               // "0000:03:00.0"
    pfName, err := netDevForPci(pfBDF)         // "ens1f0"

    vfID, err := findVfID(pfBDF, deviceID)     // ③ 扫描 virtfnN 反查序号
    ...
}
```

**两个容易踩的坑**：

> **坑 1**：`netDevForPci` 失败最常见的原因是 **VF 没绑内核驱动**（比如被绑到了 `vfio-pci` 做 DPDK）。DPDK 模式下 VF 根本没有 netdev，此时必须走 `"dpdk": true` 分支。

> **坑 2**：`vfID` 和 PCI 地址的 Function 号**不一定相等**。老网卡常常相等（`03:02.3` ↔ `vf 3`），但很多新硬件（尤其开了 ARI 的）并不如此。所以必须扫 `virtfn*` 反查，不能靠字符串解析。

单元测试 `pkg/sriov/sriov_test.go` 用一棵假 sysfs 树覆盖了这段逻辑：

```bash
$ go test ./pkg/sriov/ -v
--- PASS: TestGetVFInfo (0.00s)
--- PASS: TestGetVFInfoNotAVF (0.00s)
--- PASS: TestFindVfIDNoVFs (0.00s)
```

`pkg/ns/ns_test.go` 则实测验证了 `Do()` 的 netns 进出：创建 dummy 网卡 → 搬进新 netns → 确认宿主侧消失、新 netns 内可见、且闭包返回后线程确实切回了宿主 netns。

---

## 4. 第二步：刷 VF 硬件属性（`pkg/sriov/configure.go`）

### 4.1 为什么不能只改 netdev 的 MAC

VF 的 MAC/VLAN/spoofchk/trust/link-state **不是 netdev 的属性，而是网卡硬件里的表项**。修改它们等价于执行：

```bash
ip link set ens1f0 vf 3 mac 02:11:22:33:44:55 vlan 1000 spoofchk off trust on
```

在 Go 里就是 netlink 的 `IFLA_VF_*` 系列：

```go
netlink.LinkSetVfHardwareAddr(pfLink, vfID, hwaddr)  // IFLA_VF_MAC
netlink.LinkSetVfVlanQos(pfLink, vfID, vlan, qos)     // IFLA_VF_VLAN
netlink.LinkSetVfSpoofchk(pfLink, vfID, on)            // IFLA_VF_SPOOFCHK
netlink.LinkSetVfTrust(pfLink, vfID, on)               // IFLA_VF_TRUST
netlink.LinkSetVfState(pfLink, vfID, state)            // IFLA_VF_LINK_STATE
netlink.LinkSetVfRate(pfLink, vfID, minRate, maxRate)  // IFLA_VF_RATE
```

> **注意参数**：这些函数的第一个参数是 **PF 的 link**，不是 VF 的。这也是为什么必须先解析出 PFName 和 vfID。

### 4.2 顺序不能乱

```go
// 1) down —— 多数驱动（iavf/ice/i40e）不允许 up 状态改 MAC
netlink.LinkSetDown(vfLink)

// 2) spoofchk —— 先关掉，否则下一步设 MAC 会被硬件拒绝
netlink.LinkSetVfSpoofchk(pfLink, vfID, spoofchkOn)

// 3) MAC
netlink.LinkSetVfHardwareAddr(pfLink, vfID, hwaddr)
//    失败时退回 netlink.LinkSetHardwareAddr(vfLink, hwaddr)

// 4) VLAN（硬件打 tag/剥 tag）
netlink.LinkSetVfVlanQos(pfLink, vfID, vlan, qos)

// 5) trust / link-state / rate limit
```

### 4.3 为什么要在搬 netns **之前**刷

两个原因：

1. **可读性/可诊断性**：出问题时，在宿主机上 `ip link show ens1f0` 就能看到 VF 状态，不用 `nsenter`。
2. **驱动兼容性**：部分驱动在 VF netdev 位于非 init netns 时，对 VF 属性的写操作会返回奇怪的错误。

（技术上，VLAN/trust 这些在搬运后通过 PF 设置依然有效，因为操作对象是 PF。但 MAC 在 netdev 层面就要小心。）

### 4.4 保存原始状态

```go
type VfState struct {
    HostIFName   string  // 原 netdev 名，DEL 时要改回去
    EffectiveMAC string
    AdminMAC     string  // PF 侧记录的 MAC
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

从 PF 的 `Attrs().Vfs` 里读：

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

> 用 `LinkList()` 而不是 `LinkByName()` 来取 PF：只有完整的 link dump 才会带上 `IFLA_VFINFO`（即 `Attrs().Vfs`）。这是一个很隐蔽的坑。

---

## 5. 第三步：搬进 Pod netns

```go
netlink.LinkSetNsFd(vfLink, netnsFd)
```

一行代码，背后是内核的 `dev_change_net_namespace()`。它会：

- 关闭设备（`dev_close`）——所以搬进去时 VF 是 **down** 的
- 把设备的 netns 指针改成目标 netns
- 保留所有硬件配置（MAC、VLAN 等）

**重要**：搬运后 netdev 名字保持不变，除非目标 netns 里有同名设备（此时内核会加后缀）。

---

## 6. 第四步：进入 netns 收尾（`pkg/ns/ns.go`）

### 6.1 为什么必须锁线程

`setns(2)` **只影响调用它的那一个线程**。而 Go 的 goroutine 会被调度器在 OS 线程之间搬运。如果不锁线程，会出现：

> 上半段代码在 Pod netns 里配好了网卡，下半段代码突然在 host netns 里执行 → 报 `link not found`

所以：

```go
func init() {
    runtime.LockOSThread()   // 整个进程锁死在一条线程上
}
```

### 6.2 `Do()` 的实现要点

```go
func (n *netNS) Do(toRun func(hostNS NetNS) error) error {
    // ① 在主 goroutine 里抓一份"当前 netns"（就是 host）
    hostNS, err := GetCurrentNS()

    // ② 真正的切换放在新 goroutine 里，并先 LockOSThread
    go func() {
        runtime.LockOSThread()
        innerError = containedCall(hostNS)
    }()
    wg.Wait()
}

func containedCall(hostNS NetNS) error {
    threadNS, _ := GetCurrentNS()          // 记住这条线程原本的 netns
    netns.Set(n.handle)                    // 切进 Pod

    defer func() {
        // ③ 只有成功切回来，才 UnlockOSThread。
        //    切回失败就故意不解锁 —— 让 Go 运行时把这条"脏线程"整个丢掉，
        //    避免它被复用给别的 goroutine。
        if err := netns.Set(threadNS); err == nil {
            runtime.UnlockOSThread()
        }
    }()

    return toRun(hostNS)   // 把 host 的句柄传进去，方便把设备搬回去
}
```

### 6.3 改名为什么要在 down 状态

内核的 `dev_change_name()` 对 `IFF_UP` 的设备返回 `-EBUSY`。好在设备跨 netns 时已经被自动 close 了，所以：

```go
if name != args.IfName {
    netlink.LinkSetDown(link)              // 保险起见再 down 一次
    netlink.LinkSetName(link, args.IfName)
}
netlink.LinkSetUp(link)                    // 改完名再 up
```

---

## 7. 第五步：IPAM（`pkg/ipam/ipam.go`）

### 7.1 delegate 模式

sriov-cni 自己不分配 IP，而是把 **同一份配置 JSON** 交给 IPAM 插件执行：

```go
r, err := invoke.DelegateAdd(context.TODO(), conf.IPAM.Type, args.StdinData, nil)
res, err := types100.GetResult(r)
```

好处：host-local / static / whereabouts / dhcp 随便换，插件代码不动。

### 7.2 下发地址时的关键 flag

```go
addr := &netlink.Addr{
    IPNet: &ipc.Address,
    Flags: unix.IFA_F_NOPREFIXROUTE,
}
netlink.AddrAdd(link, addr)
```

`IFA_F_NOPREFIXROUTE` 的作用是：**不让内核自动添加前缀路由**。

- 不加：内核会自动往 main 表塞一条 `192.168.100.0/24 dev net1 scope link`
- 加了：main 表干净，路由完全由 IPAM 的 `routes` 控制

对于多网卡 Pod，这非常关键 —— 否则 main 表里会出现两条"到达同一网段"的路由，走哪条取决于 metric，行为不可预测。

### 7.3 策略路由（多网卡的正确解法）

如果 IPAM 返回了带 `table` 的路由，插件会自动补一条 `ip rule`：

```go
// 等价于：ip rule add from 192.168.100.10/32 table 100 pref 1100
rule := netlink.NewRule()
rule.Src      = &ipc.Address
rule.Table    = 100
rule.Priority = 1000 + 100
netlink.RuleAdd(rule)
```

这样"源地址是 net1 的流量 → 查 100 号表"，和 eth0 的默认路由互不干扰。

---

## 8. 第六步：DEL 的还原逻辑

DEL 比 ADD 更容易被写坏。三个要点：

### 8.1 先读缓存

运行时传给 DEL 的配置往往**不含 deviceID**（它是从 CNI 主配置里来的，不含 Multus 注入的值）。所以要把 ADD 时落盘的缓存读回来：

```go
if cached, e := config.LoadCachedConf(containerID, deviceID, ifName); e == nil {
    mergeCached(conf, cached)
}
```

### 8.2 netns 可能已经没了

Pod 被强杀时，内核会自动把 netns 里的 netdev 送回 init netns。所以：

```go
if _, statErr := os.Stat(args.Netns); statErr == nil {
    // netns 还在：进去把设备搬回来
    ...
}
// 无论上面成功与否，都在 host 上按 PCI 地址重新定位
netdev, err := sriov.GetNetdevByPci(vfInfo.DeviceID)
```

### 8.3 还原要覆盖全部属性

```go
LinkSetDown(vfLink)
LinkSetVfHardwareAddr(pfLink, vfID, origMAC)     // MAC
LinkSetVfVlanQos(pfLink, vfID, orig.Vlan, orig.VlanQoS)  // VLAN 清回 0
LinkSetVfSpoofchk(pfLink, vfID, orig.SpoofChk)    // 防欺骗
LinkSetVfTrust(pfLink, vfID, orig.Trust)
LinkSetVfState(pfLink, vfID, origState)
LinkSetVfRate(pfLink, vfID, orig.MinTxRate, orig.MaxTxRate)
LinkSetName(vfLink, orig.HostIFName)             // ★ 名字也要改回去
if orig.LinkUp { LinkSetUp(vfLink) }
```

> **为什么必须还原？** VF 是稀缺硬件资源。如果 Pod 删除后 VLAN 停留在 1000、spoofchk 关着，下一个拿到这个 VF 的 Pod 就会带着"脏配置"上线 —— 表现为随机连不通、且极难排查。

**还原失败只警告不报错**：DEL 里所有失败都用 `logging.Warnf`。因为 DEL 失败会导致 kubelet 反复重试、Pod 卡在 Terminating，比"残留一点脏配置"的代价大得多。

---

## 9. 编译与验证

```bash
cd /workspace/sriov-cni
export GOPROXY=https://goproxy.cn,direct
go build -o /tmp/sriov ./cmd/sriov
go test ./...
```

冒烟测试（本仓库已验证）：

```bash
# 缺 deviceID
$ echo '{"cniVersion":"1.0.0","name":"n","type":"sriov","ipam":{}}' \
  | CNI_COMMAND=ADD CNI_CONTAINERID=abc CNI_NETNS=/proc/self/ns/net \
    CNI_IFNAME=net1 CNI_PATH=/opt/cni/bin /tmp/sriov
{
    "code": 999,
    "msg": "sriov: missing \"deviceID\" (or \"pfName\"+\"vfID\"); it is normally injected by Multus from the SR-IOV Device Plugin"
}
```

---

## 10. 与上游 `sriov-cni` 的差异

| 能力 | 上游 | 本实现 |
| --- | --- | --- |
| 定位 VF | ✅ | ✅ |
| MAC / VLAN / QoS | ✅ | ✅ |
| spoofchk / trust / link-state | ✅ | ✅ |
| netns 搬运 + 改名 | ✅ | ✅ |
| IPAM delegate | ✅ | ✅ |
| 策略路由自动下发 | ❌（需外部 initContainer） | ✅ |
| DPDK 模式 | ✅（含驱动绑定） | ⚠️ 仅跳过搬运，绑定交给 device plugin |
| 速率限制 | ❌ | ✅ |
| RDMA / IB | ❌ | ❌ |

本实现是**教学/可二次开发的最小骨架**，生产建议直接使用上游 `sriov-cni` 或 `sriov-network-operator`。

---

**上一篇**：[02 · 与 K8s CNI 结合](./02-与K8s-CNI结合.md) · **下一篇**：[04 · 实操：Pod 里的第二块网卡](./04-实操-第二块网卡.md)
