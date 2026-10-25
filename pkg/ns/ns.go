//go:build linux
// +build linux

// Package ns provides the ability to enter and leave network namespaces.
//
// Why threads must be handled carefully:
// setns(2) affects **only the calling thread**, while Go goroutines are moved between OS threads
// by the scheduler. If a goroutine is not locked to its thread, you can get odd behavior where the first half runs in the Pod
// netns and the second half suddenly runs in the host netns. This is why
// runtime.LockOSThread() is everywhere in CNI plugins.
package ns

import (
	"fmt"
	"os"
	"runtime"
	"sync"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// NetNS abstracts a handle to a network namespace.
type NetNS interface {
	// Path returns the path of this netns under /proc or /var/run/netns
	Path() string
	// Fd returns an fd usable for setns / netlink operations
	Fd() uintptr
	// Set switches the current thread into this netns
	Set() error
	// Do runs a closure inside this netns, passing the "original" netns to it
	// so the closure can move devices back.
	Do(toRun func(hostNS NetNS) error) error
	// Close releases the handle
	Close() error
}

type netNS struct {
	handle netns.NsHandle
	path   string
	closed bool
	mu     sync.Mutex
}

// GetNS opens a netns by path.
// The CNI_NETNS that container runtimes pass to CNI is usually /proc/<pid>/ns/net or /var/run/netns/<name>.
func GetNS(path string) (NetNS, error) {
	handle, err := netns.GetFromPath(path)
	if err != nil {
		return nil, fmt.Errorf("ns: cannot open netns %q: %v", path, err)
	}
	return &netNS{handle: handle, path: path}, nil
}

// GetCurrentNS returns the netns of the current **thread**.
// Note it uses /proc/self/task/<tid>/ns/net rather than /proc/self/ns/net -- the latter
// returns the netns of the process main thread, which is wrong in multi-threaded scenarios.
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

// Do runs a closure in the target netns.
//
// Key points of the implementation:
//  1. In the main goroutine, first grab a handle to the "current netns" (hostNS);
//  2. Do the actual switch in a new goroutine, calling LockOSThread first;
//  3. Only call UnlockOSThread after successfully switching back once the closure finishes.
//     -- If switching back fails, deliberately **do not unlock**, so the Go runtime discards the whole thread
//     and a thread with a "dirty netns" is never reused by another goroutine.
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
			// Only allow releasing this thread after successfully switching back to the original netns
			if err := netns.Set(threadNS.(*netNS).handle); err == nil {
				runtime.UnlockOSThread()
			}
		}()

		return toRun(hostNS)
	}

	// Main goroutine: save the current netns
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
