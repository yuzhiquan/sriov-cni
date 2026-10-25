package chain

import (
	"encoding/json"
	"net"
	"testing"

	types100 "github.com/containernetworking/cni/pkg/types/100"
)

func TestMergeOverlaysAndStrips(t *testing.T) {
	base := `{"cniVersion":"1.0.0","name":"n","type":"sriov","mode":"chained",
	  "prevResult":{"x":1},"networks":{"a":{}},"vlan":1,"ipam":{"type":"host-local"}}`
	got, err := Merge([]byte(base), json.RawMessage(`{"vlan":1000,"ipam":{"type":"whereabouts"}}`))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"prevResult", "networks", "mode", "plugins"} {
		if _, ok := m[k]; ok {
			t.Errorf("key %q should have been stripped", k)
		}
	}
	if string(m["vlan"]) != "1000" {
		t.Errorf("vlan = %s, want overlay value 1000", m["vlan"])
	}
	if string(m["ipam"]) != `{"type":"whereabouts"}` {
		t.Errorf("ipam = %s, want whole-object replacement", m["ipam"])
	}
	if string(m["name"]) != `"n"` {
		t.Errorf("name lost: %s", m["name"])
	}
}

func TestMergeBadJSON(t *testing.T) {
	if _, err := Merge([]byte(`{`), nil); err == nil {
		t.Error("want error for bad base")
	}
	if _, err := Merge([]byte(`{}`), json.RawMessage(`[1]`)); err == nil {
		t.Error("want error for bad overlay")
	}
}

func TestAppendOffsetsInterfaceIndex(t *testing.T) {
	_, n1, _ := net.ParseCIDR("10.0.0.5/24")
	_, n2, _ := net.ParseCIDR("192.168.100.7/24")
	zero, one := 0, 0
	prev := &types100.Result{
		CNIVersion: "1.0.0",
		Interfaces: []*types100.Interface{{Name: "eth0"}},
		IPs:        []*types100.IPConfig{{Address: *n1, Interface: &zero}},
	}
	sec := &types100.Result{
		Interfaces: []*types100.Interface{{Name: "net1"}},
		IPs:        []*types100.IPConfig{{Address: *n2, Interface: &one}},
	}
	out := Append(prev, sec)

	if len(out.Interfaces) != 2 || out.Interfaces[1].Name != "net1" {
		t.Fatalf("interfaces = %+v", out.Interfaces)
	}
	if len(out.IPs) != 2 || *out.IPs[0].Interface != 0 || *out.IPs[1].Interface != 1 {
		t.Errorf("interface indexes not offset: %d %d", *out.IPs[0].Interface, *out.IPs[1].Interface)
	}
	if len(prev.Interfaces) != 1 || len(prev.IPs) != 1 || *sec.IPs[0].Interface != 0 {
		t.Error("inputs were mutated")
	}
}
