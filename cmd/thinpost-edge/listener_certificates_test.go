package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Every listener has its own certificate: each must cover its own name, and the
// control key must differ from all of them.
func TestPerListenerCertificateChecks(t *testing.T) {
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
	dir := t.TempDir()
	issue := func(name string, serial int64, key *ecdsa.PrivateKey, dnsName string) (string, string) {
		t.Helper()
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{dnsName}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		private, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPath, keyPath := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
		if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
			t.Fatal(err)
		}
		return certPath, keyPath
	}
	newKey := func() *ecdsa.PrivateKey {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	controlKey := newKey()
	controlCert, controlKeyPath := issue("control", 2, controlKey, "edge.example")
	firstCert, firstKey := issue("first", 3, newKey(), "smtp.first.example")
	secondCert, secondKey := issue("second", 4, newKey(), "smtp.second.example")
	reusedCert, _ := issue("reused", 5, controlKey, "smtp.third.example")

	if _, err = publicSMTPCertificate(firstCert, firstKey, "smtp.first.example", controlCert, roots); err != nil {
		t.Fatalf("first listener: %v", err)
	}
	if _, err = publicSMTPCertificate(secondCert, secondKey, "smtp.second.example", controlCert, roots); err != nil {
		t.Fatalf("second listener: %v", err)
	}
	// A certificate only counts for the name its own listener greets with.
	if _, err = publicSMTPCertificate(firstCert, firstKey, "smtp.second.example", controlCert, roots); err == nil {
		t.Fatal("a listener accepted a certificate that does not cover its name")
	}
	if _, err = publicSMTPCertificate(secondCert, secondKey, "smtp.first.example", controlCert, roots); err == nil {
		t.Fatal("the primary listener accepted the other listener's certificate")
	}
	if _, err = privateControlCertificate(controlCert, controlKeyPath, []string{firstCert, secondCert}, roots); err != nil {
		t.Fatalf("control certificate with two listener certificates: %v", err)
	}
	if _, err = privateControlCertificate(controlCert, controlKeyPath, []string{firstCert, reusedCert}, roots); err == nil {
		t.Fatal("control key shared with a second listener's certificate was accepted")
	}
	if _, err = privateControlCertificate(controlCert, controlKeyPath, []string{firstCert, filepath.Join(dir, "missing.crt")}, roots); err == nil {
		t.Fatal("a missing listener certificate was accepted")
	}
}
