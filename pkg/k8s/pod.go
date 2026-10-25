// Package k8s 提供 chained 模式需要的最小 Kubernetes 访问能力：
// 按 CNI_ARGS 里的 Pod 名读取 Pod 注解。
//
// 不引入 client-go：CNI 是一次性短进程，只需要一个带 CA/Token 的 GET。
package k8s

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/types"
)

// Conf 描述如何访问 API server。节点上的 CNI 不是 Pod，没有 in-cluster 环境变量，
// 所以必须显式配置。Token 建议用只有 pods/get 权限的 ServiceAccount 生成。
type Conf struct {
	APIServer string `json:"apiServer"` // 如 https://10.0.0.1:6443
	TokenFile string `json:"tokenFile"` // 含 Bearer token 的文件
	CAFile    string `json:"caFile"`    // API server CA；为空则使用系统根证书
	TimeoutMs int    `json:"timeoutMs"` // 默认 5000
}

// PodArgs 是 kubelet/容器运行时通过 CNI_ARGS 传入的 Pod 标识。
type PodArgs struct {
	types.CommonArgs
	// types.LoadArgs 按字段名（而非 json tag）匹配 CNI_ARGS 的键，所以必须用原名。
	K8S_POD_NAME      types.UnmarshallableString //nolint:revive,stylecheck
	K8S_POD_NAMESPACE types.UnmarshallableString //nolint:revive,stylecheck
}

// ParsePodArgs 解析 CNI_ARGS，返回 Pod 名与命名空间。
func ParsePodArgs(cniArgs string) (name, namespace string, err error) {
	a := PodArgs{}
	if err = types.LoadArgs(cniArgs, &a); err != nil {
		return "", "", fmt.Errorf("k8s: cannot parse CNI_ARGS: %v", err)
	}
	name, namespace = string(a.K8S_POD_NAME), string(a.K8S_POD_NAMESPACE)
	if name == "" || namespace == "" {
		return "", "", fmt.Errorf("k8s: CNI_ARGS lacks K8S_POD_NAME/K8S_POD_NAMESPACE (%q)", cniArgs)
	}
	return name, namespace, nil
}

// GetPodAnnotations 读取指定 Pod 的 metadata.annotations。
func GetPodAnnotations(c *Conf, namespace, name string) (map[string]string, error) {
	if c == nil || c.APIServer == "" {
		return nil, fmt.Errorf(`k8s: chained mode needs "kubernetes.apiServer" in the CNI config`)
	}
	timeout := time.Duration(c.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("k8s: read caFile: %v", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("k8s: no certificate in %s", c.CAFile)
		}
		tlsCfg.RootCAs = pool
	}

	u := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s",
		strings.TrimRight(c.APIServer, "/"), url.PathEscape(namespace), url.PathEscape(name))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.TokenFile != "" {
		tok, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("k8s: read tokenFile: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("k8s: GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("k8s: GET %s: %s: %s", u, resp.Status, strings.TrimSpace(string(body)))
	}

	var pod struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &pod); err != nil {
		return nil, fmt.Errorf("k8s: decode pod: %v", err)
	}
	return pod.Metadata.Annotations, nil
}
