package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
)

// certificatePublicKey reads only public certificate material. In particular,
// it does not load the other listener's private key.
func certificatePublicKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read certificate")
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("invalid certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid certificate")
	}
	return cert.RawSubjectPublicKeyInfo, nil
}

func publicSMTPTLS(certPath, keyPath, hostname, controlCertPath string, roots *x509.CertPool) (*tls.Config, error) {
	return publicSMTPTLSRoots(certPath, keyPath, hostname, controlCertPath, func() (*x509.CertPool, error) { return roots, nil })
}

// publicSMTPTLSRoots is publicSMTPTLS with trust roots read afresh for every
// handshake, so a rotated CA bundle takes effect without a restart.
func publicSMTPTLSRoots(certPath, keyPath, hostname, controlCertPath string, roots func() (*x509.CertPool, error)) (*tls.Config, error) {
	get, err := publicSMTPCertificateRoots(certPath, keyPath, hostname, controlCertPath, roots)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: get, SessionTicketsDisabled: true}, nil
}

// publicSMTPCertificate validates at startup and on every new TLS handshake.
// Kubernetes projected Secrets can change without restarting the worker. A
// mismatched, expired, untrusted, or partially rotated pair fails the new
// handshake rather than serving stale or unvalidated key material. Already
// established sessions retain the certificate with which they connected.
func publicSMTPCertificate(certPath, keyPath, hostname, controlCertPath string, roots *x509.CertPool) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	return publicSMTPCertificateRoots(certPath, keyPath, hostname, controlCertPath, func() (*x509.CertPool, error) { return roots, nil })
}

func publicSMTPCertificateRoots(certPath, keyPath, hostname, controlCertPath string, rootsFn func() (*x509.CertPool, error)) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	if hostname == "" {
		return nil, errors.New("invalid SMTP hostname")
	}
	load := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		controlKey, err := certificatePublicKey(controlCertPath)
		if err != nil {
			return nil, errors.New("cannot read control certificate")
		}
		certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil || len(certificate.Certificate) == 0 {
			return nil, errors.New("cannot load public SMTP certificate")
		}
		leaf, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			return nil, errors.New("invalid public SMTP certificate")
		}
		if bytes.Equal(controlKey, leaf.RawSubjectPublicKeyInfo) {
			return nil, errors.New("control and SMTP certificate keys must be distinct")
		}
		intermediates := x509.NewCertPool()
		for _, der := range certificate.Certificate[1:] {
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				return nil, errors.New("invalid SMTP certificate chain")
			}
			intermediates.AddCert(cert)
		}
		roots, err := rootsFn()
		if err != nil {
			return nil, errors.New("cannot load SMTP trust roots")
		}
		if _, err = leaf.Verify(x509.VerifyOptions{DNSName: hostname, Intermediates: intermediates, Roots: roots}); err != nil {
			return nil, errors.New("public SMTP certificate is not trusted for hostname")
		}
		return &certificate, nil
	}
	if _, err := load(nil); err != nil {
		return nil, err
	}
	return load, nil
}
