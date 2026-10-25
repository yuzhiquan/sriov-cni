// Package chain holds the platform-independent pure logic for chained mode:
// merging a network's override config into the base config, and appending the secondary NIC's result onto prevResult.
package chain

import (
	"encoding/json"
	"fmt"

	types100 "github.com/containernetworking/cni/pkg/types/100"
)

// Keys removed from the base config when merging: they only matter to the chained shell,
// and keeping them would make IPAM sub-plugins misread them (e.g. prevResult) or cause the merged result to be treated as chained again.
var stripKeys = []string{"prevResult", "plugins", "networks", "mode"}

// Merge returns the JSON of base overlaid with overlay (top-level keys override; nested objects such as ipam are replaced wholesale).
func Merge(base []byte, overlay json.RawMessage) ([]byte, error) {
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, fmt.Errorf("chain: parse base config: %v", err)
	}
	for _, k := range stripKeys {
		delete(m, k)
	}
	if len(overlay) > 0 {
		o := map[string]json.RawMessage{}
		if err := json.Unmarshal(overlay, &o); err != nil {
			return nil, fmt.Errorf("chain: parse network config: %v", err)
		}
		for k, v := range o {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

// Append appends the secondary NIC's result sec after the primary CNI's result prev and returns a new Result.
// The Interface indexes referenced by IPs in sec are shifted as a whole by len(prev.Interfaces).
func Append(prev, sec *types100.Result) *types100.Result {
	out := *prev
	out.Interfaces = append(append([]*types100.Interface{}, prev.Interfaces...), sec.Interfaces...)
	off := len(prev.Interfaces)

	out.IPs = append([]*types100.IPConfig{}, prev.IPs...)
	for _, ip := range sec.IPs {
		c := *ip
		if c.Interface != nil {
			idx := *c.Interface + off
			c.Interface = &idx
		}
		out.IPs = append(out.IPs, &c)
	}
	out.Routes = append(append(out.Routes[:0:0], prev.Routes...), sec.Routes...)
	if len(out.DNS.Nameservers) == 0 && out.DNS.Domain == "" {
		out.DNS = sec.DNS
	}
	return &out
}
