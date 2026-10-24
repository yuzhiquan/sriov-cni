//go:build linux
// +build linux

package config

import "regexp"

// pciAddrRe 匹配 SR-IOV VF 的地址格式：Domain:Bus:Device.Function
// 例如 0000:03:02.0（Function 因为 ARI 可以到 255，所以不固定为 0.7）
var pciAddrRe = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]{1,2}$`)
