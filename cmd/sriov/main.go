//go:build linux
// +build linux

// sriov-cni 是一个最小但可运行的 SR-IOV CNI 插件。
//
// 它与普通 CNI 的根本区别：
//
//	bridge/ptp  :  "造"一张网卡（veth pair）→ 塞进 Pod netns → 配 IP
//	sriov       :  "接管"一块真实 PCIe 设备（VF）→ 刷硬件属性 → 搬进 Pod netns → 配 IP
//
// 因此它依赖外部组件告诉它"这块 VF 归这个 Pod"：
//
//	SR-IOV Device Plugin（kubelet 侧分配资源）
//	        ↓  Pod spec: resources.limits[intel.com/intel_sriov_netdevice]=1
//	Multus（读 PodResources API 拿到 PCI 地址，注入 deviceID）
//	        ↓  {"deviceID":"0000:03:02.0", "vlan":1000, "ipam":{...}}
//	sriov-cni（本插件）
package main

import (
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/vishvananda/netlink"

	"github.com/example/sriov-cni/pkg/config"
	"github.com/example/sriov-cni/pkg/ipam"
	"github.com/example/sriov-cni/pkg/logging"
	"github.com/example/sriov-cni/pkg/ns"
	"github.com/example/sriov-cni/pkg/sriov"
)

func init() {
	// 切换 netns 的 setns(2) 只对调用线程生效，
	// 因此整个进程必须锁死在一条 OS 线程上。
	runtime.LockOSThread()
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, "CNI sriov plugin v0.1.0")
}

// cmdAdd 处理 CNI ADD。
//
// 完整时序：
//
//	解析配置 → 定位 VF(netdev/PF/vfID)
//	        → 保存 VF 原始硬件状态（供 DEL 还原）
//	        → [host netns] down + spoofchk + MAC + VLAN + trust + link-state
//	        → 把 VF netdev 搬进 Pod netns
//	        → [pod  netns] 改名 → MTU → up
//	        → 调 IPAM 拿 IP/路由 → 下发地址 + 路由（含策略路由）
func cmdAdd(args *skel.CmdArgs) (err error) {
	// ---------- 1. 解析配置 ----------
	conf, err := config.LoadConf(args.StdinData)
	if err != nil {
		return err
	}
	conf.ContainerID = args.ContainerID
	logging.Set(conf.LogLevel, conf.LogFile)

	// ---------- 2. 定位 VF ----------
	vfInfo, err := sriov.GetVFInfo(conf)
	if err != nil {
		return err
	}
	// 把推算结果回填，DEL 时即使配置里没写也能找到设备
	conf.DeviceID, conf.PFBDF = vfInfo.DeviceID, vfInfo.PFBDF
	conf.PFName, conf.VFID, conf.Netdev = vfInfo.PFName, vfInfo.VFID, vfInfo.Netdev

	logging.Infof("ADD: %s -> container=%s iface=%s netns=%s",
		vfInfo, args.ContainerID, args.IfName, args.Netns)

	// ---------- 3. 保存原始状态 ----------
	origState, err := sriov.SaveOrigVfState(vfInfo)
	if err != nil {
		return err
	}
	conf.OrigVfState = origState

	result := &types100.Result{CNIVersion: conf.CNIVersion}

	ipamDone := false
	// 失败回滚：任何一种 ADD 失败都要把 VF 干净地还回去，
	// 否则这个 VF 会带着半个配置卡住，后续 Pod 分到它就会出诡异故障。
	defer func() {
		if err == nil {
			return
		}
		logging.Errorf("ADD failed (%v), rolling back", err)
		if ipamDone && conf.IPAM.Type != "" {
			if e := ipam.ExecDel(conf.IPAM.Type, args.StdinData); e != nil {
				logging.Warnf("rollback: ipam DEL: %v", e)
			}
		}
		if e := releaseVFToHost(args, vfInfo, conf); e != nil {
			logging.Warnf("rollback: release VF: %v", e)
		}
		config.RemoveCachedConf(conf.ContainerID, conf.DeviceID, args.IfName)
	}()

	var targetNS ns.NetNS
	if !conf.DPDKMode {
		if args.Netns == "" {
			return fmt.Errorf("sriov: ADD requires CNI_NETNS")
		}
		targetNS, err = ns.GetNS(args.Netns)
		if err != nil {
			return fmt.Errorf("sriov: cannot open netns %s: %v", args.Netns, err)
		}
		defer targetNS.Close()

		// ---------- 4. 刷 VF 硬件属性（还在 host netns） ----------
		if err = sriov.ConfigureVFHardware(vfInfo, conf); err != nil {
			return err
		}

		// ---------- 5. 搬进 Pod netns ----------
		if err = sriov.MoveVFToNetns(vfInfo, int(targetNS.Fd())); err != nil {
			return err
		}
	} else {
		// DPDK 模式：VF 已被 vfio-pci 绑定，没有 netdev 可搬。
		// 只需要在结果里回传 PCI 地址，让上层的 device plugin 把
		// /dev/vfio/<n> 挂进容器即可。
		logging.Infof("DPDK mode: VF %s stays bound to vfio-pci", vfInfo.DeviceID)
	}

	// ---------- 6. IPAM：分配 IP ----------
	if conf.IPAM.Type != "" {
		ipamRes, e := ipam.ExecAdd(conf.IPAM.Type, args.StdinData)
		if e != nil {
			return e
		}
		ipamDone = true
		result.IPs = ipamRes.IPs
		result.Routes = ipamRes.Routes
		result.DNS = ipamRes.DNS
	}

	// ---------- 7. 进入 Pod netns 完成收尾 ----------
	if !conf.DPDKMode {
		if err = targetNS.Do(func(_ ns.NetNS) error {
			return finalizeInContainer(args, vfInfo, conf, result)
		}); err != nil {
			return err
		}
	} else {
		ifIndex := len(result.Interfaces)
		result.Interfaces = append(result.Interfaces, &types100.Interface{
			Name:    args.IfName,
			Mac:     conf.MAC,
			Sandbox: args.Netns,
		})
		for i := range result.IPs {
			idx := ifIndex
			result.IPs[i].Interface = &idx
		}
	}

	// ---------- 8. 落盘缓存（DEL 需要） ----------
	if err2 := config.SaveCachedConf(conf.ContainerID, conf.DeviceID, args.IfName, conf); err2 != nil {
		logging.Warnf("cannot save cached conf: %v", err2)
	}

	logging.Infof("ADD done: %s -> %s", vfInfo.DeviceID, args.IfName)
	return types.PrintResult(result, conf.CNIVersion)
}

