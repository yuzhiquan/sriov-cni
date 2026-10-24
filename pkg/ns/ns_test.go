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

// TestDoSwitchesNamespace 验证 Do() 真的把线程切进了目标 netns，
// 并且在闭包返回后能正确切回原 netns。
//
// 这是整个 CNI 里最容易写错的地方：setns 只对单个线程生效，
// 一旦忘了锁线程或者忘了切回来，就会出现"设备不见了"的假象。
func TestDoSwitchesNamespace(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("需要 root 权限才能创建 netns")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// 先记住宿主 netns
	hostNS, err := GetCurrentNS()
	if err != nil {
		t.Fatalf("GetCurrentNS: %v", err)
	}
	defer hostNS.Close()

	// 新建一个空 netns
	newNS, err := netns.New()
	if err != nil {
		t.Skipf("当前环境不支持 unshare(CLONE_NEWNET): %v", err)
	}
	tmpNS := &netNS{handle: newNS, path: "/proc/self/ns/net"}
	defer tmpNS.Close()

	// 回到宿主
	if err := netns.Set(hostNS.(*netNS).handle); err != nil {
		t.Fatalf("回到 host netns 失败: %v", err)
	}

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "sriovtest0"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("创建 dummy 网卡失败: %v", err)
	}
	defer netlink.LinkDel(link)

	// 在宿主 netns 里应该能看到
	if _, err := netlink.LinkByName("sriovtest0"); err != nil {
		t.Fatalf("宿主 netns 里找不到刚创建的网卡: %v", err)
	}

	// 搬进新 netns
	if err := netlink.LinkSetNsFd(link, int(tmpNS.Fd())); err != nil {
		t.Fatalf("搬运网卡失败: %v", err)
	}

	// 宿主 netns 里应该已经消失
	if _, err := netlink.LinkByName("sriovtest0"); err == nil {
		t.Errorf("网卡仍在宿主 netns 里，搬运未生效")
	}

	// 进入新 netns 确认能看到
	err = tmpNS.Do(func(host NetNS) error {
		if _, err := netlink.LinkByName("sriovtest0"); err != nil {
			t.Errorf("新 netns 里找不到网卡: %v", err)
		}
		if host == nil || host.Path() == "" {
			t.Errorf("Do 传入的 hostNS 为空")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	// 关键：Do 返回后，当前线程必须已经回到宿主 netns
	current, err := GetCurrentNS()
	if err != nil {
		t.Fatalf("GetCurrentNS: %v", err)
	}
	defer current.Close()
	if current.Path() != hostNS.Path() {
		t.Errorf("Do 之后没有回到宿主 netns: %s != %s", current.Path(), hostNS.Path())
	}
}
