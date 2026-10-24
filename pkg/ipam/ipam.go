//go:build linux
// +build linux

// Package ipam 负责两件事：
//  1. 以 delegate 的方式调用 IPAM 插件（host-local / static / whereabouts / dhcp）
//     拿到 IP、路由、DNS；
//  2. 把拿到结果真正下发到 VF netdev 上（地址 + 路由 + 策略路由）。
//
// SR-IOV CNI 自己不做 IPAM —— 这是 CNI 生态的惯例：
// "网卡怎么来" 和 "IP 怎么分" 是两个正交的问题，用 delegate 解耦。
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

// ExecAdd 调用 IPAM 插件的 ADD，返回其 Result。
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

// ExecDel 调用 IPAM 插件的 DEL 归还地址。
func ExecDel(ipamType string, stdinData []byte) error {
	logging.Debugf("delegating DEL to ipam plugin %q", ipamType)
	if err := invoke.DelegateDel(context.TODO(), ipamType, stdinData, nil); err != nil {
		return fmt.Errorf("sriov: ipam plugin %q DEL failed: %v", ipamType, err)
	}
	return nil
}

// ConfigureIface 把 IPAM 结果应用到指定网卡上。必须在目标 netns 内调用。
//
// 两个针对"第二块网卡"的关键处理：
//
//  1. 加地址时带 IFA_F_NOPREFIXROUTE：不让内核自动往 main 表塞
//     192.168.100.0/24 dev net1 这样的前缀路由 —— 否则会和 eth0 的默认路由
//     抢流量，出现"能从 net1 出去但回包走 eth0"的经典非对称路由问题。
//
//  2. 如果 IPAM 返回了带 table 的路由，额外下发 ip rule，
//     做成"从该源地址发出的流量走该表"的策略路由，
//     这是多网卡 Pod 最干净的隔离方式（也是 Multus 文档推荐的做法）。
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

// pickSrc 从 IPAM 结果里挑一个同族的源地址，用于策略路由的 from 选择器。
func pickSrc(res *types100.Result, wantV4 bool) *net.IPNet {
	for _, ipc := range res.IPs {
		if (ipc.Address.IP.To4() != nil) == wantV4 {
			return &ipc.Address
		}
	}
	return nil
}

// ensureRule 下发 `ip rule add from <src> table <table>`（幂等）。
func ensureRule(src *net.IPNet, table int) error {
	rules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("ipam: list ip rules: %v", err)
	}
	for _, r := range rules {
		if r.Table == table && r.Src != nil && r.Src.String() == src.String() {
			return nil // 已存在
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

// FlushAddresses 清空网卡上的所有地址（DEL 时用，防止 VF 回到 host 后污染主机路由）。
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

// disableIPv6DAD 关闭 DAD，避免 Pod 内 IPv6 地址长时间处于 tentative。
func disableIPv6DAD(ifName string) {
	base := filepath.Join("/proc/sys/net/ipv6/conf", ifName)
	for _, kv := range []struct{ file, val string }{
		{"accept_dad", "0"},
		{"dad_transmits", "0"},
	} {
		path := filepath.Join(base, kv.file)
		if _, err := os.Stat(path); err != nil {
			return // 内核没开 IPv6
		}
		// #nosec G306 -- procfs 需要 0644
		if err := os.WriteFile(path, []byte(kv.val), 0644); err != nil {
			logging.Warnf("cannot set %s: %v", path, err)
		}
	}
}

// DeleteRoutes 删除 ADD 时下发的路由（尽力而为）。
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
