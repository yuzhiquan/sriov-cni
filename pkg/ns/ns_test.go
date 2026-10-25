//go:build linux
// +build linux

package ns

import (
	"os"
	"runtime"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// TestDoSwitchesNamespace verifies that Do() really switches the thread into the target netns,
// and switches back to the original netns after the closure returns.
//
// This is the easiest part of CNI to get wrong: setns only affects a single thread,
// and forgetting to lock the thread or to switch back makes devices appear to vanish.
func TestDoSwitchesNamespace(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("creating a netns requires root")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Remember the host netns first
	hostNS, err := GetCurrentNS()
	if err != nil {
		t.Fatalf("GetCurrentNS: %v", err)
	}
	defer hostNS.Close()

	// Create an empty netns
	newNS, err := netns.New()
	if err != nil {
		t.Skipf("unshare(CLONE_NEWNET) not supported in this environment: %v", err)
	}
	tmpNS := &netNS{handle: newNS, path: "/proc/self/ns/net"}
	defer tmpNS.Close()

	// Return to the host
	if err := netns.Set(hostNS.(*netNS).handle); err != nil {
		t.Fatalf("failed to return to the host netns: %v", err)
	}

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "sriovtest0"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("failed to create dummy link: %v", err)
	}
	defer netlink.LinkDel(link)

	// Should be visible in the host netns
	if _, err := netlink.LinkByName("sriovtest0"); err != nil {
		t.Fatalf("newly created link not found in the host netns: %v", err)
	}

	// Move it into the new netns
	if err := netlink.LinkSetNsFd(link, int(tmpNS.Fd())); err != nil {
		t.Fatalf("failed to move the link: %v", err)
	}

	// Should be gone from the host netns
	if _, err := netlink.LinkByName("sriovtest0"); err == nil {
		t.Errorf("link is still in the host netns, the move had no effect")
	}

	// Enter the new netns and confirm it is visible
	err = tmpNS.Do(func(host NetNS) error {
		if _, err := netlink.LinkByName("sriovtest0"); err != nil {
			t.Errorf("link not found in the new netns: %v", err)
		}
		if host == nil || host.Path() == "" {
			t.Errorf("hostNS passed to Do is nil")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	// Key: after Do returns, the current thread must be back in the host netns
	current, err := GetCurrentNS()
	if err != nil {
		t.Fatalf("GetCurrentNS: %v", err)
	}
	defer current.Close()
	if current.Path() != hostNS.Path() {
		t.Errorf("not back in the host netns after Do: %s != %s", current.Path(), hostNS.Path())
	}
}
