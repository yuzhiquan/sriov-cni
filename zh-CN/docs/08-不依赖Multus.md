# 08 · 不依赖 Multus：chained 模式与 VF 来源

[← 返回 README](../README.md) · 上一篇：[07 · VF 间隔离与带宽](./07-VF间隔离与带宽.md)

[第 02 篇](./02-与K8s-CNI结合.md) 讲的是标准四件套：Device Plugin → Multus → sriov-cni → IPAM。这一篇讲如何**去掉 Multus**，由本插件自己维护第二块网卡。

有两个相互独立的开关，可以任意组合：

| 开关 | 取值 | 回答的问题 |
|---|---|---|
| `mode` | `multus`（默认）· `chained` | **谁来调用插件？谁来决定 Pod 挂哪个网络？** |
| `vfAllocator` | `injected`（默认）· `podresources` · `pool` | **这个 Pod 用哪块 VF？** |

默认值（`multus` + `injected`）就是第 01–07 篇描述的行为；不设置这两个字段，什么都不会变。

---

## 1. Multus 原本替你做了什么

| Multus 的职责 | 没有 Multus 时 |
|---|---|
| 运行时对每个 Pod **只调用一次** CNI，由 Multus 再去调其他 CNI | 插件作为**同一个 conflist 里主 CNI 之后的一环**被调用（`mode: chained`） |
| 读 Pod 注解和 NAD，决定挂哪些网络 | 插件读 Pod 注解，在自己的 `networks` 表里查配置 |
| 读 kubelet PodResources API 并注入 `deviceID` | `vfAllocator: podresources` 自己查；或用 `vfAllocator: pool` 完全绕开 Device Plugin |

---

## 2. `mode: chained`

### 2.1 配置布局

CNI 运行时读取 `/etc/cni/net.d/*.conflist`，按顺序执行各插件，上一个插件的结果以 `prevResult` 传给下一个。把本插件放在主 CNI 后面：

```json
{
  "name": "k8s-pod-network",
  "cniVersion": "1.0.0",
  "plugins": [
    { "type": "calico", "...": "..." },
    {
      "type": "sriov",
      "mode": "chained",
      "vfAllocator": "podresources",
      "annotation": "sriov.example.com/network",
      "secondaryIfName": "net1",
      "kubernetes": { "apiServer": "https://10.96.0.1:443", "tokenFile": "...", "caFile": "..." },
      "podResources": { "resource": "intel.com/intel_sriov_netdevice" },
      "networks": {
        "vlan1000": { "vlan": 1000, "ipam": { "type": "whereabouts", "range": "192.168.100.0/24" } }
      }
    }
  ]
}
```

完整示例：[`deploy/05-chained-conflist-podresources.json`](../../deploy/05-chained-conflist-podresources.json)、[`deploy/06-chained-conflist-pool.json`](../../deploy/06-chained-conflist-pool.json)。

Pod 只需要一个注解，不需要 NAD，也不需要 Multus：

```yaml
metadata:
  annotations:
    sriov.example.com/network: vlan1000
```

### 2.2 ADD 流程

```
prevResult（主 CNI 已经建好 eth0）
   │
   ├─ 读 CNI_ARGS → K8S_POD_NAME / K8S_POD_NAMESPACE
   ├─ GET /api/v1/namespaces/<ns>/pods/<name> → 取注解
   │
   ├─ 没有注解  ──────────────►  原样输出 prevResult（普通 Pod 完全不受影响）
   │
   └─ 注解 = "vlan1000"
        ├─ 在 networks["vlan1000"] 里查配置        （名字不存在 → ADD 失败）
        ├─ 把它叠加到基础配置上                    （顶层键覆盖；ipam 整体替换）
        ├─ 确定 VF                                  （见第 3 节）
        ├─ 以 ifname=net1 走原有 ADD 流程
        └─ 把 net1 的接口 / IP / 路由追加到 prevResult 后输出
```

需要知道的细节：

- 合并后的配置在交给 IPAM 插件之前会剔除 `prevResult`、`plugins`、`networks`、`mode` 这几个键，IPAM 看不到它们。
- 追加进来的 IP 所引用的接口下标，会整体加上 `prevResult` 里已有的接口数。
- **合并后的**配置会和 VF 状态一起缓存。`DEL` 时运行时只会给未合并的 conflist，所以要用缓存里的版本去归还 IP。
- **`DEL` 是幂等的**：找不到 `<containerID>/net1` 的缓存，说明这个 Pod 当初没挂副网卡，直接返回成功、什么都不动。`CHECK` 同理。
- 一个注解值 = 一个网络名。该模式下不支持给同一个 Pod 挂多块 VF。

### 2.3 访问 API server

CNI 二进制跑在宿主机上，不在 Pod 里，没有 in-cluster 环境变量，所以要显式配置 `kubernetes.apiServer`、`tokenFile`、`caFile`。Token 只需要 `pods` 的 `get` 权限：见 [`deploy/07-chained-rbac.yaml`](../../deploy/07-chained-rbac.yaml)，文件头部有签发 token 的命令。Token 会过期，每个节点都要有轮转机制。

