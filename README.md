# sriov-cni · 从原理到手写 CNI，再到 Pod 第二块网卡

一个**可编译、可运行、带单元测试**的最小 SR-IOV CNI 插件实现，配套完整的原理讲解与 K8s 落地手册。

---

## 目录

| 文档 | 内容 |
|---|---|
| **[01 · SR-IOV 原理](./docs/01-SRIOV原理.md)** | PF/VF、PCIe 配置空间、BAR、MSI-X、IOMMU 隔离、报文路径对比、三种模式、代价与选型 |
| **[02 · 与 K8s CNI 结合](./docs/02-与K8s-CNI结合.md)** | 三大矛盾、四件套架构、完整时序、deviceID 注入机制、Device Plugin vs DRA、IPAM 选型 |
| **[03 · 手写 SR-IOV CNI](./docs/03-编写SRIOV-CNI.md)** | 逐行讲解代码实现：sysfs 拓扑发现、netlink 硬件属性下发、netns 搬运、IPAM delegate、DEL 还原 |
| **[04 · 实操：第二块网卡](./docs/04-实操-第二块网卡.md)** | 节点准备 → 集群组件 → NAD → Pod → 验证，**含默认路由冲突的三种解法** |
| **[05 · 排错与优化](./docs/05-排错与优化.md)** | 故障对照表、诊断命令集、NUMA/中断/大页调优、生产清单 |

**代码**

```
cmd/sriov/main.go     ADD / DEL / CHECK 主流程
pkg/config/           配置解析、校验、状态缓存
pkg/sriov/            ★ VF 拓扑发现 + 硬件属性下发 + netns 搬运
pkg/ipam/             IPAM delegate + 地址/路由/策略路由下发
pkg/ns/               netns 进出封装（锁线程）
```

---

## 30 秒速答

**Q：SR-IOV 的原理？**
一块物理网卡在 PCIe 层"分身"：PF 负责管理，VF 各自有独立 PCI 地址/BAR/队列/中断，可被直接交给容器或虚拟机的 netns，数据面绕开宿主机内核与虚拟交换机。隔离由 IOMMU 按 Requester ID(BDF) 做 DMA 重映射来保证。

**Q：怎么和 K8s CNI 结合？**
四件套：**Device Plugin（或 DRA）** 负责"哪块 VF 归这个 Pod" → **Multus** 负责"一个 Pod 多张网卡"的编排 → **sriov-cni** 负责把 VF 刷好并搬进 netns → **IPAM** 负责分 IP。

**Q：Pod 里怎么有第二块网卡和第二个 IP？**
NAD 定义第二网络 + Pod 注解 `k8s.v1.cni.cncf.io/networks` + `resources.limits` 声明 VF 资源。关键是**处理好默认路由冲突**（建议策略路由）。

---

## 构建

```bash
cd sriov-cni
./build.sh
# 或
make all
```

输出：`build/sriov`

```bash
# 安装到节点
sudo cp build/sriov /opt/cni/bin/sriov && sudo chmod +x /opt/cni/bin/sriov
```

### 依赖

```bash
go version        # 需要 1.21+
export GOPROXY=https://goproxy.cn,direct   # 国内
```

| 模块 | 版本 |
|---|---|
| `github.com/containernetworking/cni` | v1.2.3 |
| `github.com/vishvananda/netlink` | v1.2.1 |
| `github.com/vishvananda/netns` | v0.0.4 |
| `golang.org/x/sys` | v0.17.0 |

---

## 冒烟测试（无需集群）

```bash
# 1. 检查硬件是否支持
sudo ./deploy/setup-vfs.sh --check

# 2. 创建 VF 并持久化
sudo ./deploy/setup-vfs.sh --pf ens1f0 --numvfs 8

# 3. 手动调用插件走一遍 ADD/DEL
sudo ./deploy/test-cni.sh
```

### 无 SR-IOV 硬件时也能验证的部分

```bash
go test ./...      # sysfs 拓扑解析逻辑（用假 sysfs 树）

# 参数校验与错误提示
echo '{"cniVersion":"1.0.0","name":"n","type":"sriov","ipam":{}}' \
  | CNI_COMMAND=ADD CNI_CONTAINERID=abc CNI_NETNS=/proc/self/ns/net \
    CNI_IFNAME=net1 CNI_PATH=/opt/cni/bin ./build/sriov
# => "sriov: missing \"deviceID\" (or \"pfName\"+\"vfID\"); it is normally
#     injected by Multus from the SR-IOV Device Plugin"
```

