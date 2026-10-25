//go:build linux
// +build linux

// Package config 负责解析 sriov-cni 的网络配置（stdin 传入的 JSON）。
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/example/sriov-cni/pkg/k8s"
	"github.com/example/sriov-cni/pkg/pool"
)

// 运行模式（Mode）。
const (
	// ModeMultus（默认）：由 Multus 按 NAD 调用本插件，本插件只负责这一块 VF。
	ModeMultus = "multus"
	// ModeChained：本插件作为 conflist 里主 CNI 之后的链式插件，
	// 自己读 Pod 注解决定要不要、挂哪个副网卡，不依赖 Multus。
	ModeChained = "chained"
)

// VF 来源（VFAllocator）：决定"这个 Pod 用哪块 VF"由谁说了算。
const (
	// AllocInjected（默认）：deviceID 由配置 / Multus / 环境变量给出。
	AllocInjected = "injected"
	// AllocPodResources：向 kubelet PodResources API 查 Device Plugin 的分配结果。
	AllocPodResources = "podresources"
	// AllocPool：用节点本地 VF 池自行分配，不需要 Device Plugin。
	AllocPool = "pool"
)

// DefaultSecondaryIfName 是 chained 模式下副网卡在 Pod 内的默认名字。
const DefaultSecondaryIfName = "net1"

// DefaultAnnotation 是 chained 模式下用来选择网络的 Pod 注解。
const DefaultAnnotation = "sriov.example.com/network"

// PodResourcesConf 配置 VF 来源 podresources。
type PodResourcesConf struct {
	// Socket 默认 /var/lib/kubelet/pod-resources/kubelet.sock。
	Socket string `json:"socket"`
	// Resource 是 Device Plugin 的资源名，如 intel.com/intel_sriov_netdevice。
	Resource string `json:"resource"`
	// Index 当 Pod 在该资源下有多块设备时，取第几块（默认 0）。
	Index int `json:"index"`
	// TimeoutMs 等待分配结果出现的最长时间，默认 3000。
	TimeoutMs int `json:"timeoutMs"`
}

