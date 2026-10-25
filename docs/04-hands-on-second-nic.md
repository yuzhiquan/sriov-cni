# 04 · Hands-On: Giving a Pod a Second NIC and a Second IP

## Goal

```bash
$ kubectl exec -it test-sriov -- ip -br addr
lo               UNKNOWN        127.0.0.1/8
eth0@if23        UP             10.244.1.5/24        # ← primary NIC, Calico, overlay
net1             UP             192.168.100.10/24    # ← second NIC, SR-IOV VF, underlay passthrough
```

---

## Stage 1: Node Preparation (once per node)

### 1.1 BIOS

| Option | Value |
|---|---|
| `SR-IOV Support` | Enabled |
| `VT-d` / `AMD IOMMU` | Enabled |
| `Above 4G Decoding` | **Enabled** (required for large BARs; if you forget it, VFs cannot be created) |
| `PCIe ACS` | Enabled (if the option exists; it affects IOMMU group isolation) |

### 1.2 Kernel Parameters

```bash
# /etc/default/grub
GRUB_CMDLINE_LINUX_DEFAULT="... intel_iommu=on iommu=pt"
# On AMD use amd_iommu=on iommu=pt
# Note: netdevice mode alone does not actually need the IOMMU, but enabling it is harmless and keeps the door open for DPDK

sudo update-grub && sudo reboot
```

Verify:

```bash
dmesg | grep -i -E "DMAR: Intel-IOMMU enabled|AMD-Vi"
```

### 1.3 Confirm the NIC Supports SR-IOV

```bash
$ lspci -vvv -s 03:00.0 | grep -i -A3 "single root"
	Capabilities: [180 v1] Single Root I/O Virtualization (SR-IOV)
		IOVCap:	Migration-, Interrupt Message Number: 000
		IOVCtl:	Enable+ Migration- Interrupt- MSE+ ARIHierarchy+
		IOVSta:	Migration-
```

### 1.4 Create VFs

```bash
PF=ens1f0
echo 8 | sudo tee /sys/class/net/$PF/device/sriov_numvfs

$ ip link show $PF
5: ens1f0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 ...
    vf 0     MAC 00:00:00:00:00:00, spoof checking on, link-state auto, trust off
    vf 1     MAC 00:00:00:00:00:00, spoof checking on, link-state auto, trust off
    ...
```

### 1.5 Persist the VFs

`sriov_numvfs` resets to zero after a reboot. Pick either approach:

```bash
# Option A: systemd unit (works everywhere)
cat <<'EOF' | sudo tee /etc/systemd/system/sriov-vfs.service
[Unit]
Description=Create SR-IOV VFs for ens1f0
After=network-pre.target

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo 8 > /sys/class/net/ens1f0/device/sriov_numvfs'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl enable --now sriov-vfs
```

```bash
# Option B: NetworkManager keyfile
nmcli connection modify ens1f0 sriov.total-vfs 8
nmcli connection up ens1f0
```

### 1.6 ⚠️ Keep Network Management Tools from Grabbing the VFs

**This is the most common source of failures in production**: NetworkManager / netplan will manage VF netdevs like ordinary NICs, which leaves the CNI unable to move them.

```bash
# NetworkManager: mark the VFs as unmanaged
cat <<'EOF' | sudo tee /etc/NetworkManager/conf.d/10-sriov-vf-unmanaged.conf
[keyfile]
unmanaged-devices=interface-name:ens1f0v*;interface-name:ens1f1v*
EOF
sudo systemctl reload NetworkManager

# Confirm
nmcli device status | grep ens1f0v
# ens1f0v0   unmanaged  --
```

You can also use the script in the repository:

```bash
sudo ./deploy/setup-vfs.sh --pf ens1f0 --numvfs 8
```

---

## Stage 2: Cluster Components

### 2.1 Multus

```bash
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/master/deployments/multus-daemonset-thick.yml
```

Verify:

```bash
kubectl -n kube-system get pods -l app=multus
ls /opt/cni/bin/multus /opt/cni/bin/multus-shim
```

