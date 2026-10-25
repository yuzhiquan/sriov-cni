// Package k8s provides the minimal Kubernetes access needed by chained mode:
// reading Pod annotations by the Pod name in CNI_ARGS.
//
// client-go is deliberately not used: the CNI is a short-lived process and only needs one GET with CA/Token.
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

// Conf describes how to reach the API server. The CNI on the node is not a Pod and has no in-cluster env vars,
// so it must be configured explicitly. Generate the Token from a ServiceAccount with only pods/get permission.
type Conf struct {
	APIServer string `json:"apiServer"` // e.g. https://10.0.0.1:6443
	TokenFile string `json:"tokenFile"` // file containing the Bearer token
	CAFile    string `json:"caFile"`    // API server CA; system roots are used if empty
	TimeoutMs int    `json:"timeoutMs"` // default 5000
}

// PodArgs is the Pod identity passed by kubelet/container runtime via CNI_ARGS.
type PodArgs struct {
	types.CommonArgs
	// types.LoadArgs matches CNI_ARGS keys by field name (not json tag), so the original names must be used.
	K8S_POD_NAME      types.UnmarshallableString //nolint:revive,stylecheck
	K8S_POD_NAMESPACE types.UnmarshallableString //nolint:revive,stylecheck
}

// ParsePodArgs parses CNI_ARGS and returns the Pod name and namespace.
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

// GetPodAnnotations reads metadata.annotations of the given Pod.
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
