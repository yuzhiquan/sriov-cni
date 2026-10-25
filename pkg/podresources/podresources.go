// Package podresources uses the kubelet PodResources gRPC API
// to look up which devices the Device Plugin allocated to a given Pod.
//
// This is what Multus (thick) does when injecting deviceID:
//
//	kubelet Admit Pod -> Device Plugin Allocate() -> records the allocation
//	                  -> creates sandbox -> CNI ADD   <- List() can see it by now
//
// sriov-network-device-plugin reports device IDs that are the VF PCI addresses.
package podresources

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	podresourcesv1 "k8s.io/kubelet/pkg/apis/podresources/v1"
)

// DefaultSocket is the default kubelet PodResources socket path.
const DefaultSocket = "/var/lib/kubelet/pod-resources/kubelet.sock"

// FindDevices returns all device IDs allocated to the pod under the resource name.
// An empty resource disables resource-name filtering (returns all devices of the Pod).
// Callers should bound the total time via ctx.
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

// WaitForDevices retries FindDevices within timeout until at least one device is found.
// Retrying tolerates the brief window after kubelet admission before the PodResources cache is refreshed.
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
