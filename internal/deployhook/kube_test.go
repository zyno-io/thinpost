package deployhook

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const podListJSON = `{"metadata":{},"items":[
{"metadata":{"name":"a"},"spec":{"containers":[{"name":"control","image":"img@sha256:new"}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
{"metadata":{"name":"b","deletionTimestamp":"2026-11-04T00:00:00Z"},"spec":{"containers":[{"name":"control","image":"img@sha256:old"}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}]}}]}`

func serverCA(server *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}

// unrelatedCA returns a fresh self-signed certificate that signed nothing.
func unrelatedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unrelated"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// kubeEnv writes the CA as certificate as ca.crt and a token file into a
// temp service-account directory and returns an env lookup for it.
func kubeEnv(t *testing.T, server *httptest.Server, pemBytes []byte) (string, func(string) string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("token-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	return dir, func(name string) string {
		return map[string]string{"KUBERNETES_SERVICE_HOST": host, "KUBERNETES_SERVICE_PORT": port}[name]
	}
}

// testClient builds the client through NewInCluster, trusting the test
// server's own certificate as the cluster CA.
func testClient(t *testing.T, handler http.HandlerFunc) (*Client, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	dir, getenv := kubeEnv(t, server, serverCA(server))
	client, err := NewInCluster(dir, getenv)
	if err != nil {
		t.Fatal(err)
	}
	return client, filepath.Join(dir, "token")
}

func TestClientRefusesRedirectsAndUntrustedCertificates(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(podListJSON))
	})
	if _, err := client.ListPods(context.Background(), "ns", "a=b"); err == nil {
		t.Fatal("followed or accepted a redirect")
	}
	mu.Lock()
	if len(paths) != 1 {
		t.Fatalf("redirect was followed: %v", paths)
	}
	mu.Unlock()

	// A server whose certificate is not signed by the configured CA is refused.
	var hit bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(podListJSON))
	}))
	defer server.Close()
	dir, getenv := kubeEnv(t, server, unrelatedCA(t))
	untrusted, err := NewInCluster(dir, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = untrusted.ListPods(context.Background(), "ns", "a=b"); err == nil || hit {
		t.Fatal("accepted an untrusted server certificate", err, hit)
	}
}

func TestListPodsParsesAndRereadsToken(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	var queries []string
	client, token := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		_, _ = w.Write([]byte(podListJSON))
	})
	pods, err := client.ListPods(context.Background(), "thinpost-control", "app.kubernetes.io/name=thinpost-operator")
	if err != nil || len(pods) != 2 {
		t.Fatal(pods, err)
	}
	if !pods[0].Ready || pods[0].Deleting || pods[0].Images["control"] != "img@sha256:new" || !pods[1].Deleting || pods[1].Ready {
		t.Fatalf("unexpected parse: %+v", pods)
	}
	if err = os.WriteFile(token, []byte("token-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = client.ListPods(context.Background(), "thinpost-control", "a=b"); err != nil {
		t.Fatal(err)
	}
	if seen[0] != "Bearer token-one" || seen[1] != "Bearer token-two" {
		t.Fatalf("token was not re-read: %v", seen)
	}
	if !strings.HasPrefix(queries[0], "/api/v1/namespaces/thinpost-control/pods?") || !strings.Contains(queries[0], "labelSelector=app.kubernetes.io%2Fname%3Dthinpost-operator") {
		t.Fatal(queries[0])
	}
}

func TestListPodsFailuresAreFixedText(t *testing.T) {
	secret := "secret-body-text"
	cases := map[string]http.HandlerFunc{
		"status":    func(w http.ResponseWriter, r *http.Request) { http.Error(w, secret, http.StatusForbidden) },
		"malformed": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{" + secret)) },
		"paged": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"metadata":{"continue":"x"},"items":[]}`))
		},
		"oversize": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"x":"` + strings.Repeat("a", maxKubeResponse) + `"}`))
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
	}
	for name, handler := range cases {
		client, _ := testClient(t, handler)
		_, err := client.ListPods(context.Background(), "ns", "a=b")
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	client, _ := testClient(t, func(http.ResponseWriter, *http.Request) {})
	if _, err := client.ListPods(context.Background(), "Bad NS", "a=b"); err == nil {
		t.Fatal("accepted an invalid namespace")
	}
	if _, err := client.ListPods(context.Background(), "ns", "a=b&limit=1"); err == nil {
		t.Fatal("accepted an invalid selector")
	}
	client.TokenPath = filepath.Join(t.TempDir(), "missing")
	if _, err := client.ListPods(context.Background(), "ns", "a=b"); err == nil {
		t.Fatal("accepted a missing token")
	}
}

func TestRolloutComplete(t *testing.T) {
	const image = "img@sha256:new"
	ready := Pod{Phase: "Running", Ready: true, Images: map[string]string{"control": image}}
	old := Pod{Phase: "Running", Ready: true, Images: map[string]string{"control": "img@sha256:old"}}
	terminating := ready
	terminating.Deleting = true
	unready := Pod{Phase: "Running", Images: map[string]string{"control": image}}
	pending := Pod{Phase: "Pending", Images: map[string]string{"control": image}}
	unknown := Pod{Phase: "Unknown", Images: map[string]string{"control": "img@sha256:old"}}
	failed := Pod{Phase: "Failed", Images: map[string]string{"control": "img@sha256:old"}}
	cases := []struct {
		name string
		pods []Pod
		want bool
	}{
		{"two ready on new image", []Pod{ready, ready}, true},
		{"terminal old pod ignored", []Pod{ready, ready, failed}, true},
		{"old pod remains", []Pod{ready, ready, old}, false},
		{"terminating pod", []Pod{ready, ready, terminating}, false},
		{"unknown phase", []Pod{ready, ready, unknown}, false},
		{"unready", []Pod{ready, unready}, false},
		{"pending extra", []Pod{ready, ready, pending}, false},
		{"too few", []Pod{ready}, false},
		{"too many", []Pod{ready, ready, ready}, false},
		{"none", nil, false},
	}
	for _, c := range cases {
		if got := RolloutComplete(c.pods, "control", image, 2); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestNewInClusterRequiresCA(t *testing.T) {
	dir := t.TempDir()
	getenv := func(name string) string {
		return map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1", "KUBERNETES_SERVICE_PORT": "443"}[name]
	}
	if _, err := NewInCluster(dir, getenv); err == nil {
		t.Fatal("accepted a missing CA")
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewInCluster(dir, getenv); err == nil {
		t.Fatal("accepted an invalid CA")
	}
	if _, err := NewInCluster(dir, func(string) string { return "" }); err == nil {
		t.Fatal("accepted a missing service address")
	}
}
