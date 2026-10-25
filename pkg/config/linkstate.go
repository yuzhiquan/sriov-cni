//go:build linux
// +build linux

package config

import "fmt"

// VF link state values, which correspond one-to-one with
// IFLA_VF_LINK_STATE_{AUTO,ENABLE,DISABLE} in the Linux kernel's include/uapi/linux/if_link.h.
const (
	LinkStateAuto    uint32 = 0
	LinkStateEnable  uint32 = 1
	LinkStateDisable uint32 = 2
)

// parseLinkState parses the string from config into the numeric value used by the kernel.
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

// ParseLinkState is for use by other packages.
func ParseLinkState(s string) (uint32, error) {
	return parseLinkState(s)
}

// LinkStateString converts the numeric value back to a readable string (for logs/cache).
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

// ParseOnOff is for use by other packages.
func ParseOnOff(name, v string) (bool, error) {
	return parseOnOff(name, v)
}
