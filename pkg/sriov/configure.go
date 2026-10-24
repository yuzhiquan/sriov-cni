//go:build linux
// +build linux

package sriov

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"

	"github.com/example/sriov-cni/pkg/config"
	"github.com/example/sriov-cni/pkg/logging"
)

// getLinkByName 通过 LinkList 查找网卡。
// 不用 netlink.LinkByName 是因为 VF 信息（Attrs().Vfs）只在
// 完整的 link dump（RTM_GETLINK 带 IFLA_VFINFO）里返回。
func getLinkByName(name string) (netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if l.Attrs().Name == name {
			return l, nil
		}
	}
	return nil, fmt.Errorf("link %s not found", name)
}

// SaveOrigVfState 采集 VF 被接管前的状态，用于 DEL 时还原。
func SaveOrigVfState(info *VFInfo) (*config.VfState, error) {
	vfLink, err := getLinkByName(info.Netdev)
	if err != nil {
		return nil, err
	}
	pfLink, err := getLinkByName(info.PFName)
	if err != nil {
		return nil, err
	}

	state := &config.VfState{
		HostIFName:   vfLink.Attrs().Name,
		EffectiveMAC: vfLink.Attrs().HardwareAddr.String(),
		MTU:          vfLink.Attrs().MTU,
		LinkUp:       vfLink.Attrs().Flags&net.FlagUp != 0,
	}

	for _, vf := range pfLink.Attrs().Vfs {
		if vf.ID != info.VFID {
			continue
		}
		state.AdminMAC = vf.Mac.String()
		state.Vlan = vf.Vlan
		state.VlanQoS = vf.Qos
		state.SpoofChk = vf.Spoofchk
		state.Trust = vf.Trust != 0
		state.LinkState = config.LinkStateString(uint32(vf.LinkState))
		state.MinTxRate = int(vf.MinTxRate)
		state.MaxTxRate = int(vf.MaxTxRate)
		break
	}
	return state, nil
}