### 2.2 SR-IOV Device Plugin

ConfigMap (this repository: [`deploy/01-device-plugin-cm.yaml`](../deploy/01-device-plugin-cm.yaml) · [`deploy/02-nad-sriov-vlan1000.yaml`](../deploy/02-nad-sriov-vlan1000.yaml) · [`deploy/03-pod-dual-nic.yaml`](../deploy/03-pod-dual-nic.yaml)):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: sriovdp-config
  namespace: kube-system
data:
  config.json: |
    {
      "resourceList": [
        {
          "resourcePrefix": "intel.com",
          "resourceName": "intel_sriov_netdevice",
          "deviceType": "netdevice",
          "selectors": {
            "vendors": ["8086"],
            "devices": ["1889"],
            "drivers": ["iavf"],
            "pfNames": ["ens1f0"]
          }
        },
        {
          "resourcePrefix": "intel.com",
          "resourceName": "intel_sriov_dpdk",
          "deviceType": "vfio",
          "selectors": {
            "vendors": ["8086"],
            "devices": ["1889"],
            "drivers": ["vfio-pci"],
            "pfNames": ["ens1f1"]
          }
        }
      ]
    }
```

> Full resource name = `<resourcePrefix>/<resourceName>`; the default prefix is `intel.com`.
> Mellanox cards commonly use `mellanox.com` + `mlnx_sriov_netdevice`.

Use the official manifest for the DaemonSet:

```bash
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/sriov-network-device-plugin/master/deployments/k8s-v1.16/sriovdp-daemonset.yaml
```

### 2.3 Install the sriov-cni Binary

On every node:

```bash
sudo cp /tmp/sriov /opt/cni/bin/sriov && sudo chmod +x /opt/cni/bin/sriov
```

### 2.4 IPAM (whereabouts)

```bash
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/whereabouts/master/doc/crds/daemonset-install.yaml
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/whereabouts/master/doc/crds/whereabouts.cni.cncf.io_ippools.yaml
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/whereabouts/master/doc/crds/whereabouts.cni.cncf.io_overlappingrangeipreservations.yaml
```

### 2.5 Confirm the Resource Is Reported

```bash
$ kubectl get node worker-1 -o jsonpath='{.status.allocatable}' | jq
{
  "cpu": "48",
  "intel.com/intel_sriov_netdevice": "8",     # ← there it is
  "memory": "256Gi",
  "pods": "110"
}
```

---

## Stage 3: Define the Second Network (NetworkAttachmentDefinition)

[`deploy/02-nad-sriov-vlan1000.yaml`](../deploy/02-nad-sriov-vlan1000.yaml)

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: sriov-vlan1000
  namespace: default
  annotations:
    # ★ Key: tells Multus which extended resource this network needs,
    #   so Multus can fetch the allocated deviceID from the PodResources API
    k8s.v1.cni.cncf.io/resourceName: intel.com/intel_sriov_netdevice
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "type": "sriov",
      "vlan": 1000,
      "spoofchk": "off",
      "trust": "on",
      "link_state": "enable",
      "ipam": {
        "type": "whereabouts",
        "range": "192.168.100.0/24",
        "range_start": "192.168.100.10",
        "range_end": "192.168.100.200",
        "gateway": "192.168.100.1",
        "routes": [
          { "dst": "192.168.100.0/24" }
        ]
      }
    }
```

Field reference:

| Field | Purpose |
|---|---|
| `type: sriov` | Invokes `/opt/cni/bin/sriov` |
| `deviceID` | **Leave it out** -- Multus injects it (if you set it, it gets overwritten; only for testing) |
| `vlan` | Tag applied in hardware; the Pod sees untagged traffic |
| `spoofchk: off` | Allows the CNI to change the MAC |
| `trust: on` | Allows the VF to change its MAC / use promiscuous mode |
| `ipam.gateway` | If `""`, no default route is added (safer for multi-NIC Pods; see below) |

---

## Stage 4: ★ Key Issue -- Default Route Conflict

This is where "dual-NIC Pods" most often go wrong.

