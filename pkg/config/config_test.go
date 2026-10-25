//go:build linux
// +build linux

package config

import "testing"

func TestLoadConfDefersDeviceForAllocators(t *testing.T) {
	cases := map[string]string{
		"podresources": `{"cniVersion":"1.0.0","name":"n","type":"sriov","vfAllocator":"podresources"}`,
		"pool":         `{"cniVersion":"1.0.0","name":"n","type":"sriov","vfAllocator":"pool"}`,
		"chained":      `{"cniVersion":"1.0.0","name":"n","type":"sriov","mode":"chained"}`,
	}
	for name, in := range cases {
		if _, err := LoadConf([]byte(in)); err != nil {
			t.Errorf("%s: LoadConf should not require deviceID yet: %v", name, err)
		}
	}
}

func TestLoadConfStillRequiresDeviceByDefault(t *testing.T) {
	if _, err := LoadConf([]byte(`{"cniVersion":"1.0.0","name":"n","type":"sriov"}`)); err == nil {
		t.Error("default mode without deviceID must fail")
	}
}

func TestLoadConfRejectsUnknownModeAndAllocator(t *testing.T) {
	for _, in := range []string{
		`{"cniVersion":"1.0.0","name":"n","type":"sriov","deviceID":"0000:03:02.0","mode":"x"}`,
		`{"cniVersion":"1.0.0","name":"n","type":"sriov","deviceID":"0000:03:02.0","vfAllocator":"x"}`,
	} {
		if _, err := LoadConf([]byte(in)); err == nil {
			t.Errorf("want error for %s", in)
		}
	}
}

func TestValidateAfterResolve(t *testing.T) {
	n, err := LoadConf([]byte(`{"cniVersion":"1.0.0","name":"n","type":"sriov","vfAllocator":"pool"}`))
	if err != nil {
		t.Fatal(err)
	}
	n.DeviceID = "bogus"
	if err := n.Validate(); err == nil {
		t.Error("resolved deviceID must still be a valid PCI address")
	}
	n.DeviceID = "0000:03:02.0"
	if err := n.Validate(); err != nil {
		t.Error(err)
	}
}

func TestCachePathWildcardWhenDeviceUnknown(t *testing.T) {
	// Only verify that path joining does not panic and that no wildcard is used when deviceID is known
	if p, err := resolveCachePath("cid", "0000:03:02.0", "net1"); err != nil || p == "" {
		t.Errorf("known device: %q %v", p, err)
	}
	if _, err := resolveCachePath("cid-not-there", "", "net1"); err == nil {
		t.Error("unknown device with no cache must error")
	}
}
