# 08 · Running Without Multus: Chained Mode and VF Allocators

[← Back to README](../README.md) · Previous: [07 · VF Isolation and Bandwidth](./07-vf-isolation-and-bandwidth.md)

[Doc 02](./02-integrating-with-k8s-cni.md) describes the standard stack: Device Plugin → Multus → sriov-cni → IPAM. This chapter shows how to **remove Multus** and let this plugin maintain the second NIC itself.

Two independent switches control this, so you can combine them freely:

| Switch | Values | Question it answers |
|---|---|---|
| `mode` | `multus` (default) · `chained` | **Who calls the plugin, and who decides which network the Pod gets?** |
| `vfAllocator` | `injected` (default) · `podresources` · `pool` | **Which VF does this Pod use?** |

The default (`multus` + `injected`) is exactly the behavior of chapters 01–07; nothing changes unless you set these fields.

---

## 1. What Multus did for you

| Multus duty | Without Multus |
|---|---|
| A runtime calls the CNI **once per Pod**; Multus calls the other CNIs | The plugin runs **after the primary CNI in the same conflist** (`mode: chained`) |
| Read the Pod annotation and the NAD to decide which networks to attach | The plugin reads a Pod annotation and looks the value up in its own `networks` table |
| Read kubelet's PodResources API and inject `deviceID` | `vfAllocator: podresources` does it, or `vfAllocator: pool` skips the Device Plugin altogether |

---

## 2. `mode: chained`

### 2.1 Layout

The CNI runtime reads `/etc/cni/net.d/*.conflist` and runs the plugins in order; each plugin receives the previous plugin's result as `prevResult`. Put this plugin after the primary CNI:

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

Full examples: [`deploy/05-chained-conflist-podresources.json`](../deploy/05-chained-conflist-podresources.json), [`deploy/06-chained-conflist-pool.json`](../deploy/06-chained-conflist-pool.json).

A Pod then only needs one annotation (no NAD, no Multus):

```yaml
metadata:
  annotations:
    sriov.example.com/network: vlan1000
```

### 2.2 ADD flow

```
prevResult (primary CNI already created eth0)
   │
   ├─ read CNI_ARGS → K8S_POD_NAME / K8S_POD_NAMESPACE
   ├─ GET /api/v1/namespaces/<ns>/pods/<name> → annotations
   │
   ├─ no annotation  ──────────────►  print prevResult unchanged (ordinary Pods are untouched)
   │
   └─ annotation = "vlan1000"
        ├─ look up networks["vlan1000"]            (unknown name → ADD fails)
        ├─ merge it over the base config           (top-level keys override; "ipam" is replaced whole)
        ├─ resolve the VF                          (§3)
        ├─ run the normal ADD flow with ifname=net1
        └─ append net1's interface / IPs / routes to prevResult and print it
```

Details worth knowing:

- Keys `prevResult`, `plugins`, `networks`, `mode` are stripped before the merged config is handed to the IPAM plugin, so it never sees them.
- The interface index of each appended IP is shifted by the number of interfaces in `prevResult`.
- The **merged** config is cached with the VF state. `DEL` replays it to the IPAM plugin, because the runtime only gives `DEL` the un-merged conflist.
- **`DEL` is idempotent**: no cache entry for `<containerID>/net1` means this Pod never had a second NIC, and `DEL` returns success without touching anything. `CHECK` behaves the same way.
- Annotation value = one network name. Attaching several VFs to one Pod is not supported in this mode.

### 2.3 Access to the API server

A CNI binary runs on the host, not in a Pod, so there are no in-cluster environment variables. Configure `kubernetes.apiServer`, `tokenFile` and `caFile` explicitly. The token only needs `get` on `pods`: see [`deploy/07-chained-rbac.yaml`](../deploy/07-chained-rbac.yaml), which also lists the commands that mint the token. Tokens expire, so rotate the file on each node.

---

## 3. `vfAllocator`: which VF does the Pod get?

