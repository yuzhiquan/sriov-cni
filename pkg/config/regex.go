//go:build linux
// +build linux

package config

import "regexp"

// pciAddrRe matches the SR-IOV VF address format: Domain:Bus:Device.Function
// e.g. 0000:03:02.0 (Function can go up to 255 with ARI, so it is not fixed to 0.7)
var pciAddrRe = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]{1,2}$`)
