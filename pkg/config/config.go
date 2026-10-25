//go:build linux
// +build linux

// Package config parses the sriov-cni network configuration (JSON passed via stdin).
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

// Run mode (Mode).
const (
	// ModeMultus (default): Multus invokes this plugin per NAD; the plugin only handles that one VF.
	ModeMultus = "multus"
	// ModeChained: the plugin runs as a chained plugin after the primary CNI in a conflist,
	// reads Pod annotations to decide whether and which secondary NIC to attach, without relying on Multus.
	ModeChained = "chained"
)

// VF source (VFAllocator): decides who determines "which VF this Pod uses".
const (
	// AllocInjected (default): deviceID is provided by config / Multus / environment variable.
	AllocInjected = "injected"
	// AllocPodResources: query the kubelet PodResources API for the Device Plugin's allocation.
	AllocPodResources = "podresources"
	// AllocPool: allocate from a node-local VF pool, no Device Plugin required.
	AllocPool = "pool"
)

// DefaultSecondaryIfName is the default name of the secondary NIC inside the Pod in chained mode.
const DefaultSecondaryIfName = "net1"

// DefaultAnnotation is the Pod annotation used to select networks in chained mode.
const DefaultAnnotation = "sriov.example.com/network"

// PodResourcesConf configures the podresources VF source.
type PodResourcesConf struct {
	// Socket defaults to /var/lib/kubelet/pod-resources/kubelet.sock.
	Socket string `json:"socket"`
	// Resource is the Device Plugin resource name, e.g. intel.com/intel_sriov_netdevice.
	Resource string `json:"resource"`
	// Index selects which device to use when the Pod has several under this resource (default 0).
	Index int `json:"index"`
	// TimeoutMs is the maximum time to wait for the allocation to appear, default 3000.
	TimeoutMs int `json:"timeoutMs"`
}

// NetConf is the complete sriov-cni configuration structure.
//
// The biggest difference from ordinary CNIs (bridge/ptp) is that:
// ordinary CNIs must "create a NIC from scratch" (veth pair + bridge), whereas sriov
// "takes over a real PCIe device that the Device Plugin has already allocated to this Pod".
// So it must first obtain the DeviceID (PCI address), then modify hardware attributes and move it into the netns.
type NetConf struct {
	types.NetConf

	// ---------- Locating the VF ----------

	// DeviceID is the PCI address of the VF allocated to this Pod, e.g. "0000:03:02.0".
	// In production it is injected automatically by Multus + SR-IOV Device Plugin, so it usually need not be written by hand.
	DeviceID string `json:"deviceID"`
	// VFID is the VF's index under the PF. If empty, it is derived automatically from the PCI topology.
	VFID int `json:"vfID"`
	// PFName is the PF's netdev name (e.g. ens1f0). Used as a fallback when automatic derivation fails.
	PFName string `json:"pfName"`

	// ---------- VF hardware attributes (written to the NIC hardware via the PF's netlink interface) ----------

	// MAC is written to both the VF netdev and the NIC's MAC filter table.
	// Note: spoofchk must be off in order to change the MAC.
	MAC string `json:"mac"`
	// Vlan / VlanQoS: VLAN tag insertion/stripping is done by the NIC hardware (symmetric for tx/rx).
	// Vlan=0 means unset (keep the VF's existing configuration).
	Vlan    int `json:"vlan"`
	VlanQoS int `json:"vlanQoS"`
	// SpoofChk: "on" | "off". Guards against MAC/IP spoofing; NICs default to on.
	SpoofChk string `json:"spoofchk"`
	// Trust: "on" | "off". Whether the VF is trusted (may enable promiscuous mode, change multicast, change MAC, etc.).
	Trust string `json:"trust"`
	// LinkState: "auto" | "enable" | "disable".
	LinkState string `json:"link_state"`
	// MinTxRate / MaxTxRate: VF transmit rate limits in Mbps; requires NIC support.
	MinTxRate int `json:"min_tx_rate"`
	MaxTxRate int `json:"max_tx_rate"`

	// ---------- Run mode ----------

	// DPDKMode=true means the VF is bound to vfio-pci and driven by a DPDK PMD in user space.
	// The VF then has no netdev and cannot be moved into the Pod netns; the CNI only returns the PCI address to the runtime.
	DPDKMode bool `json:"dpdk"`

	// MTU is optional; 0 keeps the VF netdev's current MTU.
	MTU int `json:"mtu"`

	// ---------- Without Multus: mode and VF source ----------

	// Mode: "multus" (default) | "chained". See ModeChained.
	Mode string `json:"mode"`
	// VFAllocator: "injected" (default) | "podresources" | "pool".
	// Both modes can be combined with any source.
	VFAllocator string `json:"vfAllocator"`
	// PodResources / Pool are the parameters of the corresponding source.
	PodResources *PodResourcesConf `json:"podResources,omitempty"`
	Pool         *pool.Conf        `json:"pool,omitempty"`

	// The following are used only in chained mode.

	// Kubernetes describes how to access the API server to read Pod annotations.
	Kubernetes *k8s.Conf `json:"kubernetes,omitempty"`
	// Annotation is the Pod annotation key that selects the network, default DefaultAnnotation.
	Annotation string `json:"annotation,omitempty"`
	// Networks maps "network name -> override config"; the annotation value is looked up here.
	// Any top-level field of an override (vlan, ipam, resource...) overrides the base config.
	Networks map[string]json.RawMessage `json:"networks,omitempty"`
	// SecondaryIfName is the name of the secondary NIC inside the Pod, default net1.
	SecondaryIfName string `json:"secondaryIfName,omitempty"`

	// ---------- Runtime injection ----------

	// RuntimeConfig is an alternative location where Multus injects device info
	// ({"runtimeConfig": {"deviceID": "...", "mac": "..."}}).
	RuntimeConfig struct {
		DeviceID string `json:"deviceID"`
		Mac      string `json:"mac"`
	} `json:"runtimeConfig,omitempty"`

	// ---------- Plugin internals ----------

	LogLevel string `json:"logLevel"`
	LogFile  string `json:"logFile"`
	// ResetOnDel controls whether the VF is restored on DEL, default true.
	ResetOnDel *bool `json:"resetOnDel"`

	// ContainerID / OrigVfState are internal plugin state, cached to disk for use by DEL.
	ContainerID string      `json:"containerID,omitempty"`
	OrigVfState *VfState    `json:"origVfState,omitempty"`
	Netdev      string      `json:"netdev,omitempty"`
	PFBDF       string      `json:"pfBDF,omitempty"`
	Extra       interface{} `json:"-"`
	// StdinData caches the config actually passed to IPAM during ADD (the merged version in chained mode);
	// on DEL the runtime provides the unmerged conflist, so this is needed to release the IP correctly.
	StdinData json.RawMessage `json:"stdinData,omitempty"`
}

