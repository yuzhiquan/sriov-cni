// Package pool 是一个节点本地的 VF 分配器，用来在**没有 Device Plugin**
// 的情况下由 CNI 自己决定"这个 Pod 用哪个 VF"。
//
// 设计与 host-local IPAM 类似：
//   - 候选集合：配置里显式列出的 PCI 地址，或某些 PF 下的全部 VF；
//   - 状态：stateDir 下每个已分配 VF 对应一个文件，内容是持有者（containerID/ifName）；
//   - 并发：整个目录用一把 flock 串行化，多个 CNI 进程同时 ADD 不会拿到同一个 VF。
//
// 代价：kube-scheduler 看不到这个池子，节点上 VF 用完时 Pod 仍会被调度过来，
// 在 CNI ADD 阶段才失败。生产环境请优先使用 Device Plugin + PodResources。
package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// DefaultStateDir 是分配状态目录。
const DefaultStateDir = "/var/lib/cni/sriov/pool"

// 声明为变量，便于单元测试替换成假 sysfs 树。
var (
	sysBusPci   = "/sys/bus/pci/devices"
	sysClassNet = "/sys/class/net"
)

// Conf 描述 VF 池。Devices 与 PFNames 至少配置一个，可同时使用。
type Conf struct {
	// Devices 显式列出可用 VF 的 PCI 地址。
	Devices []string `json:"devices"`
	// PFNames 列出 PF 的 netdev 名，池子 = 这些 PF 下的全部 VF。
	PFNames []string `json:"pfNames"`
	// StateDir 默认 /var/lib/cni/sriov/pool。
	StateDir string `json:"stateDir"`
}

func (c *Conf) stateDir() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	return DefaultStateDir
}

// Owner 生成持有者标识。
func Owner(containerID, ifName string) string { return containerID + "/" + ifName }

// Allocate 为 owner 分配一个空闲 VF，返回其 PCI 地址。
// 同一个 owner 重复调用（ADD 重试）会返回同一个 VF。
func Allocate(c *Conf, owner string) (string, error) {
	if c == nil || (len(c.Devices) == 0 && len(c.PFNames) == 0) {
		return "", fmt.Errorf(`pool: "pool.devices" or "pool.pfNames" is required`)
	}
	dir := c.stateDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("pool: cannot create %s: %v", dir, err)
	}
	unlock, err := lock(dir)
	if err != nil {
		return "", err
	}
	defer unlock()

	candidates, err := candidates(c)
	if err != nil {
		return "", err
	}

	// 先看这个 owner 是不是已经拿过（ADD 重试）。
	for _, bdf := range candidates {
		if b, err := os.ReadFile(filepath.Join(dir, bdf)); err == nil && string(b) == owner {
			return bdf, nil
		}
	}
	for _, bdf := range candidates {
		if !usable(bdf) {
			continue // 正被别的 Pod 持有（netdev 不在 host netns）或未绑定内核驱动
		}
		f, err := os.OpenFile(filepath.Join(dir, bdf), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", fmt.Errorf("pool: claim %s: %v", bdf, err)
		}
		_, werr := f.WriteString(owner)
		f.Close()
		if werr != nil {
			os.Remove(filepath.Join(dir, bdf))
			return "", werr
		}
		return bdf, nil
	}
	return "", fmt.Errorf("pool: no free VF among %d candidates", len(candidates))
}

// Release 归还 owner 持有的 VF；不是持有者或已释放则什么都不做。
func Release(c *Conf, bdf, owner string) error {
	if c == nil || bdf == "" {
		return nil
	}
	dir := c.stateDir()
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()

	p := filepath.Join(dir, bdf)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	if string(b) != owner {
		return nil
	}
	return os.Remove(p)
}

// candidates 展开候选 VF，保持稳定顺序。
func candidates(c *Conf) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(bdf string) {
		if !seen[bdf] {
			seen[bdf] = true
			out = append(out, bdf)
		}
	}
	for _, d := range c.Devices {
		add(d)
	}
	for _, pf := range c.PFNames {
		dev, err := filepath.EvalSymlinks(filepath.Join(sysClassNet, pf, "device"))
		if err != nil {
			return nil, fmt.Errorf("pool: PF %q: %v", pf, err)
		}
		pfBDF := filepath.Base(dev)
		matches, _ := filepath.Glob(filepath.Join(sysBusPci, pfBDF, "virtfn*"))
		sort.Slice(matches, func(i, j int) bool { return vfNum(matches[i]) < vfNum(matches[j]) })
		for _, m := range matches {
			if r, err := filepath.EvalSymlinks(m); err == nil {
				add(filepath.Base(r))
			}
		}
	}
	return out, nil
}

func vfNum(path string) int {
	var n int
	fmt.Sscanf(strings.TrimPrefix(filepath.Base(path), "virtfn"), "%d", &n)
	return n
}

// usable 判断 VF 现在能不能被接管：必须还有 netdev 留在 host netns。
// 这是一道兜底：即使状态文件丢了，已经被搬进别的 Pod 的 VF 也不会被二次分配。
func usable(bdf string) bool {
	entries, err := os.ReadDir(filepath.Join(sysBusPci, bdf, "net"))
	return err == nil && len(entries) > 0
}

func lock(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("pool: open lock: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("pool: flock: %v", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
