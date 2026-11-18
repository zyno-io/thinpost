package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/controltls"
)

func TestPrivateControlCertificateRotation(t *testing.T) {
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
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
	identity, err := url.Parse("spiffe://thinpost.test/staging/worker")
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, key *ecdsa.PrivateKey, expiry time.Time, usage x509.ExtKeyUsage) ([]byte, []byte) {
		t.Helper()
		if key == nil {
			key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{"edge.example"}, URIs: []*url.URL{identity}, NotBefore: now.Add(-time.Hour), NotAfter: expiry, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		private, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
	}
	smtpKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	smtpCert, smtpPrivate := issue(2, smtpKey, now.Add(time.Hour), x509.ExtKeyUsageServerAuth)
	firstCert, firstKey := issue(3, nil, now.Add(time.Hour), x509.ExtKeyUsageServerAuth)
	nextCert, nextKey := issue(4, nil, now.Add(time.Hour), x509.ExtKeyUsageServerAuth)
	reusedCert, reusedKey := issue(5, smtpKey, now.Add(time.Hour), x509.ExtKeyUsageServerAuth)
	expiredCert, expiredKey := issue(6, nil, now.Add(-time.Minute), x509.ExtKeyUsageServerAuth)
	wrongUsageCert, wrongUsageKey := issue(7, nil, now.Add(time.Hour), x509.ExtKeyUsageClientAuth)
	clientCertPEM, clientKeyPEM := issue(8, nil, now.Add(time.Hour), x509.ExtKeyUsageClientAuth)
	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "control.crt"), filepath.Join(dir, "control.key")
	smtpPath, smtpKeyPath := filepath.Join(dir, "smtp.crt"), filepath.Join(dir, "smtp.key")
	write := func(path string, value []byte) {
		t.Helper()
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(smtpPath, smtpCert)
	write(smtpKeyPath, smtpPrivate)
	for _, name := range []string{"control.crt", "control.key"} {
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
		write(filepath.Join(versionDir, "control.crt"), cert)
		write(filepath.Join(versionDir, "control.key"), key)
		next := filepath.Join(dir, "..next")
		if err := os.Symlink(version, next); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	rotate("initial", firstCert, firstKey)
	if _, err := privateControlCertificate(certPath, keyPath, []string{smtpPath}, x509.NewCertPool()); err == nil {
		t.Fatal("accepted untrusted control certificate")
	}
	serverConfig, err := privateControlTLS(certPath, keyPath, []string{smtpPath}, roots, []string{identity.String()})
	if err != nil {
		t.Fatal(err)
	}
	if !serverConfig.SessionTicketsDisabled {
		t.Fatal("private TLS resumption can bypass certificate revalidation")
	}
	clientConfig, err := controltls.Client(roots, clientCert, "edge.example", identity.String())
	if err != nil {
		t.Fatal(err)
	}
	public, err := publicSMTPCertificate(smtpPath, smtpKeyPath, "edge.example", certPath, roots)
	if err != nil {
		t.Fatal(err)
	}
	handshake := func() (int64, error) {
		serverConn, clientConn := net.Pipe()
		defer serverConn.Close()
		defer clientConn.Close()
		deadline := time.Now().Add(3 * time.Second)
		_ = serverConn.SetDeadline(deadline)
		_ = clientConn.SetDeadline(deadline)
		server := tls.Server(serverConn, serverConfig)
		client := tls.Client(clientConn, clientConfig)
		done := make(chan error, 1)
		go func() { done <- server.Handshake() }()
		clientErr := client.Handshake()
		serverErr := <-done
		if clientErr != nil {
			return 0, clientErr
		}
		if serverErr != nil {
			return 0, serverErr
		}
		return client.ConnectionState().PeerCertificates[0].SerialNumber.Int64(), nil
	}
	if serial, err := handshake(); err != nil || serial != 3 {
		t.Fatalf("initial certificate: serial=%d err=%v", serial, err)
	}
	rotate("next", nextCert, nextKey)
	if serial, err := handshake(); err != nil || serial != 4 {
		t.Fatalf("rotated certificate: serial=%d err=%v", serial, err)
	}
	for _, tc := range []struct {
		name      string
		cert, key []byte
	}{
		{"mismatched", firstCert, nextKey}, {"expired", expiredCert, expiredKey}, {"wrong-usage", wrongUsageCert, wrongUsageKey}, {"reused-smtp-key", reusedCert, reusedKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rotate(tc.name, tc.cert, tc.key)
			if _, err := handshake(); err == nil {
				t.Fatal("accepted invalid rotation")
			}
		})
	}
	if _, err := public(nil); err == nil {
		t.Fatal("SMTP accepted a newly reused control key")
	}
	rotate("recovered", nextCert, nextKey)
	if serial, err := handshake(); err != nil || serial != 4 {
		t.Fatalf("recovered certificate: serial=%d err=%v", serial, err)
	}
	if _, err := public(nil); err != nil {
		t.Fatalf("SMTP failed after recovery: %v", err)
	}
}