// VfState records the VF's hardware attributes before the CNI took it over, used to restore them on DEL.
// This "restore" step is critical: VFs are scarce hardware resources; if VLAN/spoofchk stay at the previous
// Pod's settings after the Pod is deleted, the next Pod gets a dirty device.
type VfState struct {
	HostIFName   string `json:"hostIFName"` // VF's netdev name in the host netns
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

// ResetOnDelOrDefault returns whether to restore the VF config on DEL.
func (n *NetConf) ResetOnDelOrDefault() bool {
	if n.ResetOnDel == nil {
		return true
	}
	return *n.ResetOnDel
}

// LoadConf parses the CNI config JSON.
func LoadConf(data []byte) (*NetConf, error) {
	n := &NetConf{}
	if err := json.Unmarshal(data, n); err != nil {
		return nil, fmt.Errorf("sriov: failed to load netconf: %v", err)
	}

	// Support both injection locations: top-level deviceID, or runtimeConfig.deviceID
	if n.DeviceID == "" {
		n.DeviceID = n.RuntimeConfig.DeviceID
	}
	if n.MAC == "" {
		n.MAC = n.RuntimeConfig.Mac
	}

	// Fallback: k8s injects the Device Plugin's allocated devices into the container via
	// PCIDEVICE_<resource> environment variables, which can locate the VF in standalone (non-Multus) tests.
	if n.DeviceID == "" {
		n.DeviceID = deviceIDFromEnv()
	}

	// Chained mode needs prevResult (the primary CNI's result).
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

// Validate re-validates after deviceID has been resolved by podresources/pool.
func (n *NetConf) Validate() error { return n.validate() }

// DeferDevice indicates that not having a deviceID yet is normal:
// either it will be resolved later by podresources/pool, or the chained shell only knows it after merging the network config.
func (n *NetConf) DeferDevice() bool {
	return n.Mode == ModeChained ||
		n.VFAllocator == AllocPodResources || n.VFAllocator == AllocPool
}

// SecondaryIfNameOrDefault returns the secondary NIC name in chained mode.
func (n *NetConf) SecondaryIfNameOrDefault() string {
	if n.SecondaryIfName != "" {
		return n.SecondaryIfName
	}
	return DefaultSecondaryIfName
}

// AnnotationOrDefault returns the annotation key that selects the network.
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
		// Normalize to avoid comparison failures caused by case differences
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

// parseOnOff parses "on"/"off"/"true"/"false" into a bool.
func parseOnOff(name, v string) (bool, error) {
	switch strings.ToLower(v) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("sriov: invalid %s value %q, expect on|off", name, v)
}

// deviceIDFromEnv takes the first PCI address from the PCIDEVICE_* environment variables.
// For example PCIDEVICE_INTEL_COM_INTEL_SRIOV_NETDEVICE="0000:03:02.0,0000:03:02.1"
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
