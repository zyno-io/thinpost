package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"

	"github.com/zyno-io/thinpost/internal/controltls"
)

func privateControlTLS(certPath, keyPath string, smtpCertPaths []string, roots *x509.CertPool, controllers []string) (*tls.Config, error) {
	get, err := privateControlCertificate(certPath, keyPath, smtpCertPaths, roots)
	if err != nil {
		return nil, err
	}
	initial, err := get(nil)
	if err != nil {
		return nil, err
	}
	config, err := controltls.Server(roots, *initial, controllers)
	if err != nil {
		return nil, errors.New("invalid control TLS configuration")
	}
	config.Certificates = nil
	config.GetCertificate = get
	// Resumption can skip certificate selection and thus bypass a bad update.
	config.SessionTicketsDisabled = true
	return config, nil
}

// privateControlCertificate validates the current projected certificate on
// startup and for each new handshake. Trust roots and authorization policy are
// deliberately fixed for the life of the process; rotating them needs a
// coordinated restart. Existing connections keep their negotiated certificate.
func privateControlCertificate(certPath, keyPath string, smtpCertPaths []string, roots *x509.CertPool) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	if roots == nil {
		return nil, errors.New("missing private control CA")
	}
	load := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil || len(cert.Certificate) == 0 {
			return nil, errors.New("cannot load control certificate")
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, errors.New("invalid control certificate")
		}
		// The control key must differ from every listener's SMTP key.
		for _, smtpCertPath := range smtpCertPaths {
			publicKey, err := certificatePublicKey(smtpCertPath)
			if err != nil {
				return nil, errors.New("cannot read public SMTP certificate")
			}
			if bytes.Equal(publicKey, leaf.RawSubjectPublicKeyInfo) {
				return nil, errors.New("control and SMTP certificate keys must be distinct")
			}
		}
		intermediates := x509.NewCertPool()
		for _, der := range cert.Certificate[1:] {
			intermediate, err := x509.ParseCertificate(der)
			if err != nil {
				return nil, errors.New("invalid control certificate chain")
			}
			intermediates.AddCert(intermediate)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return nil, errors.New("control certificate is not trusted for server authentication")
		}
		return &cert, nil
	}
	if _, err := load(nil); err != nil {
		return nil, err
	}
	return load, nil
}