---

## 3. `vfAllocator`：Pod 用哪块 VF

| 取值 | `deviceID` 的来源 | 调度器能感知 VF 数量？ | 需要 Device Plugin？ |
|---|---|---|---|
| `injected`（默认） | 配置里的 `deviceID`、`runtimeConfig.deviceID`（Multus 注入）或 `PCIDEVICE_*` | 能（前提是用了 Device Plugin） | 通常需要 |
| `podresources` | kubelet PodResources API | **能** | 需要 |
| `pool` | 插件自己管理的节点本地池 | **不能** | 不需要 |

只要任何一个来源已经给出了 `deviceID`，就直接使用它，不再走分配器。两种分配器在两种模式下都可以用。

### 3.1 `podresources`（推荐）

调度与计量仍交给 Device Plugin（`resources.limits`），插件向 kubelet 查询它分配了哪块设备：

```json
"vfAllocator": "podresources",
"podResources": {
  "socket": "/var/lib/kubelet/pod-resources/kubelet.sock",
  "resource": "intel.com/intel_sriov_netdevice",
  "index": 0,
  "timeoutMs": 3000
}
```

- kubelet 在**准入** Pod 时就完成设备分配，早于创建 sandbox，所以 CNI ADD 时结果已经可查。插件仍会在 `timeoutMs` 内重试，以容忍缓存的短暂延迟。
- sriov device plugin 上报的 device ID 就是 VF 的 PCI 地址，正好是 `deviceID` 需要的值。
- Pod 在同一种资源下有多块设备时，用 `index` 选其中一块。
- 前提：CNI 进程能打开这个 socket（它是宿主机二进制，可以），并且 Pod 在 `resources.limits` 里声明了该资源。

### 3.2 `pool`

插件自己维护一个节点本地的池子，思路和 `host-local` 管 IP 一样：

```json
"vfAllocator": "pool",
"pool": {
  "pfNames": ["ens1f0"],
  "devices": ["0000:03:02.0"],
  "stateDir": "/var/lib/cni/sriov/pool"
}
```

- 候选集 = `pfNames` 下的全部 VF 加上 `devices`，顺序稳定。
- `stateDir` 下每个被占用的 VF 对应一个文件，内容是持有者（`<containerID>/<ifname>`）。整个目录用一把 `flock` 串行化，并发 ADD 不会拿到同一块 VF。
- 同一个持有者重试 ADD 会拿回同一块 VF；ADD 失败和 `DEL` 都会释放它。
- 即使状态文件丢了，host netns 里已经没有 netdev 的 VF（在别的 Pod 里）也会被跳过。
- **代价：** kube-scheduler 看不到这个池子。节点 VF 用完后 Pod 仍会被调度过来，在 CNI ADD 阶段才失败；Pod 异常退出而没走到 `DEL` 时，占用记录会一直留着，需要手工清理。适合单机实验、裸金属边缘节点，或不想部署 Device Plugin 的场景。
- `pool` 模式下不要再让 Pod 去申请 Device Plugin 管理的同一批 VF：两个分配器互不知情。

---

## 4. 怎么选

| 场景 | 建议配置 |
|---|---|
| 已有 Multus + Device Plugin 的集群 | 保持默认（`multus` + `injected`） |
| 想去掉 Multus，又要保留调度器对 VF 的感知 | `chained` + `podresources` |
| 实验 / 单节点，不要 Device Plugin 也不要 Multus | `chained` + `pool` |
| 保留 Multus，但不想在 NAD 里写 `resourceName` | `multus` + `podresources` |

## 5. 相比 Multus 放弃了什么

- chained 模式下副网卡只能是 SR-IOV；Multus 可以串任意 CNI。
- 每个 Pod 只有一块副网卡。
- 不会写 `k8s.v1.cni.cncf.io/network-status` 注解。
- 每个节点都要有 API token，并且要有轮转方案。
- conflist 要自己维护，改动会影响节点上所有 Pod。

## 6. 验证

```bash
go test ./...        # pool、podresources（假 kubelet gRPC 服务）、k8s（假 API server）、配置合并
```

在有真实硬件的节点上，装上 `deploy/05` 或 `deploy/06` 的 conflist，应用 `deploy/08-pod-chained.yaml`，然后检查：

```bash
kubectl exec test-chained-podresources -- ip -br addr     # eth0 + net1
tail /var/log/sriov-cni.log                               # "chained: pod default/... network "vlan1000" -> net1"
ls /var/lib/cni/sriov/pool                                # （pool 模式）每个被占用的 VF 一个文件
```

需要真实硬件的部分（把 VF 搬进 Pod、IPAM、DEL）和 multus 模式完全一样；它们之上的 chained 与分配器逻辑由单元测试覆盖。