| Value | Source of `deviceID` | Scheduler aware of VF count? | Needs Device Plugin? |
|---|---|---|---|
| `injected` (default) | `deviceID` in the config, `runtimeConfig.deviceID` (Multus), or `PCIDEVICE_*` | yes (if a Device Plugin is used) | normally yes |
| `podresources` | kubelet PodResources API | **yes** | yes |
| `pool` | node-local pool managed by the plugin | **no** | no |

If `deviceID` is already set by any source, it wins and the allocator is skipped. Both allocators work in both modes.

### 3.1 `podresources` (recommended)

The Device Plugin keeps doing scheduling and accounting (`resources.limits`). The plugin asks kubelet which device it allocated:

```json
"vfAllocator": "podresources",
"podResources": {
  "socket": "/var/lib/kubelet/pod-resources/kubelet.sock",
  "resource": "intel.com/intel_sriov_netdevice",
  "index": 0,
  "timeoutMs": 3000
}
```

- kubelet allocates devices while **admitting** the Pod, before the sandbox is created, so the result is already visible when CNI ADD runs. The plugin still retries for `timeoutMs` to cover cache lag.
- The sriov device plugin reports the VF's PCI address as the device ID, which is exactly what `deviceID` needs.
- `index` picks one device when the Pod holds several of the same resource.
- Requires: the CNI process can open the socket (it is a host binary, so it can), and the Pod declares the resource in `resources.limits`.

### 3.2 `pool`

The plugin owns a node-local pool, like `host-local` does for IPs:

```json
"vfAllocator": "pool",
"pool": {
  "pfNames": ["ens1f0"],
  "devices": ["0000:03:02.0"],
  "stateDir": "/var/lib/cni/sriov/pool"
}
```

- Candidates = every VF under `pfNames` plus any `devices`, in a stable order.
- One file per claimed VF under `stateDir` records the owner (`<containerID>/<ifname>`). A single `flock` serialises concurrent ADDs, so two Pods can never get the same VF.
- Retrying ADD for the same owner returns the same VF. A failed ADD, and `DEL`, release it.
- A VF with no netdev left in the host netns (it is inside another Pod) is skipped even if the state file was lost.
- **Trade-off:** kube-scheduler cannot see this pool. When a node runs out of VFs, Pods still land on it and fail at CNI ADD. If the Pod crashes without a `DEL`, the claim stays until the stale file is removed. Use it for single-node labs, bare-metal edge boxes, or when you do not want a Device Plugin.
- Pods in `pool` mode should not also request a Device Plugin resource for the same VFs: the two allocators know nothing about each other.

---

## 4. Choosing a combination

| Situation | Suggested setting |
|---|---|
| Existing Multus + Device Plugin cluster | keep defaults (`multus` + `injected`) |
| Want to drop Multus, keep scheduler-aware VF counting | `chained` + `podresources` |
| Lab / single node, no Device Plugin, no Multus | `chained` + `pool` |
| Multus is fine but you do not want the NAD to carry `resourceName` | `multus` + `podresources` |

## 5. What you give up compared with Multus

- Only SR-IOV can be a secondary NIC in chained mode; Multus can chain any CNI.
- One secondary NIC per Pod.
- The `k8s.v1.cni.cncf.io/network-status` annotation is not written.
- Every node needs an API token and a rotation story.
- You maintain the conflist yourself, and editing it affects all Pods on the node.

## 6. Verifying

```bash
go test ./...        # pool, podresources (fake kubelet gRPC server), k8s (fake API server), chain merge
```

On a node with real hardware, install a conflist from `deploy/05` or `deploy/06`, apply `deploy/08-pod-chained.yaml`, then check:

```bash
kubectl exec test-chained-podresources -- ip -br addr     # eth0 + net1
tail /var/log/sriov-cni.log                               # "chained: pod default/... network "vlan1000" -> net1"
ls /var/lib/cni/sriov/pool                                # (pool mode) one file per claimed VF
```

The paths that need real hardware (moving the VF into the Pod, IPAM, DEL) are the same as in multus mode; the chained and allocator logic above them is covered by the unit tests.