// NetConf 是 sriov-cni 的完整配置结构。
//
// 它和普通 CNI（bridge/ptp）最大的不同在于：
// 普通 CNI 要"凭空造一张网卡"（veth pair + 网桥），而 sriov 是
// "接管一块真实存在、且已经被 Device Plugin 分配给这个 Pod 的 PCIe 设备"。
// 因此它必须先拿到 DeviceID（PCI 地址），再去改硬件属性、搬 netns。
type NetConf struct {
	types.NetConf

	// ---------- 定位 VF ----------

	// DeviceID 是分配给该 Pod 的 VF 的 PCI 地址，形如 "0000:03:02.0"。
	// 生产环境由 Multus + SR-IOV Device Plugin 自动注入，通常不需要手写。
	DeviceID string `json:"deviceID"`
	// VFID 是 VF 在 PF 下的序号。留空则自动从 PCI 拓扑推算。
	VFID int `json:"vfID"`
	// PFName 是 PF 的 netdev 名（如 ens1f0）。自动推算失败时兜底。
	PFName string `json:"pfName"`

	// ---------- VF 硬件属性（通过 PF 的 netlink 接口写进网卡硬件） ----------

	// MAC 会同时写入 VF netdev 与网卡的 MAC 过滤表。
	// 注意：要能改 MAC，spoofchk 必须为 off。
	MAC string `json:"mac"`
	// Vlan / VlanQoS：由网卡硬件做 VLAN tag 的插入/剥离（tx/rx 对称）。
	// Vlan=0 表示不设置（保留 VF 原有配置）。
	Vlan    int `json:"vlan"`
	VlanQoS int `json:"vlanQoS"`
	// SpoofChk: "on" | "off"。防 MAC/IP 欺骗，默认网卡是 on。
	SpoofChk string `json:"spoofchk"`
	// Trust: "on" | "off"。VF 是否可信（能否开混杂模式、改多播、改 MAC 等）。
	Trust string `json:"trust"`
	// LinkState: "auto" | "enable" | "disable"。
	LinkState string `json:"link_state"`
	// MinTxRate / MaxTxRate：VF 发送限速，单位 Mbps，需要网卡支持。
	MinTxRate int `json:"min_tx_rate"`
	MaxTxRate int `json:"max_tx_rate"`

	// ---------- 运行模式 ----------

	// DPDKMode=true 表示 VF 已被绑定到 vfio-pci，由 DPDK PMD 在用户态驱动。
	// 此时 VF 不产生 netdev，不能搬进 Pod netns，CNI 只需把 PCI 地址回传给运行时。
	DPDKMode bool `json:"dpdk"`

	// MTU 可选；0 表示沿用 VF netdev 当前 MTU。
	MTU int `json:"mtu"`

	// ---------- 无 Multus 方案：模式与 VF 来源 ----------

	// Mode: "multus"（默认）| "chained"。见 ModeChained。
	Mode string `json:"mode"`
	// VFAllocator: "injected"（默认）| "podresources" | "pool"。
	// 两种模式都可以搭配任何一种来源。
	VFAllocator string `json:"vfAllocator"`
	// PodResources / Pool 是对应来源的参数。
	PodResources *PodResourcesConf `json:"podResources,omitempty"`
	Pool         *pool.Conf        `json:"pool,omitempty"`

	// 以下仅 chained 模式使用。

	// Kubernetes 描述如何访问 API server 读取 Pod 注解。
	Kubernetes *k8s.Conf `json:"kubernetes,omitempty"`
	// Annotation 是选择网络的 Pod 注解键，默认 DefaultAnnotation。
	Annotation string `json:"annotation,omitempty"`
	// Networks 是"网络名 -> 覆盖配置"，注解的值用来在这里查表。
	// 覆盖配置的任意顶层字段（vlan、ipam、resource…）会盖掉基础配置。
	Networks map[string]json.RawMessage `json:"networks,omitempty"`
	// SecondaryIfName 是副网卡在 Pod 内的名字，默认 net1。
	SecondaryIfName string `json:"secondaryIfName,omitempty"`

	// ---------- 运行时注入 ----------

	// RuntimeConfig 是 Multus 注入设备信息的另一种位置
	// （{"runtimeConfig": {"deviceID": "...", "mac": "..."}}）。
	RuntimeConfig struct {
		DeviceID string `json:"deviceID"`
		Mac      string `json:"mac"`
	} `json:"runtimeConfig,omitempty"`

	// ---------- 插件自身 ----------

	LogLevel string `json:"logLevel"`
	LogFile  string `json:"logFile"`
	// ResetOnDel 控制 DEL 时是否把 VF 恢复原状，默认 true。
	ResetOnDel *bool `json:"resetOnDel"`

	// ContainerID / OrigVfState 是插件内部状态，会缓存到磁盘供 DEL 使用。
	ContainerID string      `json:"containerID,omitempty"`
	OrigVfState *VfState    `json:"origVfState,omitempty"`
	Netdev      string      `json:"netdev,omitempty"`
	PFBDF       string      `json:"pfBDF,omitempty"`
	Extra       interface{} `json:"-"`
	// StdinData 缓存 ADD 时实际传给 IPAM 的配置（chained 模式合并后的版本），
	// DEL 时 runtime 给的是未合并的 conflist，需要它才能正确归还 IP。
	StdinData json.RawMessage `json:"stdinData,omitempty"`
}

// VfState 记录 VF 被 CNI 接管之前的硬件属性，DEL 时用于还原。
// 这一层"还原"非常关键：VF 是稀缺的硬件资源，如果 Pod 删除后
// VLAN/spoofchk 停留在上一个 Pod 的配置，下一个 Pod 就会拿到脏设备。
type VfState struct {
	HostIFName   string `json:"hostIFName"` // VF 在 host netns 里的 netdev 名
	EffectiveMAC string `json:"effectiveMAC"`
	AdminMAC     string `json:"adminMAC"`
	Vlan         int    `json:"vlan"`
	VlanQoS      int    `json:"vlanQoS"`
	SpoofChk     bool   `json:"spoofchk"`
	Trust        bool   `json:"trust"`
	LinkState    string `json:"linkState"`
	MinTxRate    int    `json:"minTxRate"`
	MaxTxRate    int    `json:"maxTxRate"`
	MTU          int    `json:"mtu"`
	LinkUp       bool   `json:"linkUp"`
}

// ResetOnDelOrDefault 返回 DEL 是否还原 VF 配置。
func (n *NetConf) ResetOnDelOrDefault() bool {
	if n.ResetOnDel == nil {
		return true
	}
	return *n.ResetOnDel
}

