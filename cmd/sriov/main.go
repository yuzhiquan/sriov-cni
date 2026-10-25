//go:build linux
// +build linux

// sriov-cni is a minimal but working SR-IOV CNI plugin.
//
// The fundamental difference from ordinary CNI plugins:
//
//	bridge/ptp  :  "create" a NIC (veth pair) → put it into the Pod netns → configure IP
//	sriov       :  "take over" a real PCIe device (VF) → program hardware attributes → move into the Pod netns → configure IP
//
// It therefore relies on external components to tell it "this VF belongs to this Pod":
//
//	SR-IOV Device Plugin (resource allocation on the kubelet side)
//	        ↓  Pod spec: resources.limits[intel.com/intel_sriov_netdevice]=1
//	Multus (reads the PodResources API to get the PCI address, injects deviceID)
//	        ↓  {"deviceID":"0000:03:02.0", "vlan":1000, "ipam":{...}}
//	sriov-cni (this plugin)
package main

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/vishvananda/netlink"

	"github.com/example/sriov-cni/pkg/chain"
	"github.com/example/sriov-cni/pkg/config"
	"github.com/example/sriov-cni/pkg/ipam"
	"github.com/example/sriov-cni/pkg/k8s"
	"github.com/example/sriov-cni/pkg/logging"
	"github.com/example/sriov-cni/pkg/ns"
	"github.com/example/sriov-cni/pkg/podresources"
	"github.com/example/sriov-cni/pkg/pool"
	"github.com/example/sriov-cni/pkg/sriov"
)

func init() {
	// setns(2), used to switch netns, only affects the calling thread,
	// so the whole process must be pinned to a single OS thread.
	runtime.LockOSThread()
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, "CNI sriov plugin v0.1.0")
}

// cmdAdd handles CNI ADD. Two modes of operation:
//
//	multus  : Multus calls this plugin per NAD and it takes over one VF directly (addVF)
//	chained : this plugin runs after the primary CNI, reads Pod annotations itself, takes over the VF and appends the result to prevResult
func cmdAdd(args *skel.CmdArgs) error {
	conf, err := config.LoadConf(args.StdinData)
	if err != nil {
		return err
	}
	conf.ContainerID = args.ContainerID
	logging.Set(conf.LogLevel, conf.LogFile)

	if conf.Mode == config.ModeChained {
		return cmdAddChained(args, conf)
	}

	result := &types100.Result{CNIVersion: conf.CNIVersion}
	if err := addVF(args, conf, result); err != nil {
		return err
	}
	return types.PrintResult(result, conf.CNIVersion)
}

// cmdAddChained implements ADD in chained mode:
//
//	prevResult (primary CNI has already set up eth0)
//	  → read Pod annotations → no annotation: pass prevResult through unchanged, ordinary Pods are unaffected
//	  → annotation present: look up the networks table → merge config → addVF(ifName=net1)
//	  → append the net1 interface/IP/routes to prevResult and return it
func cmdAddChained(args *skel.CmdArgs, base *config.NetConf) error {
	if base.PrevResult == nil {
		return fmt.Errorf("sriov: mode=chained must run after another plugin in a conflist (no prevResult)")
	}
	prev, err := types100.NewResultFromResult(base.PrevResult)
	if err != nil {
		return fmt.Errorf("sriov: cannot convert prevResult: %v", err)
	}

	podName, podNS, err := k8s.ParsePodArgs(args.Args)
	if err != nil {
		return err
	}
	annotations, err := k8s.GetPodAnnotations(base.Kubernetes, podNS, podName)
	if err != nil {
		return err
	}
	netName := annotations[base.AnnotationOrDefault()]
	if netName == "" {
		logging.Debugf("pod %s/%s has no %s annotation, pass through", podNS, podName, base.AnnotationOrDefault())
		return types.PrintResult(prev, base.CNIVersion)
	}
	overlay, ok := base.Networks[netName]
	if !ok {
		return fmt.Errorf("sriov: pod %s/%s asks for network %q which is not in \"networks\"",
			podNS, podName, netName)
	}

	merged, err := chain.Merge(args.StdinData, overlay)
	if err != nil {
		return err
	}
	conf, err := config.LoadConf(merged)
	if err != nil {
		return err
	}
	conf.ContainerID = args.ContainerID

	sub := *args
	sub.StdinData = merged
	sub.IfName = base.SecondaryIfNameOrDefault()

	sec := &types100.Result{CNIVersion: conf.CNIVersion}
	if err := addVF(&sub, conf, sec); err != nil {
		return err
	}
	logging.Infof("chained: pod %s/%s network %q -> %s", podNS, podName, netName, sub.IfName)
	return types.PrintResult(chain.Append(prev, sec), conf.CNIVersion)
}