---

## 配置字段速查

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

| 字段 | 默认 | 说明 |
|---|---|---|
| `deviceID` | — | VF 的 PCI 地址，**生产由 Multus 注入** |
| `vfID` | 自动推算 | VF 在 PF 下的序号 |
| `pfName` | 自动推算 | PF 的 netdev 名 |
| `mac` | 保留原值 | 需 `spoofchk: off` |
| `vlan` | 不设置 | 1..4094，硬件打 tag |
| `vlanQoS` | 0 | 802.1p 优先级 0..7 |
| `spoofchk` | `on` | 防 MAC 欺骗 |
| `trust` | `off` | 可信 VF |
| `link_state` | `auto` | `auto`/`enable`/`disable` |
| `min_tx_rate` / `max_tx_rate` | 0 | 限速 Mbps（依赖硬件） |
| `mtu` | 继承 | — |
| `dpdk` | `false` | true 时跳过 netns 搬运 |
| `resetOnDel` | `true` | DEL 时是否还原 VF |

`deviceID` 的三种来源（按优先级）：顶层 `deviceID` → `runtimeConfig.deviceID` → 环境变量 `PCIDEVICE_*`。

---

## 部署清单

| 文件 | 说明 |
|---|---|
| [`deploy/setup-vfs.sh`](./deploy/setup-vfs.sh) | 节点侧检查 + 创建/持久化 VF |
| [`deploy/01-device-plugin-cm.yaml`](./deploy/01-device-plugin-cm.yaml) | SR-IOV Device Plugin 资源池定义 |
| [`deploy/02-nad-sriov-vlan1000.yaml`](./deploy/02-nad-sriov-vlan1000.yaml) | NetworkAttachmentDefinition（whereabouts / 策略路由两个变体） |
| [`deploy/03-pod-dual-nic.yaml`](./deploy/03-pod-dual-nic.yaml) | 双网卡 Pod 示例 |
| [`deploy/04-nad-sriov-static-ip.yaml`](./deploy/04-nad-sriov-static-ip.yaml) | 固定 IP 的 NAD |
| [`deploy/test-cni.sh`](./deploy/test-cni.sh) | 无集群的 ADD/DEL 冒烟测试 |

---

## 适用范围

| 场景 | 建议 |
|---|---|
| 学习与二次开发 | ✅ 本仓库 |
| 生产部署 | 用上游 [`sriov-cni`](https://github.com/k8snetworkplumbingwg/sriov-cni) + [`multus-cni`](https://github.com/k8snetworkplumbingwg/multus-cni) |
| 全自动运维 | 用 [`sriov-network-operator`](https://github.com/k8snetworkplumbingwg/sriov-network-operator)（含 VF 创建、驱动绑定、NAD 生成） |
| 新集群 + 精细调度 | 关注 DRA（k8s 1.35 起 GA）+ [`dra-driver-sriov`](https://github.com/k8snetworkplumbingwg/dra-driver-sriov) |

---

## 验证状态

```
$ gofmt -l .                     无输出
$ go vet ./...                   clean
$ go build ./...                 OK  (静态链接 ELF，CGO_ENABLED=0)
$ go test ./...
    ok  github.com/example/sriov-cni/pkg/sriov    # sysfs 拓扑解析（假 sysfs 树）
    ok  github.com/example/sriov-cni/pkg/ns       # netns 进出与还原（需 root）
$ CNI_COMMAND=VERSION ./build/sriov
    {"cniVersion":"1.1.0","supportedVersions":["0.1.0","0.2.0","0.3.0","0.3.1","0.4.0","1.0.0","1.1.0"]}
$ ./deploy/setup-vfs.sh --check       # 在无 SR-IOV 硬件时正确报错退出
```

> 需要**真实 SR-IOV 网卡**（Intel E810/X710、Mellanox ConnectX 等）才能跑完整的 ADD/DEL 端到端；
> 通用虚拟机 / 大多数云主机没有这个能力。
