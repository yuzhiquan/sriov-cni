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

// CacheDir 保存每个 Pod 接管 VF 时的原始状态。
// 名称与上游 sriov-cni 保持一致，便于混用排查。
const CacheDir = "/var/lib/cni/sriov"

// SaveCachedConf 把（含 OrigVfState 的）配置落盘，供 DEL 使用。
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

// LoadCachedConf 读取缓存配置。
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

// RemoveCachedConf 删除缓存。
func RemoveCachedConf(containerID, deviceID, ifName string) {
	if path, err := resolveCachePath(containerID, deviceID, ifName); err == nil {
		_ = os.Remove(path)
	}
}

// resolveCachePath 在 deviceID 已知时直接拼路径；未知时（chained 模式的 DEL，
// runtime 不会再给 deviceID）按 containerID 和 ifName 通配查找。
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
	// 防止路径穿越
	name = strings.ReplaceAll(name, string(os.PathSeparator), "_")
	return filepath.Join(CacheDir, name)
}
