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
	path := cachePath(containerID, deviceID, ifName)
	data, err := os.ReadFile(path)
	if err != nil {
		// 兼容：deviceID 未知时用前缀匹配
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
	_ = os.Remove(cachePath(containerID, deviceID, ifName))
}

func cachePath(containerID, deviceID, ifName string) string {
	name := fmt.Sprintf("%s-%s-%s.json", containerID, deviceID, ifName)
	// 防止路径穿越
	name = strings.ReplaceAll(name, string(os.PathSeparator), "_")
	return filepath.Join(CacheDir, name)
}
