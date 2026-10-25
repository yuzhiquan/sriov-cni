package k8s

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParsePodArgs(t *testing.T) {
	n, ns, err := ParsePodArgs("IgnoreUnknown=1;K8S_POD_NAMESPACE=demo;K8S_POD_NAME=p1;K8S_POD_INFRA_CONTAINER_ID=abc")
	if err != nil || n != "p1" || ns != "demo" {
		t.Fatalf("got %q %q %v", n, ns, err)
	}
	if _, _, err := ParsePodArgs("IgnoreUnknown=1"); err == nil {
		t.Error("want error when pod identity is missing")
	}
}

func TestGetPodAnnotations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/demo/pods/p1" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"metadata":{"annotations":{"sriov.example.com/network":"vlan1000"}}}`))
	}))
	defer srv.Close()

	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := GetPodAnnotations(&Conf{APIServer: srv.URL, TokenFile: tok}, "demo", "p1")
	if err != nil || got["sriov.example.com/network"] != "vlan1000" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := GetPodAnnotations(&Conf{APIServer: srv.URL}, "demo", "p1"); err == nil {
		t.Error("want error without a token (403)")
	}
	if _, err := GetPodAnnotations(&Conf{APIServer: srv.URL, TokenFile: tok}, "demo", "missing"); err == nil {
		t.Error("want error for 404")
	}
	if _, err := GetPodAnnotations(nil, "demo", "p1"); err == nil {
		t.Error("want error without apiServer config")
	}
}
