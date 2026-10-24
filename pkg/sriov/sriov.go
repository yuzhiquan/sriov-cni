//go:build linux
// +build linux

// Package sriov 封装了 SR-IOV VF 的"发现"与"硬件属性配置"两件事。
//
// 关键点：VF 的 MAC/VLAN/spoofchk/trust/link-state 这些属性，
// 并不是 VF 自己 netdev 上的属性，而是由 **PF 驱动** 通过
// netlink 的 IFLA_VF_* 属性写进网卡硬件的（对应命令
// `ip link set <pf> vf <id> mac ... vlan ... spoofchk ... trust ...`）。
// 这正是 SR-IOV CNI 不能直接复用 host-device CNI 的原因。
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

// sysBusPci 是 PCI 设备拓扑的入口。
// 声明为变量是为了单元测试能用假 sysfs 树替换它。
var (
	sysBusPci   = "/sys/bus/pci/devices"
	sysClassNet = "/sys/class/net"
)

// VFInfo 描述一个 VF 在 PCI 拓扑与 netdev 层面的全部定位信息。
type VFInfo struct {
	// DeviceID 是 VF 自己的 PCI 地址，如 0000:03:02.0
	DeviceID string
	// PFBDF 是 VF 所属 PF 的 PCI 地址，如 0000:03:00.0
	PFBDF string
	// PFName 是 PF 在 host 上的 netdev 名，如 ens1f0
	PFName string
	// VFID 是 VF 在 PF 下的序号（0..numvfs-1）
	VFID int
	// Netdev 是 VF 当前在 host netns 里的 netdev 名，如 ens1f0v3
	Netdev string
}

// String 用于日志输出。
func (v *VFInfo) String() string {
	return fmt.Sprintf("vf=%s (pf=%s/%s id=%d netdev=%s)",
		v.DeviceID, v.PFBDF, v.PFName, v.VFID, v.Netdev)
}

// GetVFInfo 根据 DeviceID（或 pfName+vfID）解析出 VF 的完整拓扑信息。
//
// 实现上走的是 /sys 文件系统：
//   - /sys/bus/pci/devices/<vf>/physfn        -> 指向 PF 的软链接
//   - /sys/bus/pci/devices/<pf>/virtfn<N>     -> 指向第 N 个 VF 的软链接
//   - /sys/bus/pci/devices/<bdf>/net/<name>   -> 该 PCI 设备对应的 netdev
func GetVFInfo(conf *config.NetConf) (*VFInfo, error) {
	deviceID := conf.DeviceID

	// 允许用 pfName + vfID 定位（没有 Device Plugin 时的独立测试路径）
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

	// 1) 找到 VF 对应的 netdev（VF 必须已绑定内核驱动才有 netdev）
	netdev, err := netDevForPci(deviceID)
	if err != nil {
		return nil, err
	}

	// 2) 通过 physfn 软链接找到 PF
	pfBDF, err := filepath.EvalSymlinks(filepath.Join(vfDir, "physfn"))
	if err != nil {
		return nil, fmt.Errorf("sriov: %s is not a VF (no physfn link): %v", deviceID, err)
	}
	pfBDF = filepath.Base(pfBDF)

	pfName, err := netDevForPci(pfBDF)
	if err != nil {
		return nil, fmt.Errorf("sriov: cannot find PF netdev for %s: %v", pfBDF, err)
	}

	// 3) 扫描 PF 的 virtfnN 软链接，反查出 VF 的序号
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

// GetNetdevByPci 返回某个 PCI 地址对应的 netdev 名。
func GetNetdevByPci(pciAddr string) (string, error) {
	return netDevForPci(pciAddr)
}

// findVfID 遍历 /sys/bus/pci/devices/<pf>/virtfn*，找出指向 target 的那个 N。
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

// netDevForPci 读取 /sys/bus/pci/devices/<bdf>/net/ 目录下的第一个条目。
// 注意：一个 PCI 设备理论上可能对应多个 netdev（少见），这里取第一个。
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

// vfNetdevByID 通过 /sys/class/net/<pf>/device/virtfn<N> 找到第 N 个 VF 的 netdev。
func vfNetdevByID(pfBDF string, vfID int) (string, error) {
	link, err := filepath.EvalSymlinks(
		filepath.Join(sysBusPci, pfBDF, fmt.Sprintf("virtfn%d", vfID)))
	if err != nil {
		return "", fmt.Errorf("sriov: no virtfn%d on %s: %v", vfID, pfBDF, err)
	}
	return netDevForPci(filepath.Base(link))
}

// pciAddrForNetdev 通过 /sys/class/net/<name>/device 反查 PCI 地址。
func pciAddrForNetdev(name string) (string, error) {
	link, err := filepath.EvalSymlinks(filepath.Join(sysClassNet, name, "device"))
	if err != nil {
		return "", fmt.Errorf("sriov: netdev %s has no PCI device: %v", name, err)
	}
	return filepath.Base(link), nil
}
