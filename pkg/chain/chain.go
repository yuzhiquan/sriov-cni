// Package chain 放 chained 模式下与平台无关的纯逻辑：
// 把"某个网络的覆盖配置"合并进基础配置，以及把副网卡结果拼到 prevResult 上。
package chain

import (
	"encoding/json"
	"fmt"

	types100 "github.com/containernetworking/cni/pkg/types/100"
)

// 合并时从基础配置里剔除的键：它们只对 chained 外壳有意义，
// 留着会让 IPAM 子插件误读（例如 prevResult）或让合并结果再次被当成 chained。
var stripKeys = []string{"prevResult", "plugins", "networks", "mode"}

// Merge 返回 base 叠加 overlay 后的 JSON（顶层键覆盖；ipam 等嵌套对象整体替换）。
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

// Append 把副网卡的结果 sec 拼到主 CNI 的结果 prev 之后，返回新的 Result。
// sec 里 IP 对 Interface 的下标会整体偏移 len(prev.Interfaces)。
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