// resolveDevice decides which VF to use based on vfAllocator and writes it to conf.DeviceID.
// If a deviceID already exists (injected by Multus or hard-coded in config), it is used as is.
func resolveDevice(args *skel.CmdArgs, conf *config.NetConf) error {
	if conf.DeviceID != "" {
		return nil
	}
	switch conf.VFAllocator {
	case config.AllocPodResources:
		pr := conf.PodResources
		if pr == nil || pr.Resource == "" {
			return fmt.Errorf(`sriov: vfAllocator=podresources needs "podResources.resource"`)
		}
		podName, podNS, err := k8s.ParsePodArgs(args.Args)
		if err != nil {
			return err
		}
		timeout := time.Duration(pr.TimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = 3 * time.Second
		}
		ids, err := podresources.WaitForDevices(pr.Socket, podName, podNS, pr.Resource, timeout)
		if err != nil {
			return err
		}
		if pr.Index < 0 || pr.Index >= len(ids) {
			return fmt.Errorf("sriov: podResources.index %d out of range, pod has %d %q devices",
				pr.Index, len(ids), pr.Resource)
		}
		conf.DeviceID = ids[pr.Index]
		logging.Infof("podresources: %s -> %s", pr.Resource, conf.DeviceID)
	case config.AllocPool:
		bdf, err := pool.Allocate(conf.Pool, pool.Owner(args.ContainerID, args.IfName))
		if err != nil {
			return err
		}
		conf.DeviceID = bdf
		logging.Infof("pool: allocated %s to %s", bdf, pool.Owner(args.ContainerID, args.IfName))
	}
	return conf.Validate()
}

// addVF takes over a VF and writes the interface/IP/routes into result.
//
// Full sequence:
//
//	locate VF (podresources/pool/injected) → locate VF (netdev/PF/vfID)
//	        → save original VF hardware state (for restoring on DEL)
//	        → [host netns] down + spoofchk + MAC + VLAN + trust + link-state
//	        → move the VF netdev into the Pod netns
//	        → [pod  netns] rename → MTU → up
//	        → call IPAM to get IP/routes → apply addresses + routes (including policy routing)
func addVF(args *skel.CmdArgs, conf *config.NetConf, result *types100.Result) (err error) {
	// ---------- 1. Decide which VF to use ----------
	if err = resolveDevice(args, conf); err != nil {
		return err
	}
	// In pool mode, if any later step fails, the VF must be returned to the pool.
	defer func() {
		if err != nil && conf.VFAllocator == config.AllocPool {
			_ = pool.Release(conf.Pool, conf.DeviceID, pool.Owner(args.ContainerID, args.IfName))
		}
	}()

	// ---------- 2. Locate the VF ----------
	vfInfo, err := sriov.GetVFInfo(conf)
	if err != nil {
		return err
	}
	// Write the derived values back so DEL can find the device even if the config does not specify them
	conf.DeviceID, conf.PFBDF = vfInfo.DeviceID, vfInfo.PFBDF
	conf.PFName, conf.VFID, conf.Netdev = vfInfo.PFName, vfInfo.VFID, vfInfo.Netdev

	logging.Infof("ADD: %s -> container=%s iface=%s netns=%s",
		vfInfo, args.ContainerID, args.IfName, args.Netns)

	// ---------- 3. Save original state ----------
	origState, err := sriov.SaveOrigVfState(vfInfo)
	if err != nil {
		return err
	}
	conf.OrigVfState = origState

	ipamDone := false
	// Rollback on failure: any ADD failure must hand the VF back cleanly,
	// otherwise the VF stays half-configured and later Pods that get it will see strange failures.
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

		// ---------- 4. Program VF hardware attributes (still in host netns) ----------
		if err = sriov.ConfigureVFHardware(vfInfo, conf); err != nil {
			return err
		}

		// ---------- 5. Move into the Pod netns ----------
		if err = sriov.MoveVFToNetns(vfInfo, int(targetNS.Fd())); err != nil {
			return err
		}
	} else {
		// DPDK mode: the VF is bound to vfio-pci, so there is no netdev to move.
		// Just return the PCI address in the result so the upstream device plugin can
		// mount /dev/vfio/<n> into the container.
		logging.Infof("DPDK mode: VF %s stays bound to vfio-pci", vfInfo.DeviceID)
	}

	// ---------- 6. IPAM: allocate IP ----------
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

	// ---------- 7. Enter the Pod netns and finish up ----------
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

	// ---------- 8. Persist cache (needed by DEL) ----------
	conf.StdinData = args.StdinData
	if err2 := config.SaveCachedConf(conf.ContainerID, conf.DeviceID, args.IfName, conf); err2 != nil {
		logging.Warnf("cannot save cached conf: %v", err2)
	}

	logging.Infof("ADD done: %s -> %s", vfInfo.DeviceID, args.IfName)
	return nil
}

