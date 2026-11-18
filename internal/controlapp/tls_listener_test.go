package controlapp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePair(t *testing.T, dir, name string) (cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	// Write via rename so the replacement has a new identity, like a Secret volume swap.
	put := func(file string, block *pem.Block) {
		tmp := filepath.Join(dir, file+".tmp")
		if err := os.WriteFile(tmp, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
	}
	put("tls.crt", &pem.Block{Type: "CERTIFICATE", Bytes: der})
	put("tls.key", &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	parsed, _ := x509.ParseCertificate(der)
	return parsed
}

func startTLS(t *testing.T, dir string) (string, *CertReloader) {
	t.Helper()
	certs, err := NewCertReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	server := NewTLSServer("127.0.0.1:0", mux, certs)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	return listener.Addr().String(), certs
}

func dial(t *testing.T, addr string, version uint16) (*x509.Certificate, error) {
	t.Helper()
	// MinVersion is lowered explicitly: Go clients refuse below TLS 1.2 by
	// default, which would make an old-version dial fail client-side without
	// ever testing the server's own minimum.
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10, MaxVersion: version})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0], nil
}

func TestTLSListenerServesHandlerVerifiedAgainstCA(t *testing.T) {
	dir := t.TempDir()
	cert := writePair(t, dir, "one")
	addr, _ := startTLS(t, dir)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	response, err := client.Get("https://" + addr + "/livez")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
}

func TestTLSListenerRefusesTLS11AndReloadsReplacedCertificate(t *testing.T) {
	dir := t.TempDir()
	first := writePair(t, dir, "one")
	addr, _ := startTLS(t, dir)
	if _, err := dial(t, addr, tls.VersionTLS11); err == nil {
		t.Fatal("TLS 1.1 accepted")
	}
	got, err := dial(t, addr, tls.VersionTLS13)
	if err != nil || got.SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatalf("first certificate not served: %v", err)
	}
	second := writePair(t, dir, "two")
	got, err = dial(t, addr, tls.VersionTLS13)
	if err != nil || got.SerialNumber.Cmp(second.SerialNumber) != 0 {
		t.Fatalf("renewed certificate not served: %v", err)
	}
}

func TestTLSListenerKeepsServingWhenReplacementIsInvalid(t *testing.T) {
	dir := t.TempDir()
	first := writePair(t, dir, "one")
	addr, _ := startTLS(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "tls.crt.tmp"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "tls.crt.tmp"), filepath.Join(dir, "tls.crt")); err != nil {
		t.Fatal(err)
	}
	got, err := dial(t, addr, tls.VersionTLS13)
	if err != nil || got.SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatalf("previous certificate not retained: %v", err)
	}
}

func TestCertReloaderFailsClosedOnBadConfiguration(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewCertReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")); err == nil {
		t.Fatal("missing files accepted")
	}
	writePair(t, dir, "one")
	other := t.TempDir()
	writePair(t, other, "two")
	if _, err := NewCertReloader(filepath.Join(dir, "tls.crt"), filepath.Join(other, "tls.key")); err == nil {
		t.Fatal("mismatched key accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCertReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")); err == nil {
		t.Fatal("invalid key accepted")
	}
}

func TestControlTLSConfiguration(t *testing.T) {
	base := env()
	cfg, err := load(base)
	if err != nil || cfg.TLSListen != "" {
		t.Fatalf("TLS must be off by default: %v %q", err, cfg.TLSListen)
	}
	with := func(kv map[string]string) map[string]string {
		m := env()
		for k, v := range kv {
			m[k] = v
		}
		return m
	}
	cfg, err = load(with(map[string]string{"THINPOST_CONTROL_TLS_CERT": "/run/tls/tls.crt", "THINPOST_CONTROL_TLS_KEY": "/run/tls/tls.key"}))
	if err != nil || cfg.TLSListen != ":8443" {
		t.Fatalf("expected default :8443: %v %q", err, cfg.TLSListen)
	}
	for name, kv := range map[string]map[string]string{
		"listen only":   {"THINPOST_CONTROL_TLS_LISTEN": ":8443"},
		"cert only":     {"THINPOST_CONTROL_TLS_CERT": "/run/tls/tls.crt"},
		"relative":      {"THINPOST_CONTROL_TLS_CERT": "tls.crt", "THINPOST_CONTROL_TLS_KEY": "tls.key"},
		"same as plain": {"THINPOST_LISTEN": ":8443", "THINPOST_CONTROL_TLS_LISTEN": ":8443", "THINPOST_CONTROL_TLS_CERT": "/a", "THINPOST_CONTROL_TLS_KEY": "/b"},
	} {
		if _, err := load(with(kv)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
