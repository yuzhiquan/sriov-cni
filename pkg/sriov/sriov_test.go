//go:build linux
// +build linux

package sriov

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example/sriov-cni/pkg/config"
)

// fakeSysfs 搭一棵假的 PCI 拓扑树，模拟：
//
//	0000:03:00.0   PF  (netdev: ens1f0)
//	  ├── virtfn0 -> 0000:03:02.0  (netdev: ens1f0v0)
//	  └── virtfn1 -> 0000:03:02.1  (netdev: ens1f0v1)
func fakeSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	mkdir := func(p string) {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	symlink := func(target, link string) {
		mkdir(filepath.Dir(link))
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink %s: %v", link, err)
		}
	}

	mkdir(filepath.Join(root, "0000:03:00.0", "net", "ens1f0"))
	mkdir(filepath.Join(root, "0000:03:02.0", "net", "ens1f0v0"))
	mkdir(filepath.Join(root, "0000:03:02.1", "net", "ens1f0v1"))

	symlink("../0000:03:02.0", filepath.Join(root, "0000:03:00.0", "virtfn0"))
	symlink("../0000:03:02.1", filepath.Join(root, "0000:03:00.0", "virtfn1"))
	symlink("../0000:03:00.0", filepath.Join(root, "0000:03:02.0", "physfn"))
	symlink("../0000:03:00.0", filepath.Join(root, "0000:03:02.1", "physfn"))

	return root
}

func TestGetVFInfo(t *testing.T) {
	root := fakeSysfs(t)
	orig := sysBusPci
	sysBusPci = root
	defer func() { sysBusPci = orig }()

	cases := []struct {
		deviceID string
		wantPF   string
		wantID   int
		wantDev  string
	}{
		{"0000:03:02.0", "ens1f0", 0, "ens1f0v0"},
		{"0000:03:02.1", "ens1f0", 1, "ens1f0v1"},
	}

	for _, c := range cases {
		info, err := GetVFInfo(&config.NetConf{DeviceID: c.deviceID})
		if err != nil {
			t.Fatalf("GetVFInfo(%s): %v", c.deviceID, err)
		}
		if info.PFName != c.wantPF || info.VFID != c.wantID || info.Netdev != c.wantDev {
			t.Errorf("GetVFInfo(%s) = %s, want pf=%s id=%d netdev=%s",
				c.deviceID, info, c.wantPF, c.wantID, c.wantDev)
		}
		if info.PFBDF != "0000:03:00.0" {
			t.Errorf("PFBDF = %s, want 0000:03:00.0", info.PFBDF)
		}
	}
}

func TestGetVFInfoNotAVF(t *testing.T) {
	root := fakeSysfs(t)
	orig := sysBusPci
	sysBusPci = root
	defer func() { sysBusPci = orig }()

	// PF 自己没有 physfn，应当报错
	if _, err := GetVFInfo(&config.NetConf{DeviceID: "0000:03:00.0"}); err == nil {
		t.Errorf("expect error when device is a PF, got nil")
	}
}

func TestFindVfIDNoVFs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "0000:04:00.0"), 0755); err != nil {
		t.Fatal(err)
	}
	orig := sysBusPci
	sysBusPci = root
	defer func() { sysBusPci = orig }()

	if _, err := findVfID("0000:04:00.0", "0000:04:02.0"); err == nil {
		t.Errorf("expect error when PF has no VFs")
	}
}