// finalizeInContainer 在 Pod netns 内执行：改名、MTU、up、下发 IP。
func finalizeInContainer(args *skel.CmdArgs, vfInfo *sriov.VFInfo,
	conf *config.NetConf, result *types100.Result) error {

	name := vfInfo.Netdev
	link, err := netlink.LinkByName(name)
	if err != nil {
		// 极端情况：搬进来时名字冲突，内核自动加了后缀。用 MAC 兜底找。
		if name, err = findLinkByMAC(conf.MAC); err != nil {
			return err
		}
		if link, err = netlink.LinkByName(name); err != nil {
			return err
		}
	}

	// 改名必须在 down 状态下做（内核 dev_change_name 对 UP 设备返回 EBUSY）。
	if name != args.IfName {
		if err = netlink.LinkSetDown(link); err != nil {
			return fmt.Errorf("sriov: down %s before rename: %v", name, err)
		}
		if err = netlink.LinkSetName(link, args.IfName); err != nil {
			return fmt.Errorf("sriov: rename %s -> %s: %v", name, args.IfName, err)
		}
		logging.Debugf("renamed %s -> %s", name, args.IfName)
	}

	if conf.MTU > 0 && link.Attrs().MTU != conf.MTU {
		if err = netlink.LinkSetMTU(link, conf.MTU); err != nil {
			return fmt.Errorf("sriov: set mtu %d on %s: %v", conf.MTU, args.IfName, err)
		}
	}

	if err = netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("sriov: set %s up: %v", args.IfName, err)
	}

	// 回填 CNI Result 的 interfaces 段
	ifIndex := len(result.Interfaces)
	result.Interfaces = append(result.Interfaces, &types100.Interface{
		Name:    args.IfName,
		Mac:     link.Attrs().HardwareAddr.String(),
		Sandbox: args.Netns,
	})
	for i := range result.IPs {
		if result.IPs[i].Interface == nil {
			idx := ifIndex
			result.IPs[i].Interface = &idx
		}
	}

	return ipam.ConfigureIface(args.IfName, result)
}

