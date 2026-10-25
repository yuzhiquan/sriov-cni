// Package pool is a node-local VF allocator that lets the CNI itself decide which VF a Pod uses
// when there is **no Device Plugin**.
//
// The design is similar to host-local IPAM:
//   - candidates: PCI addresses listed explicitly in the config, or all VFs under certain PFs;
//   - state: one file per allocated VF under stateDir, containing the owner (containerID/ifName);
//   - concurrency: the whole directory is serialized by one flock, so concurrent CNI ADDs never get the same VF.
//
// Trade-off: kube-scheduler cannot see this pool, so Pods are still scheduled to a node after its VFs run out
// and only fail at CNI ADD. Prefer Device Plugin + PodResources in production.
package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// DefaultStateDir is the allocation state directory.
const DefaultStateDir = "/var/lib/cni/sriov/pool"

// Declared as a variable so unit tests can swap in a fake sysfs tree.
var (
	sysBusPci   = "/sys/bus/pci/devices"
	sysClassNet = "/sys/class/net"
)

// Conf describes the VF pool. At least one of Devices and PFNames must be set; both may be used together.
type Conf struct {
	// Devices explicitly lists the PCI addresses of usable VFs.
	Devices []string `json:"devices"`
	// PFNames lists PF netdev names; the pool = all VFs under these PFs.
	PFNames []string `json:"pfNames"`
	// StateDir defaults to /var/lib/cni/sriov/pool.
	StateDir string `json:"stateDir"`
}

func (c *Conf) stateDir() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	return DefaultStateDir
}

// Owner builds the owner identifier.
func Owner(containerID, ifName string) string { return containerID + "/" + ifName }

// Allocate allocates a free VF for owner and returns its PCI address.
// Repeated calls by the same owner (ADD retries) return the same VF.
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

	// First check whether this owner already holds one (ADD retry).
	for _, bdf := range candidates {
		if b, err := os.ReadFile(filepath.Join(dir, bdf)); err == nil && string(b) == owner {
			return bdf, nil
		}
	}
	for _, bdf := range candidates {
		if !usable(bdf) {
			continue // held by another Pod (netdev not in host netns) or not bound to a kernel driver
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

// Release releases the VF held by owner; does nothing if not the owner or already released.
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

// candidates expands the candidate VFs in a stable order.
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

// usable reports whether the VF can be taken over now: a netdev must still be in the host netns.
// This is a safety net: even if the state file is lost, a VF already moved into another Pod is never allocated twice.
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
