package podresources

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	podresourcesv1 "k8s.io/kubelet/pkg/apis/podresources/v1"
)

type fakeLister struct {
	podresourcesv1.UnimplementedPodResourcesListerServer
	resp *podresourcesv1.ListPodResourcesResponse
}

func (f *fakeLister) List(context.Context, *podresourcesv1.ListPodResourcesRequest) (*podresourcesv1.ListPodResourcesResponse, error) {
	return f.resp, nil
}

func serve(t *testing.T, resp *podresourcesv1.ListPodResourcesResponse) string {
	t.Helper()
	// unix socket paths have a length limit; don't use the long path from t.TempDir()
	dir, err := os.MkdirTemp("/tmp", "pr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "k.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	podresourcesv1.RegisterPodResourcesListerServer(s, &fakeLister{resp: resp})
	go s.Serve(l)
	t.Cleanup(s.Stop)
	return sock
}

func pod(name, ns string, devs ...*podresourcesv1.ContainerDevices) *podresourcesv1.PodResources {
	return &podresourcesv1.PodResources{Name: name, Namespace: ns,
		Containers: []*podresourcesv1.ContainerResources{{Name: "c", Devices: devs}}}
}

func TestFindDevicesFiltersByPodAndResource(t *testing.T) {
	sock := serve(t, &podresourcesv1.ListPodResourcesResponse{PodResources: []*podresourcesv1.PodResources{
		pod("other", "demo", &podresourcesv1.ContainerDevices{ResourceName: "intel.com/sriov", DeviceIds: []string{"0000:03:02.9"}}),
		pod("p1", "demo",
			&podresourcesv1.ContainerDevices{ResourceName: "intel.com/sriov", DeviceIds: []string{"0000:03:02.1", "0000:03:02.2"}},
			&podresourcesv1.ContainerDevices{ResourceName: "nvidia.com/gpu", DeviceIds: []string{"GPU-1"}}),
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := FindDevices(ctx, sock, "p1", "demo", "intel.com/sriov")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "0000:03:02.1" || got[1] != "0000:03:02.2" {
		t.Errorf("got %v", got)
	}
}

func TestWaitForDevicesTimesOutWhenNothingAllocated(t *testing.T) {
	sock := serve(t, &podresourcesv1.ListPodResourcesResponse{})
	if _, err := WaitForDevices(sock, "p1", "demo", "intel.com/sriov", 700*time.Millisecond); err == nil {
		t.Error("want error when the pod has no device")
	}
}

func TestFindDevicesNoSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := FindDevices(ctx, "/nonexistent/k.sock", "p", "ns", ""); err == nil {
		t.Error("want connect error")
	}
}
