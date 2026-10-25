//go:build linux
// +build linux

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CacheDir stores the original state of each VF when a Pod takes it over.
// The name matches upstream sriov-cni to ease troubleshooting when mixing both.
const CacheDir = "/var/lib/cni/sriov"

// SaveCachedConf persists the config (including OrigVfState) to disk for use by DEL.
func SaveCachedConf(containerID, deviceID, ifName string, conf *NetConf) error {
	if err := os.MkdirAll(CacheDir, 0700); err != nil {
		return fmt.Errorf("sriov: cannot create cache dir %s: %v", CacheDir, err)
	}
	data, err := json.Marshal(conf)
	if err != nil {
		return err
	}
	path := cachePath(containerID, deviceID, ifName)
	return os.WriteFile(path, data, 0600)
}

// LoadCachedConf reads the cached config.
func LoadCachedConf(containerID, deviceID, ifName string) (*NetConf, error) {
	path, err := resolveCachePath(containerID, deviceID, ifName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("sriov: no cached state for container %s iface %s", containerID, ifName)
	}
	conf := &NetConf{}
	if err := json.Unmarshal(data, conf); err != nil {
		return nil, err
	}
	return conf, nil
}

// RemoveCachedConf deletes the cache.
func RemoveCachedConf(containerID, deviceID, ifName string) {
	if path, err := resolveCachePath(containerID, deviceID, ifName); err == nil {
		_ = os.Remove(path)
	}
}

// resolveCachePath joins the path directly when deviceID is known; when unknown (DEL in chained mode,
// where the runtime no longer passes deviceID), it globs by containerID and ifName.
func resolveCachePath(containerID, deviceID, ifName string) (string, error) {
	if deviceID != "" {
		return cachePath(containerID, deviceID, ifName), nil
	}
	pattern := filepath.Join(CacheDir, fmt.Sprintf("%s-*-%s.json",
		strings.ReplaceAll(containerID, string(os.PathSeparator), "_"),
		strings.ReplaceAll(ifName, string(os.PathSeparator), "_")))
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf("sriov: no cached state for container %s iface %s", containerID, ifName)
	}
	return matches[0], nil
}

func cachePath(containerID, deviceID, ifName string) string {
	name := fmt.Sprintf("%s-%s-%s.json", containerID, deviceID, ifName)
	// Prevent path traversal
	name = strings.ReplaceAll(name, string(os.PathSeparator), "_")
	return filepath.Join(CacheDir, name)
}
