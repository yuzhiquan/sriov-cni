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

// getLinkByName looks up a link via LinkList.
// netlink.LinkByName is not used because VF info (Attrs().Vfs) is only returned in
// a full link dump (RTM_GETLINK with IFLA_VFINFO).
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

// SaveOrigVfState captures the VF state before takeover so DEL can restore it.
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

// ConfigureVFHardware sets the VF hardware attributes to the configured values in the host netns.
//
// The order matters:
//  1. Bring it down first -- most drivers (iavf/ice/i40e) do not allow changing the MAC while up;
//  2. Then disable spoofchk -- otherwise setting the MAC afterwards is rejected by the hardware;
//  3. Then MAC / VLAN / trust / link-state / rate;
//  4. Only at the end move the netdev into the Pod netns (done by the caller).
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

	// 2) spoofchk -- only touched when changing the MAC or when spoofing must be allowed
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

	// 3) MAC -- prefer the PF's IFLA_VF_MAC (works for mlx5/i40e/ice),
	//    and fall back to changing the VF netdev's MAC directly on failure.
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

	// 4) VLAN -- the NIC hardware adds/strips the tag; the Pod sees untagged traffic
	if conf.Vlan != 0 {
		if err := netlink.LinkSetVfVlanQos(pfLink, info.VFID, conf.Vlan, conf.VlanQoS); err != nil {
			return fmt.Errorf("sriov: failed to set vlan %d (qos %d) on VF %d: %v",
				conf.Vlan, conf.VlanQoS, info.VFID, err)
		}
		logging.Debugf("vf %d vlan=%d qos=%d", info.VFID, conf.Vlan, conf.VlanQoS)
	}

	// 5) trust -- only trusted VFs may enable promiscuous mode, add multicast, or change the MAC
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

	// 7) Rate limit (Mbps). Drivers without support return EOPNOTSUPP, which is downgraded to a warning instead of a failure.
	if conf.MaxTxRate != 0 || conf.MinTxRate != 0 {
		if err := netlink.LinkSetVfRate(pfLink, info.VFID, conf.MinTxRate, conf.MaxTxRate); err != nil {
			logging.Warnf("failed to set tx rate on VF %d: %v", info.VFID, err)
		}
	}

	return nil
}

// ResetVFHardware restores the VF to the state described by orig.
// Call it only after the VF has returned to the host netns.
func ResetVFHardware(info *VFInfo, orig *config.VfState) error {
	if orig == nil {
		return nil
	}

	vfLink, err := getLinkByName(info.Netdev)
	if err != nil {
		// The VF netdev name may collide and be renamed when moved back to the host; retry with the original name
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

	// Restore the original netdev name to avoid name drift on the next allocation
	if vfLink.Attrs().Name != orig.HostIFName {
		if err := netlink.LinkSetName(vfLink, orig.HostIFName); err != nil {
			logging.Warnf("cannot restore VF netdev name to %s: %v", orig.HostIFName, err)
		}
	}

	// Restore the original up/down state
	if orig.LinkUp {
		if err := netlink.LinkSetUp(vfLink); err != nil {
			logging.Warnf("cannot set VF %s up during reset: %v", vfLink.Attrs().Name, err)
		}
	}

	logging.Infof("VF %s reset to original state (name=%s mac=%s vlan=%d)",
		info.DeviceID, orig.HostIFName, orig.AdminMAC, orig.Vlan)
	return nil
}

// MoveVFToNetns moves the VF netdev into the target netns (the Pod's network namespace).
//
// This is the "last mile" of SR-IOV passthrough: once moved in, processes in the Pod can
// mmap the VF queues directly and send/receive packets bypassing the host kernel stack.
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

// MoveVFToHostNetns runs inside the Pod netns and moves the VF back to the host netns.
func MoveVFToHostNetns(netdevName string, hostNetnsFd int) error {
	vfLink, err := getLinkByName(netdevName)
	if err != nil {
		return fmt.Errorf("sriov: find %s in container netns: %v", netdevName, err)
	}
	// Clear leftover IP addresses first so they do not pollute the host routing table after the move
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

// VerifyVF implements CNI CHECK: confirm the VF is still in the Pod netns, the MAC is correct, and the state is up.
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
