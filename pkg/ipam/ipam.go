//go:build linux
// +build linux

// Package ipam does two things:
//  1. Invokes an IPAM plugin as a delegate (host-local / static / whereabouts / dhcp)
//     to obtain IP, routes, and DNS;
//  2. Applies the result to the VF netdev (addresses + routes + policy routing).
//
// SR-IOV CNI does no IPAM itself, following CNI ecosystem convention:
// "where the NIC comes from" and "how IPs are assigned" are orthogonal, decoupled via delegation.
package ipam

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/containernetworking/cni/pkg/invoke"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/example/sriov-cni/pkg/logging"
)

// ExecAdd calls the IPAM plugin's ADD and returns its Result.
func ExecAdd(ipamType string, stdinData []byte) (*types100.Result, error) {
	logging.Debugf("delegating ADD to ipam plugin %q", ipamType)
	r, err := invoke.DelegateAdd(context.TODO(), ipamType, stdinData, nil)
	if err != nil {
		return nil, fmt.Errorf("sriov: ipam plugin %q ADD failed: %v", ipamType, err)
	}
	res, err := types100.GetResult(r)
	if err != nil {
		return nil, fmt.Errorf("sriov: cannot parse ipam result: %v", err)
	}
	return res, nil
}

// ExecDel calls the IPAM plugin's DEL to release the address.
func ExecDel(ipamType string, stdinData []byte) error {
	logging.Debugf("delegating DEL to ipam plugin %q", ipamType)
	if err := invoke.DelegateDel(context.TODO(), ipamType, stdinData, nil); err != nil {
		return fmt.Errorf("sriov: ipam plugin %q DEL failed: %v", ipamType, err)
	}
	return nil
}

// ConfigureIface applies the IPAM result to the given interface. Must be called inside the target netns.
//
// Two key treatments for a secondary NIC:
//
//  1. Add the address with IFA_F_NOPREFIXROUTE so the kernel does not auto-insert into the main table a
//     prefix route like 192.168.100.0/24 dev net1, which would otherwise compete with eth0's default route
//     for traffic, causing the classic asymmetric routing problem (egress via net1, replies via eth0).
//
//  2. If IPAM returns routes with a table, also install an ip rule,
//     making a policy route: traffic from that source address uses that table.
//     This is the cleanest isolation for multi-NIC Pods (also recommended by the Multus docs).
func ConfigureIface(ifName string, res *types100.Result) error {
	if res == nil || len(res.IPs) == 0 {
		logging.Debugf("no ipam result, skip iface configuration")
		return nil
	}

	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("ipam: cannot find iface %s: %v", ifName, err)
	}

	disableIPv6DAD(ifName)

	var v4gw, v6gw net.IP
	for _, ipc := range res.IPs {
		isV4 := ipc.Address.IP.To4() != nil
		if ipc.Gateway != nil {
			if isV4 && ipc.Gateway.To4() != nil {
				v4gw = ipc.Gateway
			} else if !isV4 && ipc.Gateway.To4() == nil {
				v6gw = ipc.Gateway
			}
		}

		addr := &netlink.Addr{
			IPNet: &ipc.Address,
			Flags: unix.IFA_F_NOPREFIXROUTE,
		}
		if err := netlink.AddrAdd(link, addr); err != nil && !os.IsExist(err) {
			return fmt.Errorf("ipam: add address %v to %s: %v", ipc.Address, ifName, err)
		}
		logging.Infof("assigned %v to %s", ipc.Address, ifName)
	}

	for _, r := range res.Routes {
		isV4 := r.Dst.IP.To4() != nil
		gw := r.GW
		if gw == nil {
			if isV4 {
				gw = v4gw
			} else {
				gw = v6gw
			}
		}

		table := 0
		if r.Table != nil {
			table = *r.Table
		}

		if table > 0 {
			src := pickSrc(res, isV4)
			if src != nil {
				if err := ensureRule(src, table); err != nil {
					return err
				}
			}
		}

		route := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       &r.Dst,
			Gw:        gw,
			Table:     table,
		}
		if err := netlink.RouteAdd(route); err != nil && !os.IsExist(err) {
			return fmt.Errorf("ipam: add route %v via %v (table %d) to %s: %v",
				r.Dst, gw, table, ifName, err)
		}
		logging.Infof("added route %v via %v (table %d) on %s", r.Dst, gw, table, ifName)
	}

	return nil
}

// pickSrc picks a source address of the same family from the IPAM result, for the policy-route from selector.
func pickSrc(res *types100.Result, wantV4 bool) *net.IPNet {
	for _, ipc := range res.IPs {
		if (ipc.Address.IP.To4() != nil) == wantV4 {
			return &ipc.Address
		}
	}
	return nil
}

// ensureRule installs `ip rule add from <src> table <table>` (idempotent).
func ensureRule(src *net.IPNet, table int) error {
	rules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("ipam: list ip rules: %v", err)
	}
	for _, r := range rules {
		if r.Table == table && r.Src != nil && r.Src.String() == src.String() {
			return nil // already exists
		}
	}
	rule := netlink.NewRule()
	rule.Src = src
	rule.Table = table
	rule.Priority = 1000 + table
	if err := netlink.RuleAdd(rule); err != nil && !os.IsExist(err) {
		return fmt.Errorf("ipam: add ip rule from %v table %d: %v", src, table, err)
	}
	logging.Infof("added ip rule: from %v lookup %d (pref %d)", src, table, 1000+table)
	return nil
}

// FlushAddresses removes all addresses from the interface (used on DEL, so the VF does not pollute host routing after returning to the host).
func FlushAddresses(ifName string) error {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return err
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if err := netlink.AddrDel(link, &a); err != nil {
			logging.Warnf("cannot delete addr %v from %s: %v", a.IP, ifName, err)
		}
	}
	return nil
}

// disableIPv6DAD disables DAD so IPv6 addresses in the Pod do not stay tentative for long.
func disableIPv6DAD(ifName string) {
	base := filepath.Join("/proc/sys/net/ipv6/conf", ifName)
	for _, kv := range []struct{ file, val string }{
		{"accept_dad", "0"},
		{"dad_transmits", "0"},
	} {
		path := filepath.Join(base, kv.file)
		if _, err := os.Stat(path); err != nil {
			return // IPv6 disabled in the kernel
		}
		// #nosec G306 -- procfs needs 0644
		if err := os.WriteFile(path, []byte(kv.val), 0644); err != nil {
			logging.Warnf("cannot set %s: %v", path, err)
		}
	}
}

// DeleteRoutes removes routes installed during ADD (best effort).
func DeleteRoutes(ifName string, res *types100.Result) {
	if res == nil {
		return
	}
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return
	}
	for _, r := range res.Routes {
		table := 0
		if r.Table != nil {
			table = *r.Table
		}
		netlink.RouteDel(&netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       &r.Dst,
			Gw:        r.GW,
			Table:     table,
		})
	}
}
