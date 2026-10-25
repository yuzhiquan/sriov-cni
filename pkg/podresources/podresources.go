// Package podresources 通过 kubelet 的 PodResources gRPC 接口，
// 查出"Device Plugin 把哪些设备分配给了某个 Pod"。
//
// 这正是 Multus（thick 版）注入 deviceID 时做的事：
//
//	kubelet Admit Pod -> Device Plugin Allocate() -> 记录分配结果
//	                  -> 创建 sandbox -> CNI ADD   <- 此时 List() 已能查到
//
// sriov-network-device-plugin 上报的 device ID 就是 VF 的 PCI 地址。
package podresources

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	podresourcesv1 "k8s.io/kubelet/pkg/apis/podresources/v1"
)

// DefaultSocket 是 kubelet 默认的 PodResources socket 路径。
const DefaultSocket = "/var/lib/kubelet/pod-resources/kubelet.sock"

// FindDevices 返回 pod 在 resource 这个资源名下被分配到的全部设备 ID。
// resource 为空表示不过滤资源名（返回该 Pod 所有设备）。
// 调用方应通过 ctx 控制总超时。
func FindDevices(ctx context.Context, socket, podName, podNamespace, resource string) ([]string, error) {
	if socket == "" {
		socket = DefaultSocket
	}
	conn, err := grpc.DialContext(ctx, "unix://"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock())
	if err != nil {
		return nil, fmt.Errorf("podresources: cannot connect to %s: %v", socket, err)
	}
	defer conn.Close()

	resp, err := podresourcesv1.NewPodResourcesListerClient(conn).
		List(ctx, &podresourcesv1.ListPodResourcesRequest{})
	if err != nil {
		return nil, fmt.Errorf("podresources: List failed: %v", err)
	}

	var ids []string
	for _, pod := range resp.GetPodResources() {
		if pod.GetName() != podName || pod.GetNamespace() != podNamespace {
			continue
		}
		for _, c := range pod.GetContainers() {
			for _, d := range c.GetDevices() {
				if resource == "" || d.GetResourceName() == resource {
					ids = append(ids, d.GetDeviceIds()...)
				}
			}
		}
	}
	return ids, nil
}

// WaitForDevices 在 timeout 内重试 FindDevices，直到查到至少一个设备。
// 重试是为了容忍 kubelet 刚完成准入、PodResources 缓存尚未刷新的短暂窗口。
func WaitForDevices(socket, podName, podNamespace, resource string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var lastErr error
	for {
		ids, err := FindDevices(ctx, socket, podName, podNamespace, resource)
		if err == nil && len(ids) > 0 {
			return ids, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("podresources: no %q device allocated to pod %s/%s",
				resource, podNamespace, podName)
		case <-time.After(300 * time.Millisecond):
		}
	}
}