### The Problem

```bash
# Inside the Pod
$ ip route
default via 169.254.1.1 dev eth0      # Calico's default route
default via 192.168.100.1 dev net1    # IPAM adds another one ← conflict!
```

With two default routes, which one outbound traffic takes depends on the metric, and the result is **unpredictable**. Common symptoms:

- Intermittent failures reaching Services / DNS
- Packets sent out of `net1` never return (asymmetric routing)
- The application binds to net1's IP but traffic leaves via eth0 and is rejected by the peer

### Three Solutions

**Solution 1: Don't configure a gateway on net1 (simplest)**

```json
"ipam": {
  "type": "whereabouts",
  "range": "192.168.100.0/24",
  "gateway": ""          // leave empty → no default route is added
}
```

- Only the link route for `192.168.100.0/24` exists
- Reaching other subnets requires adding routes manually in the Pod, or having the application **bind explicitly to the NIC**
- **Best for**: "dedicated data plane" scenarios such as NFV, 5G UPF, databases, and RDMA

**Solution 2: Policy routing (recommended, natively supported by this plugin)**

```json
"ipam": {
  "type": "whereabouts",
  "range": "192.168.100.0/24",
  "gateway": "192.168.100.1",
  "routes": [
    { "dst": "0.0.0.0/0", "gw": "192.168.100.1", "table": 100 }
  ]
}
```

The plugin **automatically** installs an additional rule:

```bash
ip rule add from 192.168.100.10/32 table 100 pref 1100
```

Effect:

- Default outbound traffic still goes through `eth0` (cluster networking keeps working)
- **Traffic whose source address is 192.168.100.10** goes through `net1`
- The application only needs to `bind(192.168.100.10)` to automatically use the second NIC

**Solution 3: Manual configuration in an initContainer (fallback)**

Requires the `NET_ADMIN` capability:

```yaml
initContainers:
- name: net1-routes
  image: alpine
  securityContext:
    capabilities:
      add: ["NET_ADMIN"]
  command:
  - sh
  - -c
  - |
    IP=$(ip -4 -o addr show net1 | awk '{print $4}' | cut -d/ -f1)
    ip route add 192.168.100.0/24 dev net1 src $IP table 100
    ip route add default via 192.168.100.1 dev net1 table 100
    ip rule add from $IP/32 table 100 pref 1100
```

---

## Stage 5: Create the Pod

[`deploy/03-pod-dual-nic.yaml`](../deploy/03-pod-dual-nic.yaml)

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-sriov
  namespace: default
  annotations:
    k8s.v1.cni.cncf.io/networks: |
      [{"name":"sriov-vlan1000","namespace":"default","interface":"net1"}]
spec:
  containers:
  - name: app
    image: alpine:3.19
    command: ["sleep","infinity"]
    resources:
      requests:
        intel.com/intel_sriov_netdevice: 1
      limits:
        intel.com/intel_sriov_netdevice: 1     # ★ Must be declared, and the name must match the NAD annotation