// ConfigureVFHardware 在 host netns 里把 VF 的硬件属性刷成配置要求的值。
//
// 顺序是有讲究的：
//  1. 先 down —— 多数驱动（iavf/ice/i40e）不允许在 up 状态改 MAC；
//  2. 再关 spoofchk —— 否则后续设置 MAC 会被硬件拒绝；
//  3. 然后 MAC / VLAN / trust / link-state / rate；
//  4. 最后才把 netdev 搬进 Pod netns（由调用方完成）。
func ConfigureVFHardware(info *VFInfo, conf *config.NetConf) error {
	vfLink, err := getLinkByName(info.Netdev)
	if err != nil {
		return fmt.Errorf("sriov: get VF netdev %s: %v", info.Netdev, err)
	}
	pfLink, err := getLinkByName(info.PFName)
	if err != nil {
		return fmt.Errorf("sriov: get PF %s: %v", info.PFName, err)
	}

	logging.Debugf("configuring %s: mac=%s vlan=%d qos=%d spoofchk=%s trust=%s link_state=%s",
		info, conf.MAC, conf.Vlan, conf.VlanQoS, conf.SpoofChk, conf.Trust, conf.LinkState)

	// 1) down
	if err := netlink.LinkSetDown(vfLink); err != nil {
		return fmt.Errorf("sriov: failed to set VF %s down: %v", info.Netdev, err)
	}

	// 2) spoofchk —— 只有在打算改 MAC / 要求放开伪造时才动它
	if conf.SpoofChk != "" {
		val, err := config.ParseOnOff("spoofchk", conf.SpoofChk)
		if err != nil {
			return err
		}
		if err := netlink.LinkSetVfSpoofchk(pfLink, info.VFID, val); err != nil {
			return fmt.Errorf("sriov: failed to set spoofchk=%v on VF %d: %v", val, info.VFID, err)
		}
		logging.Debugf("vf %d spoofchk=%v", info.VFID, val)
	}

	// 3) MAC —— 优先用 PF 的 IFLA_VF_MAC（对 mlx5/i40e/ice 都通用），
	//    失败时退回直接改 VF netdev 的 MAC。
	if conf.MAC != "" {
		hwaddr, err := net.ParseMAC(conf.MAC)
		if err != nil {
			return fmt.Errorf("sriov: bad mac %q: %v", conf.MAC, err)
		}
		if err := netlink.LinkSetVfHardwareAddr(pfLink, info.VFID, hwaddr); err != nil {
			logging.Warnf("cannot set VF mac via PF (%v), fallback to netdev", err)
			if err := netlink.LinkSetHardwareAddr(vfLink, hwaddr); err != nil {
				return fmt.Errorf("sriov: failed to set mac on VF %s: %v", info.Netdev, err)
			}
		}
		logging.Debugf("vf %d mac=%s", info.VFID, conf.MAC)
	}

	// 4) VLAN —— 由网卡硬件打/剥 tag，Pod 里看到的是 untagged 流量
	if conf.Vlan != 0 {
		if err := netlink.LinkSetVfVlanQos(pfLink, info.VFID, conf.Vlan, conf.VlanQoS); err != nil {
			return fmt.Errorf("sriov: failed to set vlan %d (qos %d) on VF %d: %v",
				conf.Vlan, conf.VlanQoS, info.VFID, err)
		}
		logging.Debugf("vf %d vlan=%d qos=%d", info.VFID, conf.Vlan, conf.VlanQoS)
	}

	// 5) trust —— 只有可信 VF 才能开混杂、加多播、改 MAC
	if conf.Trust != "" {
		val, err := config.ParseOnOff("trust", conf.Trust)
		if err != nil {
			return err
		}
		if err := netlink.LinkSetVfTrust(pfLink, info.VFID, val); err != nil {
			return fmt.Errorf("sriov: failed to set trust=%v on VF %d: %v", val, info.VFID, err)
		}
		logging.Debugf("vf %d trust=%v", info.VFID, val)
	}

	// 6) link-state
	if conf.LinkState != "" {
		state, err := config.ParseLinkState(conf.LinkState)
		if err != nil {
			return err
		}
		if err := netlink.LinkSetVfState(pfLink, info.VFID, state); err != nil {
			return fmt.Errorf("sriov: failed to set link_state=%s on VF %d: %v",
				conf.LinkState, info.VFID, err)
		}
		logging.Debugf("vf %d link_state=%s", info.VFID, conf.LinkState)
	}

	// 7) 限速（Mbps）。驱动不支持时会报 EOPNOTSUPP，降级为警告而不是失败。
	if conf.MaxTxRate != 0 || conf.MinTxRate != 0 {
		if err := netlink.LinkSetVfRate(pfLink, info.VFID, conf.MinTxRate, conf.MaxTxRate); err != nil {
			logging.Warnf("failed to set tx rate on VF %d: %v", info.VFID, err)
		}
	}

	return nil
}

// ResetVFHardware 把 VF 恢复成 orig 描述的状态。
// 只在 VF 已经回到 host netns 之后调用。
func ResetVFHardware(info *VFInfo, orig *config.VfState) error {
	if orig == nil {
		return nil
	}

	vfLink, err := getLinkByName(info.Netdev)
	if err != nil {
		// VF  netdev 名字在搬回 host 时可能冲突而被改名，尝试用原名字再找一次
		if vfLink, err = getLinkByName(orig.HostIFName); err != nil {
			return fmt.Errorf("sriov: cannot find VF netdev on host for reset: %v", err)
		}
	}
	pfLink, err := getLinkByName(info.PFName)
	if err != nil {
		return fmt.Errorf("sriov: get PF %s: %v", info.PFName, err)
	}

	if err := netlink.LinkSetDown(vfLink); err != nil {
		logging.Warnf("cannot down VF %s during reset: %v", vfLink.Attrs().Name, err)
	}

	if mac, err := net.ParseMAC(orig.AdminMAC); err == nil && len(mac) == 6 {
		if err := netlink.LinkSetVfHardwareAddr(pfLink, info.VFID, mac); err != nil {
			logging.Warnf("cannot restore VF %d mac: %v", info.VFID, err)
		}
	}

	if err := netlink.LinkSetVfVlanQos(pfLink, info.VFID, orig.Vlan, orig.VlanQoS); err != nil {
		logging.Warnf("cannot restore VF %d vlan: %v", info.VFID, err)
	}
	if err := netlink.LinkSetVfSpoofchk(pfLink, info.VFID, orig.SpoofChk); err != nil {
		logging.Warnf("cannot restore VF %d spoofchk: %v", info.VFID, err)
	}
	if err := netlink.LinkSetVfTrust(pfLink, info.VFID, orig.Trust); err != nil {
		logging.Warnf("cannot restore VF %d trust: %v", info.VFID, err)
	}
	if state, err := config.ParseLinkState(orig.LinkState); err == nil {
		if err := netlink.LinkSetVfState(pfLink, info.VFID, state); err != nil {
			logging.Warnf("cannot restore VF %d link_state: %v", info.VFID, err)
		}
	}
	if orig.MaxTxRate != 0 || orig.MinTxRate != 0 {
		if err := netlink.LinkSetVfRate(pfLink, info.VFID, orig.MinTxRate, orig.MaxTxRate); err != nil {
			logging.Warnf("cannot restore VF %d rate: %v", info.VFID, err)
		}
	}

	// 改回原来的 netdev 名，避免下次分配时名字漂移
	if vfLink.Attrs().Name != orig.HostIFName {
		if err := netlink.LinkSetName(vfLink, orig.HostIFName); err != nil {
			logging.Warnf("cannot restore VF netdev name to %s: %v", orig.HostIFName, err)
		}
	}

	// 恢复原来的 up/down 状态
	if orig.LinkUp {
		if err := netlink.LinkSetUp(vfLink); err != nil {
			logging.Warnf("cannot set VF %s up during reset: %v", vfLink.Attrs().Name, err)
		}
	}

	logging.Infof("VF %s reset to original state (name=%s mac=%s vlan=%d)",
		info.DeviceID, orig.HostIFName, orig.AdminMAC, orig.Vlan)
	return nil
}

