//go:build linux
// +build linux

package config

import "fmt"

// VF 链路状态的取值，与 Linux 内核 include/uapi/linux/if_link.h 中
// IFLA_VF_LINK_STATE_{AUTO,ENABLE,DISABLE} 一一对应。
const (
	LinkStateAuto    uint32 = 0
	LinkStateEnable  uint32 = 1
	LinkStateDisable uint32 = 2
)

// parseLinkState 把配置里的字符串解析成内核使用的数值。
func parseLinkState(s string) (uint32, error) {
	switch s {
	case "auto":
		return LinkStateAuto, nil
	case "enable":
		return LinkStateEnable, nil
	case "disable":
		return LinkStateDisable, nil
	}
	return 0, fmt.Errorf("sriov: invalid link_state %q, expect auto|enable|disable", s)
}

// ParseLinkState 供其他包使用。
func ParseLinkState(s string) (uint32, error) {
	return parseLinkState(s)
}

// LinkStateString 把数值转回可读字符串（用于日志/缓存）。
func LinkStateString(v uint32) string {
	switch v {
	case LinkStateAuto:
		return "auto"
	case LinkStateEnable:
		return "enable"
	case LinkStateDisable:
		return "disable"
	}
	return "unknown"
}

// ParseOnOff 供其他包使用。
func ParseOnOff(name, v string) (bool, error) {
	return parseOnOff(name, v)
}
