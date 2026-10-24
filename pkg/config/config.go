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
)

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

	return n, n.validate()
}

func (n *NetConf) validate() error {
	if n.DeviceID == "" && (n.PFName == "" || n.VFID < 0) {
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