// LoadConf 解析 CNI 配置 JSON。
func LoadConf(data []byte) (*NetConf, error) {
	n := &NetConf{}
	if err := json.Unmarshal(data, n); err != nil {
		return nil, fmt.Errorf("sriov: failed to load netconf: %v", err)
	}

	// 兼容两种注入位置：顶层 deviceID，或 runtimeConfig.deviceID
	if n.DeviceID == "" {
		n.DeviceID = n.RuntimeConfig.DeviceID
	}
	if n.MAC == "" {
		n.MAC = n.RuntimeConfig.Mac
	}

	// 兜底：k8s 会把 Device Plugin 分配的设备以 PCIDEVICE_<resource> 环境变量
	// 注入容器，独立测试（非 Multus）时可以用它定位 VF。
	if n.DeviceID == "" {
		n.DeviceID = deviceIDFromEnv()
	}

	// chained 模式需要 prevResult（主 CNI 的结果）。
	if n.Mode == ModeChained && n.RawPrevResult != nil {
		if err := version.ParsePrevResult(&n.NetConf); err != nil {
			return nil, fmt.Errorf("sriov: invalid prevResult: %v", err)
		}
	}

	switch n.Mode {
	case "", ModeMultus, ModeChained:
	default:
		return nil, fmt.Errorf("sriov: invalid mode %q, expect multus|chained", n.Mode)
	}
	switch n.VFAllocator {
	case "", AllocInjected, AllocPodResources, AllocPool:
	default:
		return nil, fmt.Errorf("sriov: invalid vfAllocator %q, expect injected|podresources|pool", n.VFAllocator)
	}

	return n, n.validate()
}

// Validate 在 deviceID 被 podresources/pool 解析出来之后重新校验。
func (n *NetConf) Validate() error { return n.validate() }

// DeferDevice 表示此刻还没有 deviceID 是正常的：
// 要么稍后由 podresources/pool 解析，要么 chained 外壳要等合并网络配置后才知道。
func (n *NetConf) DeferDevice() bool {
	return n.Mode == ModeChained ||
		n.VFAllocator == AllocPodResources || n.VFAllocator == AllocPool
}

// SecondaryIfNameOrDefault 返回 chained 模式下副网卡的名字。
func (n *NetConf) SecondaryIfNameOrDefault() string {
	if n.SecondaryIfName != "" {
		return n.SecondaryIfName
	}
	return DefaultSecondaryIfName
}

// AnnotationOrDefault 返回选择网络的注解键。
func (n *NetConf) AnnotationOrDefault() string {
	if n.Annotation != "" {
		return n.Annotation
	}
	return DefaultAnnotation
}

func (n *NetConf) validate() error {
	if n.DeviceID == "" && !n.DeferDevice() && (n.PFName == "" || n.VFID < 0) {
		return fmt.Errorf(`sriov: missing "deviceID" (or "pfName"+"vfID"); ` +
			`it is normally injected by Multus from the SR-IOV Device Plugin`)
	}
	if n.DeviceID != "" && !pciAddrRe.MatchString(n.DeviceID) {
		return fmt.Errorf("sriov: invalid PCI address %q, expect 0000:03:02.0", n.DeviceID)
	}
	if n.MAC != "" {
		hw, err := net.ParseMAC(n.MAC)
		if err != nil {
			return fmt.Errorf("sriov: invalid mac %q: %v", n.MAC, err)
		}
		// 归一化，避免大小写差异导致比对失败
		n.MAC = hw.String()
	}
	if n.Vlan < 0 || n.Vlan > 4094 {
		return fmt.Errorf("sriov: invalid vlan %d, must be 0..4094", n.Vlan)
	}
	if n.VlanQoS < 0 || n.VlanQoS > 7 {
		return fmt.Errorf("sriov: invalid vlanQoS %d, must be 0..7", n.VlanQoS)
	}
	if n.SpoofChk != "" {
		if _, err := parseOnOff("spoofchk", n.SpoofChk); err != nil {
			return err
		}
	}
	if n.Trust != "" {
		if _, err := parseOnOff("trust", n.Trust); err != nil {
			return err
		}
	}
	if n.LinkState != "" {
		if _, err := parseLinkState(n.LinkState); err != nil {
			return err
		}
	}
	return nil
}

// parseOnOff 把 "on"/"off"/"true"/"false" 解析成 bool。
func parseOnOff(name, v string) (bool, error) {
	switch strings.ToLower(v) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("sriov: invalid %s value %q, expect on|off", name, v)
}

// deviceIDFromEnv 从 PCIDEVICE_* 环境变量里取第一个 PCI 地址。
// 例如 PCIDEVICE_INTEL_COM_INTEL_SRIOV_NETDEVICE="0000:03:02.0,0000:03:02.1"
func deviceIDFromEnv() string {
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PCIDEVICE_") {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		for _, addr := range strings.Split(parts[1], ",") {
			addr = strings.TrimSpace(addr)
			if pciAddrRe.MatchString(addr) {
				return addr
			}
		}
	}
	return ""
}