// cmdDel 处理 CNI DEL：归还 IP、把 VF 搬回 host 并还原硬件属性。
func cmdDel(args *skel.CmdArgs) error {
	conf, err := config.LoadConf(args.StdinData)
	if err != nil {
		return err
	}
	conf.ContainerID = args.ContainerID
	logging.Set(conf.LogLevel, conf.LogFile)

	// DEL 时 runtime 传进来的配置通常不含 deviceID，
	// 优先用 ADD 时落盘的缓存补齐。
	if cached, e := config.LoadCachedConf(conf.ContainerID, conf.DeviceID, args.IfName); e == nil {
		mergeCached(conf, cached)
	} else {
		logging.Warnf("no cached state (%v), falling back to stdin config", e)
	}

	if conf.DeviceID == "" {
		return fmt.Errorf("sriov: DEL without deviceID, cannot locate the VF")
	}

	// 1) 归还 IP（先做：IPAM 不依赖 netns，且失败概率更低）
	if conf.IPAM.Type != "" {
		if e := ipam.ExecDel(conf.IPAM.Type, args.StdinData); e != nil {
			logging.Warnf("ipam DEL failed: %v", e)
		}
	}

	// 2) 把 VF 搬回 host 并还原
	vfInfo, err := sriov.GetVFInfo(conf)
	if err != nil {
		return err
	}
	if err := releaseVFToHost(args, vfInfo, conf); err != nil {
		return err
	}

	config.RemoveCachedConf(conf.ContainerID, conf.DeviceID, args.IfName)
	logging.Infof("DEL done: %s", conf.DeviceID)
	return nil
}

// mergeCached 用缓存里的关键信息补齐当前配置。
func mergeCached(conf, cached *config.NetConf) {
	if conf.DeviceID == "" {
		conf.DeviceID = cached.DeviceID
	}
	if conf.PFName == "" {
		conf.PFName = cached.PFName
	}
	if conf.PFBDF == "" {
		conf.PFBDF = cached.PFBDF
	}
	if conf.Netdev == "" {
		conf.Netdev = cached.Netdev
	}
	if conf.VFID <= 0 {
		conf.VFID = cached.VFID
	}
	if conf.OrigVfState == nil {
		conf.OrigVfState = cached.OrigVfState
	}
	if conf.MAC == "" {
		conf.MAC = cached.MAC
	}
}

// cmdCheck 处理 CNI CHECK：确认 VF 仍在 Pod netns 且配置正确。
func cmdCheck(args *skel.CmdArgs) error {
	conf, err := config.LoadConf(args.StdinData)
	if err != nil {
		return err
	}
	logging.Set(conf.LogLevel, conf.LogFile)

	if args.Netns == "" {
		return fmt.Errorf("sriov: CHECK requires CNI_NETNS")
	}
	targetNS, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("sriov: cannot open netns %s: %v", args.Netns, err)
	}
	defer targetNS.Close()

	return targetNS.Do(func(_ ns.NetNS) error {
		return sriov.VerifyVF(args.IfName, conf.MAC)
	})
}

// releaseVFToHost 把 VF 从 Pod netns 搬回 host netns 并还原配置。
//
// 注意一个内核行为：如果 Pod 的 netns 已经被销毁（Pod 被强杀），
// 内核会自动把里面的 netdev 送回 init netns（宿主）。所以这里对
// "netns 不存在" 的情况是容错的 —— 直接在 host 上找即可。
func releaseVFToHost(args *skel.CmdArgs, vfInfo *sriov.VFInfo, conf *config.NetConf) (err error) {
	if args.Netns != "" {
		if _, statErr := os.Stat(args.Netns); statErr == nil {
			targetNS, nsErr := ns.GetNS(args.Netns)
			if nsErr == nil {
				nsErr = targetNS.Do(func(hostNS ns.NetNS) error {
					return sriov.MoveVFToHostNetns(args.IfName, int(hostNS.Fd()))
				})
				if nsErr != nil {
					logging.Warnf("cannot move %s out of netns (may already be back on host): %v",
						args.IfName, nsErr)
				}
				targetNS.Close()
			}
		}
	}

	// 回到 host netns，按 PCI 地址重新定位 netdev（名字可能被改过）
	netdev, err := sriov.GetNetdevByPci(vfInfo.DeviceID)
	if err != nil {
		return err
	}
	hostInfo := *vfInfo
	hostInfo.Netdev = netdev

	if !conf.ResetOnDelOrDefault() {
		logging.Infof("resetOnDel=false, leave VF %s as is", vfInfo.DeviceID)
		return nil
	}
	return sriov.ResetVFHardware(&hostInfo, conf.OrigVfState)
}

// findLinkByMAC 在当前 netns 里按 MAC 找 netdev 名。
func findLinkByMAC(mac string) (string, error) {
	if mac == "" {
		return "", fmt.Errorf("sriov: cannot locate VF netdev: no mac hint")
	}
	want, err := net.ParseMAC(mac)
	if err != nil {
		return "", err
	}
	links, err := netlink.LinkList()
	if err != nil {
		return "", err
	}
	for _, l := range links {
		if l.Attrs().HardwareAddr != nil &&
			l.Attrs().HardwareAddr.String() == want.String() {
			return l.Attrs().Name, nil
		}
	}
	return "", fmt.Errorf("sriov: no netdev with mac %s", mac)
}
