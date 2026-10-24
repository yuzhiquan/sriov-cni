//go:build linux
// +build linux

// Package ns 提供网络命名空间的进出能力。
//
// 为什么必须自己小心处理线程：
// setns(2) 只影响**调用它的那一个线程**，而 Go 的 goroutine 会在 OS 线程之间
// 被调度器搬运。如果不把 goroutine 锁死在线程上，就可能出现"上半段在 Pod
// netns、下半段突然回到 host netns"的诡异行为。这就是 CNI 插件里
// runtime.LockOSThread() 无处不在的原因。
package ns

import (
	"fmt"
	"os"
	"runtime"
	"sync"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// NetNS 是对一个网络命名空间句柄的抽象。
type NetNS interface {
	// Path 返回该 netns 在 /proc 或 /var/run/netns 下的路径
	Path() string
	// Fd 返回可用于 setns / netlink 操作的 fd
	Fd() uintptr
	// Set 把当前线程切换到该 netns
	Set() error
	// Do 在该 netns 内执行闭包，参数是把"原来的"netns 传进去，
	// 方便闭包里把设备搬回去。
	Do(toRun func(hostNS NetNS) error) error
	// Close 释放句柄
	Close() error
}

type netNS struct {
	handle netns.NsHandle
	path   string
	closed bool
	mu     sync.Mutex
}

// GetNS 通过路径打开一个 netns。
// 容器运行时给 CNI 的 CNI_NETNS 通常是 /proc/<pid>/ns/net 或 /var/run/netns/<name>。
func GetNS(path string) (NetNS, error) {
	handle, err := netns.GetFromPath(path)
	if err != nil {
		return nil, fmt.Errorf("ns: cannot open netns %q: %v", path, err)
	}
	return &netNS{handle: handle, path: path}, nil
}

// GetCurrentNS 获取当前**线程**所在的 netns。
// 注意用的是 /proc/self/task/<tid>/ns/net 而不是 /proc/self/ns/net —— 后者
// 拿到的是进程主线程的 netns，在多线程场景里会取错。
func GetCurrentNS() (NetNS, error) {
	path := fmt.Sprintf("/proc/%d/task/%d/ns/net", os.Getpid(), unix.Gettid())
	return GetNS(path)
}

func (n *netNS) Path() string { return n.path }

func (n *netNS) Fd() uintptr { return uintptr(n.handle) }

func (n *netNS) Set() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return fmt.Errorf("ns: %s is already closed", n.path)
	}
	runtime.LockOSThread()
	if err := netns.Set(n.handle); err != nil {
		runtime.UnlockOSThread()
		return err
	}
	return nil
}

// Do 在目标 netns 里执行闭包。
//
// 实现要点：
//  1. 先在主 goroutine 里抓一份"当前 netns"的句柄（hostNS）；
//  2. 真正的切换放在一个新的 goroutine 里做，并且先 LockOSThread；
//  3. 闭包执行完必须先成功切回来，才 UnlockOSThread。
//     —— 如果切回失败，就故意**不解锁**，让 Go 运行时把这条线程整个丢掉，
//     避免一个"netns 脏了"的线程被复用给别的 goroutine。
func (n *netNS) Do(toRun func(hostNS NetNS) error) error {
	if err := n.errorIfClosed(); err != nil {
		return err
	}

	containedCall := func(hostNS NetNS) error {
		threadNS, err := GetCurrentNS()
		if err != nil {
			return fmt.Errorf("ns: cannot open current netns: %v", err)
		}
		defer threadNS.Close()

		n.mu.Lock()
		if n.closed {
			n.mu.Unlock()
			return fmt.Errorf("ns: %s is already closed", n.path)
		}
		runtime.LockOSThread()
		if err := netns.Set(n.handle); err != nil {
			runtime.UnlockOSThread()
			n.mu.Unlock()
			return fmt.Errorf("ns: cannot switch to %s: %v", n.path, err)
		}
		n.mu.Unlock()

		defer func() {
			// 只有成功切回原 netns，才允许释放这条线程
			if err := netns.Set(threadNS.(*netNS).handle); err == nil {
				runtime.UnlockOSThread()
			}
		}()

		return toRun(hostNS)
	}

	// 主 goroutine：保存当前 netns
	hostNS, err := GetCurrentNS()
	if err != nil {
		return fmt.Errorf("ns: cannot open current netns: %v", err)
	}
	defer hostNS.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	var innerError error
	go func() {
		defer wg.Done()
		runtime.LockOSThread()
		innerError = containedCall(hostNS)
	}()
	wg.Wait()

	return innerError
}

func (n *netNS) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil
	}
	n.closed = true
	return unix.Close(int(n.handle))
}

func (n *netNS) errorIfClosed() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return fmt.Errorf("ns: %s is already closed", n.path)
	}
	return nil
}
