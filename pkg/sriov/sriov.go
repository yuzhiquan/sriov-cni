//go:build linux
// +build linux

// Package sriov wraps two jobs: VF "discovery" and VF "hardware attribute configuration".
//
// Key point: VF attributes such as MAC/VLAN/spoofchk/trust/link-state
// are not properties of the VF's own netdev. They are written into the NIC hardware by the **PF driver** via
// netlink IFLA_VF_* attributes (equivalent to the command
// `ip link set <pf> vf <id> mac ... vlan ... spoofchk ... trust ...`）。
// This is why SR-IOV CNI cannot simply reuse the host-device CNI.
package sriov

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"

	"github.com/example/sriov-cni/pkg/config"
)

// sysBusPci is the entry point of the PCI device topology.
// It is a variable so unit tests can replace it with a fake sysfs tree.
var (
	sysBusPci   = "/sys/bus/pci/devices"
	sysClassNet = "/sys/class/net"
)

// VFInfo describes everything needed to locate a VF in the PCI topology and at the netdev level.
type VFInfo struct {
	// DeviceID is the VF's own PCI address, e.g. 0000:03:02.0
	DeviceID string
	// PFBDF is the PCI address of the PF that owns the VF, e.g. 0000:03:00.0
	PFBDF string
	// PFName is the PF netdev name on the host, e.g. ens1f0
	PFName string
	// VFID is the VF index under its PF (0..numvfs-1)
	VFID int
	// Netdev is the VF's current netdev name in the host netns, e.g. ens1f0v3
	Netdev string
}

// String is used for log output.
func (v *VFInfo) String() string {
	return fmt.Sprintf("vf=%s (pf=%s/%s id=%d netdev=%s)",
		v.DeviceID, v.PFBDF, v.PFName, v.VFID, v.Netdev)
}

// GetVFInfo resolves the full topology of a VF from its DeviceID (or pfName+vfID).
//
// It works through the /sys filesystem:
//   - /sys/bus/pci/devices/<vf>/physfn        -> symlink to the PF
//   - /sys/bus/pci/devices/<pf>/virtfn<N>     -> symlink to the Nth VF
//   - /sys/bus/pci/devices/<bdf>/net/<name>   -> netdev of that PCI device
func GetVFInfo(conf *config.NetConf) (*VFInfo, error) {
	deviceID := conf.DeviceID

	// Allow locating by pfName + vfID (standalone test path when there is no Device Plugin)
	if deviceID == "" && conf.PFName != "" {
		link, err := netlink.LinkByName(conf.PFName)
		if err != nil {
			return nil, fmt.Errorf("sriov: cannot find PF %q: %v", conf.PFName, err)
		}
		pfBDF, err := pciAddrForNetdev(link.Attrs().Name)
		if err != nil {
			return nil, err
		}
		vfLink, err := vfNetdevByID(pfBDF, conf.VFID)
		if err != nil {
			return nil, err
		}
		deviceID, err = pciAddrForNetdev(vfLink)
		if err != nil {
			return nil, err
		}
	}

	if deviceID == "" {
		return nil, fmt.Errorf("sriov: no deviceID available")
	}

	vfDir := filepath.Join(sysBusPci, deviceID)
	if _, err := os.Lstat(vfDir); err != nil {
		return nil, fmt.Errorf("sriov: VF %s not found: %v", deviceID, err)
	}

	// 1) Find the VF's netdev (the VF must be bound to a kernel driver to have one)
	netdev, err := netDevForPci(deviceID)
	if err != nil {
		return nil, err
	}

	// 2) Find the PF via the physfn symlink
	pfBDF, err := filepath.EvalSymlinks(filepath.Join(vfDir, "physfn"))
	if err != nil {
		return nil, fmt.Errorf("sriov: %s is not a VF (no physfn link): %v", deviceID, err)
	}
	pfBDF = filepath.Base(pfBDF)

	pfName, err := netDevForPci(pfBDF)
	if err != nil {
		return nil, fmt.Errorf("sriov: cannot find PF netdev for %s: %v", pfBDF, err)
	}

	// 3) Scan the PF's virtfnN symlinks to look up the VF index
	vfID := conf.VFID
	if vfID <= 0 {
		vfID, err = findVfID(pfBDF, deviceID)
		if err != nil {
			return nil, err
		}
	}

	return &VFInfo{
		DeviceID: deviceID,
		PFBDF:    pfBDF,
		PFName:   pfName,
		VFID:     vfID,
		Netdev:   netdev,
	}, nil
}

// GetNetdevByPci returns the netdev name for a PCI address.
func GetNetdevByPci(pciAddr string) (string, error) {
	return netDevForPci(pciAddr)
}

// findVfID iterates /sys/bus/pci/devices/<pf>/virtfn* and finds the N that points to target.
func findVfID(pfBDF, target string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(sysBusPci, pfBDF, "virtfn*"))
	if err != nil {
		return -1, err
	}
	if len(matches) == 0 {
		return -1, fmt.Errorf("sriov: PF %s has no VFs (check sriov_numvfs)", pfBDF)
	}
	for _, m := range matches {
		resolved, err := filepath.EvalSymlinks(m)
		if err != nil {
			continue
		}
		if filepath.Base(resolved) != target {
			continue
		}
		name := filepath.Base(m) // virtfn3
		id, err := strconv.Atoi(strings.TrimPrefix(name, "virtfn"))
		if err != nil {
			return -1, fmt.Errorf("sriov: malformed %s", name)
		}
		return id, nil
	}
	return -1, fmt.Errorf("sriov: VF %s does not belong to PF %s", target, pfBDF)
}

// netDevForPci reads the first entry under /sys/bus/pci/devices/<bdf>/net/.
// Note: a PCI device may in theory map to multiple netdevs (rare); the first one is used here.
func netDevForPci(pciAddr string) (string, error) {
	netDir := filepath.Join(sysBusPci, pciAddr, "net")
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return "", fmt.Errorf("sriov: no netdev for %s (is the VF bound to a kernel driver?): %v",
			pciAddr, err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("sriov: no netdev for %s", pciAddr)
	}
	return entries[0].Name(), nil
}

// vfNetdevByID finds the netdev of the Nth VF via /sys/class/net/<pf>/device/virtfn<N>.
func vfNetdevByID(pfBDF string, vfID int) (string, error) {
	link, err := filepath.EvalSymlinks(
		filepath.Join(sysBusPci, pfBDF, fmt.Sprintf("virtfn%d", vfID)))
	if err != nil {
		return "", fmt.Errorf("sriov: no virtfn%d on %s: %v", vfID, pfBDF, err)
	}
	return netDevForPci(filepath.Base(link))
}

// pciAddrForNetdev looks up the PCI address via /sys/class/net/<name>/device.
func pciAddrForNetdev(name string) (string, error) {
	link, err := filepath.EvalSymlinks(filepath.Join(sysClassNet, name, "device"))
	if err != nil {
		return "", fmt.Errorf("sriov: netdev %s has no PCI device: %v", name, err)
	}
	return filepath.Base(link), nil
}
