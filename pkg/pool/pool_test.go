package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeSysfs builds a fake sysfs tree with a PF and n VFs, and returns the PF name.
func fakeSysfs(t *testing.T, n int) {
	t.Helper()
	root := t.TempDir()
	sysBusPci = filepath.Join(root, "bus/pci/devices")
	sysClassNet = filepath.Join(root, "class/net")

	pf := filepath.Join(sysBusPci, "0000:03:00.0")
	must(t, os.MkdirAll(pf, 0o755))
	must(t, os.MkdirAll(filepath.Join(sysClassNet, "ens1f0"), 0o755))
	must(t, os.Symlink(pf, filepath.Join(sysClassNet, "ens1f0", "device")))
	for i := 0; i < n; i++ {
		bdf := fmt.Sprintf("0000:03:02.%d", i)
		vf := filepath.Join(sysBusPci, bdf)
		must(t, os.MkdirAll(filepath.Join(vf, "net", fmt.Sprintf("ens1f0v%d", i)), 0o755))
		must(t, os.Symlink(vf, filepath.Join(pf, fmt.Sprintf("virtfn%d", i))))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAllocateReleaseCycle(t *testing.T) {
	fakeSysfs(t, 2)
	c := &Conf{PFNames: []string{"ens1f0"}, StateDir: t.TempDir()}

	a, err := Allocate(c, "c1/net1")
	must(t, err)
	b, err := Allocate(c, "c2/net1")
	must(t, err)
	if a == b {
		t.Fatalf("two owners got the same VF %s", a)
	}
	if a != "0000:03:02.0" || b != "0000:03:02.1" {
		t.Errorf("order not stable: %s %s", a, b)
	}
	if _, err := Allocate(c, "c3/net1"); err == nil {
		t.Fatal("pool of 2 should be exhausted")
	}

	// ADD retry: the same owner gets the same VF
	again, err := Allocate(c, "c1/net1")
	must(t, err)
	if again != a {
		t.Errorf("retry got %s, want %s", again, a)
	}

	// Release by a non-owner is a no-op; after the owner releases, the VF can be reused
	must(t, Release(c, a, "someone-else"))
	if _, err := Allocate(c, "c3/net1"); err == nil {
		t.Fatal("foreign Release must not free the VF")
	}
	must(t, Release(c, a, "c1/net1"))
	got, err := Allocate(c, "c3/net1")
	must(t, err)
	if got != a {
		t.Errorf("released VF not reused: got %s", got)
	}
}

func TestAllocateSkipsVFWithoutHostNetdev(t *testing.T) {
	fakeSysfs(t, 2)
	// VF0 was moved into another Pod: the host no longer has a net/ entry
	must(t, os.RemoveAll(filepath.Join(sysBusPci, "0000:03:02.0", "net", "ens1f0v0")))

	c := &Conf{Devices: []string{"0000:03:02.0", "0000:03:02.1"}, StateDir: t.TempDir()}
	got, err := Allocate(c, "c1/net1")
	must(t, err)
	if got != "0000:03:02.1" {
		t.Errorf("got %s, want the VF that still has a host netdev", got)
	}
}

func TestAllocateNeedsCandidates(t *testing.T) {
	if _, err := Allocate(&Conf{StateDir: t.TempDir()}, "o"); err == nil {
		t.Error("want error for empty pool config")
	}
	if _, err := Allocate(nil, "o"); err == nil {
		t.Error("want error for nil config")
	}
}
