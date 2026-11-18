package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPublicSMTPRotatesAndFailsClosed(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	controlKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, name string, key *ecdsa.PrivateKey, expiry time.Time) ([]byte, []byte, []byte) {
		t.Helper()
		if key == nil {
			key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{name}, NotBefore: now.Add(-time.Hour), NotAfter: expiry, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return der, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	}
	_, controlCert, _ := issue(2, "control.example", controlKey, now.Add(time.Hour))
	_, firstCert, firstKey := issue(3, "smtp.example", nil, now.Add(time.Hour))
	_, nextCert, nextKey := issue(4, "smtp.example", nil, now.Add(time.Hour))
	_, wrongCert, wrongKey := issue(5, "wrong.example", nil, now.Add(time.Hour))
	_, reusedCert, reusedKey := issue(6, "smtp.example", controlKey, now.Add(time.Hour))
	_, expiredCert, expiredKey := issue(7, "smtp.example", nil, now.Add(-time.Minute))
	dir := t.TempDir()
	controlPath := filepath.Join(dir, "control.crt")
	if err := os.WriteFile(controlPath, controlCert, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	rotate := func(version string, cert, key []byte) {
		t.Helper()
		versionDir := filepath.Join(dir, version)
		if err := os.Mkdir(versionDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(versionDir, "tls.crt"), cert, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(versionDir, "tls.key"), key, 0600); err != nil {
			t.Fatal(err)
		}
		next := filepath.Join(dir, "..next")
		if err := os.Symlink(version, next); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	rotate("one", firstCert, firstKey)
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if _, err := publicSMTPCertificate(certPath, keyPath, "smtp.example", controlPath, x509.NewCertPool()); err == nil {
		t.Fatal("startup accepted an untrusted certificate")
	}
	if _, err := publicSMTPCertificate(certPath, keyPath, "", controlPath, roots); err == nil {
		t.Fatal("startup accepted an empty SMTP hostname")
	}
	if _, err := publicSMTPCertificate(certPath, keyPath, "smtp.example", filepath.Join(dir, "missing"), roots); err == nil {
		t.Fatal("startup accepted an invalid control certificate")
	}
	serverConfig, err := publicSMTPTLS(certPath, keyPath, "smtp.example", controlPath, roots)
	if err != nil {
		t.Fatal(err)
	}
	if !serverConfig.SessionTicketsDisabled {
		t.Fatal("SMTP TLS resumption can bypass certificate revalidation")
	}
	handshake := func() (int64, error) {
		t.Helper()
		serverConn, clientConn := net.Pipe()
		deadline := time.Now().Add(3 * time.Second)
		_ = serverConn.SetDeadline(deadline)
		_ = clientConn.SetDeadline(deadline)
		server := tls.Server(serverConn, serverConfig)
		client := tls.Client(clientConn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "smtp.example", RootCAs: roots})
		defer serverConn.Close()
		defer clientConn.Close()
		serverDone := make(chan error, 1)
		go func() { serverDone <- server.Handshake() }()
		clientErr := client.Handshake()
		serverErr := <-serverDone
		if clientErr != nil {
			return 0, clientErr
		}
		if serverErr != nil {
			return 0, serverErr
		}
		return client.ConnectionState().PeerCertificates[0].SerialNumber.Int64(), nil
	}
	if serial, err := handshake(); err != nil || serial != 3 {
		t.Fatalf("initial certificate: serial=%d error=%v", serial, err)
	}
	rotate("two", nextCert, nextKey)
	if serial, err := handshake(); err != nil || serial != 4 {
		t.Fatalf("rotated certificate: serial=%d error=%v", serial, err)
	}
	for _, tc := range []struct {
		name string
		cert []byte
		key  []byte
	}{{"wrong-host", wrongCert, wrongKey}, {"reused-control-key", reusedCert, reusedKey}, {"expired", expiredCert, expiredKey}, {"mismatched", firstCert, nextKey}, {"invalid", []byte("not a certificate"), nextKey}} {
		t.Run(tc.name, func(t *testing.T) {
			rotate(tc.name, tc.cert, tc.key)
			if _, err := handshake(); err == nil {
				t.Fatal("invalid rotated certificate accepted")
			}
		})
	}
	rotate("recovered", nextCert, nextKey)
	if serial, err := handshake(); err != nil || serial != 4 {
		t.Fatalf("recovered certificate: serial=%d error=%v", serial, err)
	}
}