```

> **Three names that must line up**:
> 1. `resourcePrefix` + `resourceName` in the Device Plugin ConfigMap
> 2. The NAD annotation `k8s.v1.cni.cncf.io/resourceName`
> 3. The Pod's `resources.limits`
>
> If any one of them is inconsistent, either scheduling fails or the deviceID cannot be obtained → CNI ADD returns an error.

---

## Stage 6: Verification

### 6.1 Check the NICs

```bash
$ kubectl exec -it test-sriov -- ip -br link
lo               UNKNOWN        00:00:00:00:00:00 <LOOPBACK,UP,LOWER_UP>
eth0@if23        UP             6a:8b:2c:...      <BROADCAST,MULTICAST,UP,LOWER_UP>
net1             UP             02:11:22:33:44:55 <BROADCAST,MULTICAST,UP,LOWER_UP>
```

### 6.2 Check the IP

```bash
$ kubectl exec -it test-sriov -- ip -br addr show net1
net1             UP             192.168.100.10/24
```

### 6.3 Confirm It Is an SR-IOV VF (Not an Ordinary Virtual NIC)

```bash
$ kubectl exec -it test-sriov -- sh -c 'ethtool -i net1'
driver: iavf                       # ← Intel Adaptive VF
version: 4.9.x
bus-info: 0000:03:02.1             # ← real PCI address
```

```bash
# You can also see the VF's PCI topology
$ kubectl exec -it test-sriov -- sh -c 'ls /sys/class/net/net1/device/'
iommu_group  physfn  sriov_vf_device  ...
```

### 6.4 Check the Network Status Written to the Annotations

```bash
$ kubectl get pod test-sriov -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/networks-status}' | jq
[
  {
    "name": "k8s-pod-network",
    "interface": "eth0",
    "ips": ["10.244.1.5"],
    "default": true
  },
  {
    "name": "default/sriov-vlan1000",
    "interface": "net1",
    "ips": ["192.168.100.10"],
    "device-info": {
      "type": "pci",
      "version": "1.0.0",
      "pci": { "pci-address": "0000:03:02.1" }
    }
  }
]
```

### 6.5 Verify Policy Routing

```bash
$ kubectl exec -it test-sriov -- ip rule
0:	from all lookup local
1100:	from 192.168.100.10 lookup 100       # ← installed automatically
32766:	from all lookup main
32767:	from all lookup default

$ kubectl exec -it test-sriov -- ip route show table 100
default via 192.168.100.1 dev net1
```

### 6.6 Connectivity Test

```bash
# Ping with an explicit source address
$ kubectl exec -it test-sriov -- ping -I 192.168.100.10 -c 3 192.168.100.1
PING 192.168.100.1 (192.168.100.1): 56 data bytes
64 bytes from 192.168.100.1: seq=0 ttl=64 time=0.18 ms

# Confirm it goes out via net1
$ kubectl exec -it test-sriov -- ip route get 192.168.100.1 from 192.168.100.10
192.168.100.1 from 192.168.100.10 dev net1 table 100 uid 0
```

### 6.7 Confirm on the Host That the VF Is In Use

```bash
$ ip link show ens1f0
5: ens1f0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 ...
    vf 0     MAC 02:11:22:33:44:55, vlan 1000, spoof checking off, link-state enable, trust on
    vf 1     MAC 00:00:00:00:00:00, spoof checking on, link-state auto, trust off
```

`vf 0` now carries the MAC + VLAN we configured, which shows the configuration took effect.

---

## Stage 7: Making the Application Actually Use the Second IP

Getting the NIC is only the first step; you still have to make traffic take the right path:

| Layer | Approach |
|---|---|
| **Socket binding** | `bind(192.168.100.10)` + this plugin's policy routing → automatically goes out via net1 |
| **Force device binding** | `setsockopt(SOL_SOCKET, SO_BINDTODEVICE, "net1")` |
| **HTTP/clients** | Set `local_address` / `source_address` |
| **curl** | `curl --interface 192.168.100.10 http://...` |
| **Go** | `net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("192.168.100.10")}}` |
| **Whole-container isolation** | Network namespace / VRF |

### Required Kernel Parameters

With multiple NICs and asymmetric routing, reverse path filtering will **silently drop packets**:

```bash
# /etc/sysctl.d/99-sriov-multinic.conf
net.ipv4.conf.all.rp_filter=2        # 2 = loose mode (recommended); 0 = disabled
net.ipv4.conf.default.rp_filter=2
net.ipv4.conf.all.arp_filter=1       # prevent ARP flux
net.ipv4.conf.all.arp_announce=2
net.ipv4.conf.all.arp_ignore=1
```

```bash
sudo sysctl --system
```

> `rp_filter=1` (strict mode) is the number one killer in multi-NIC setups: when a reply packet arrives on a different NIC, the kernel drops it outright, and **leaves no log at all**.

---

**Previous**: [03 · Writing a SR-IOV CNI](./03-writing-a-sriov-cni.md) · **Next**: [05 · Troubleshooting and Performance Tuning](./05-troubleshooting-and-tuning.md)