// finalizeInContainer runs inside the Pod netns: rename, MTU, up, apply IP.
func finalizeInContainer(args *skel.CmdArgs, vfInfo *sriov.VFInfo,
	conf *config.NetConf, result *types100.Result) error {

	name := vfInfo.Netdev
	link, err := netlink.LinkByName(name)
	if err != nil {
		// Edge case: on a name collision when moving in, the kernel appended a suffix. Fall back to finding it by MAC.
		if name, err = findLinkByMAC(conf.MAC); err != nil {
			return err
		}
		if link, err = netlink.LinkByName(name); err != nil {
			return err
		}
	}

	// Renaming must be done while the link is down (kernel dev_change_name returns EBUSY for UP devices).
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

	// Fill in the interfaces section of the CNI Result
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

// cmdDel handles CNI DEL: release the IP, move the VF back to the host and restore hardware attributes.
func cmdDel(args *skel.CmdArgs) error {
	conf, err := config.LoadConf(args.StdinData)
	if err != nil {
		return err
	}
	conf.ContainerID = args.ContainerID
	logging.Set(conf.LogLevel, conf.LogFile)

	chained := conf.Mode == config.ModeChained
	if chained {
		// In chained mode the runtime passes the primary NIC eth0 as IfName; the secondary NIC must be net1.
		sub := *args
		sub.IfName = conf.SecondaryIfNameOrDefault()
		args = &sub
	}

	// The config passed by the runtime on DEL usually has no deviceID,
	// so prefer filling it in from the cache persisted at ADD time.
	if cached, e := config.LoadCachedConf(conf.ContainerID, conf.DeviceID, args.IfName); e == nil {
		mergeCached(conf, cached)
		if chained && len(cached.StdinData) > 0 {
			// Release the IP using the merged config from ADD, not the unmerged conflist.
			sub := *args
			sub.StdinData = cached.StdinData
			args = &sub
		}
	} else if chained {
		// No cache = this Pod never had a secondary NIC (or it was already cleaned up); DEL must be idempotent.
		logging.Debugf("chained DEL: nothing cached for %s/%s, skip", conf.ContainerID, args.IfName)
		return nil
	} else {
		logging.Warnf("no cached state (%v), falling back to stdin config", e)
	}

	if conf.DeviceID == "" {
		return fmt.Errorf("sriov: DEL without deviceID, cannot locate the VF")
	}

	// 1) Release the IP (first: IPAM does not depend on netns and is less likely to fail)
	if conf.IPAM.Type != "" {
		if e := ipam.ExecDel(conf.IPAM.Type, args.StdinData); e != nil {
			logging.Warnf("ipam DEL failed: %v", e)
		}
	}

	// 2) Move the VF back to the host and restore it
	vfInfo, err := sriov.GetVFInfo(conf)
	if err != nil {
		return err
	}
	if err := releaseVFToHost(args, vfInfo, conf); err != nil {
		return err
	}

	if conf.VFAllocator == config.AllocPool {
		if e := pool.Release(conf.Pool, conf.DeviceID, pool.Owner(args.ContainerID, args.IfName)); e != nil {
			logging.Warnf("pool release %s: %v", conf.DeviceID, e)
		}
	}
	config.RemoveCachedConf(conf.ContainerID, conf.DeviceID, args.IfName)
	logging.Infof("DEL done: %s", conf.DeviceID)
	return nil
}

// mergeCached fills in the current config with key information from the cache.
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
	// Source and pool config follow what was used at ADD time (in chained mode stdin has no merged network overrides).
	if cached.VFAllocator != "" {
		conf.VFAllocator = cached.VFAllocator
		conf.Pool = cached.Pool
	}
	if cached.IPAM.Type != "" && conf.IPAM.Type == "" {
		conf.IPAM = cached.IPAM
	}
}

// cmdCheck handles CNI CHECK: verify the VF is still in the Pod netns and correctly configured.
func cmdCheck(args *skel.CmdArgs) error {
	conf, err := config.LoadConf(args.StdinData)
	if err != nil {
		return err
	}
	logging.Set(conf.LogLevel, conf.LogFile)

	if conf.Mode == config.ModeChained {
		sub := *args
		sub.IfName = conf.SecondaryIfNameOrDefault()
		args = &sub
		cached, e := config.LoadCachedConf(args.ContainerID, "", args.IfName)
		if e != nil {
			return nil // this Pod has no secondary NIC
		}
		conf.MAC = cached.MAC
	}

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

// releaseVFToHost moves the VF from the Pod netns back to the host netns and restores its config.
//
// Note a kernel behavior: if the Pod netns has already been destroyed (Pod force-killed),
// the kernel automatically returns the netdevs inside to the init netns (host). So this
// tolerates a missing netns: just look the device up on the host.
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

	// Back in the host netns, re-locate the netdev by PCI address (the name may have been changed)
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

// findLinkByMAC finds a netdev name by MAC in the current netns.
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