// MoveVFToNetns 把 VF netdev 搬进目标 netns（Pod 的网络命名空间）。
//
// 这是 SR-IOV 直通的"最后一公里"：搬进去之后，Pod 里的进程就能
// 直接 mmap VF 的队列、收发包绕过 host 内核协议栈。
func MoveVFToNetns(info *VFInfo, netnsFd int) error {
	vfLink, err := getLinkByName(info.Netdev)
	if err != nil {
		return fmt.Errorf("sriov: get VF netdev %s: %v", info.Netdev, err)
	}
	if err := netlink.LinkSetNsFd(vfLink, netnsFd); err != nil {
		return fmt.Errorf("sriov: failed to move VF %s into netns: %v", info.Netdev, err)
	}
	logging.Infof("moved VF netdev %s (pci %s) into container netns", info.Netdev, info.DeviceID)
	return nil
}

// MoveVFToHostNetns 在 Pod netns 内部执行，把 VF 搬回 host netns。
func MoveVFToHostNetns(netdevName string, hostNetnsFd int) error {
	vfLink, err := getLinkByName(netdevName)
	if err != nil {
		return fmt.Errorf("sriov: find %s in container netns: %v", netdevName, err)
	}
	// 先把残留的 IP 地址清掉，防止回到 host 后污染主机路由表
	if addrs, err := netlink.AddrList(vfLink, netlink.FAMILY_ALL); err == nil {
		for _, a := range addrs {
			if err := netlink.AddrDel(vfLink, &a); err != nil {
				logging.Warnf("cannot flush addr %v from %s: %v", a.IP, netdevName, err)
			}
		}
	}
	if err := netlink.LinkSetDown(vfLink); err != nil {
		logging.Warnf("cannot down %s: %v", netdevName, err)
	}
	if err := netlink.LinkSetNsFd(vfLink, hostNetnsFd); err != nil {
		return fmt.Errorf("sriov: failed to move %s back to host netns: %v", netdevName, err)
	}
	return nil
}

// VerifyVF 是 CNI CHECK 的实现：确认 VF 还在 Pod netns 里、MAC 正确、状态 up。
func VerifyVF(ifName, expectMAC string) error {
	link, err := getLinkByName(ifName)
	if err != nil {
		return fmt.Errorf("sriov: CHECK failed, %s not found in container netns: %v", ifName, err)
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		return fmt.Errorf("sriov: CHECK failed, %s is down", ifName)
	}
	if expectMAC != "" && link.Attrs().HardwareAddr.String() != expectMAC {
		return fmt.Errorf("sriov: CHECK failed, %s has mac %s, expect %s",
			ifName, link.Attrs().HardwareAddr.String(), expectMAC)
	}
	return nil
}
