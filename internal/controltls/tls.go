// Package controltls configures mutual TLS for trusted-initiated edge control
// connections. It creates no listeners, network interfaces or trust roots.
package controltls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"
	"strings"
)

// Server requires a private-CA-verified client certificate and an exact
// configured controller URI identity. Allowed identities are copied so callers
// cannot mutate the live policy. The control protocol bounds grants by peer
// certificate validity; live revocation and credential rotation remain separate.
func Server(roots *x509.CertPool, certificate tls.Certificate, controllers []string) (*tls.Config, error) {
	if roots == nil || len(certificate.Certificate) == 0 || len(controllers) == 0 {
		return nil, errors.New("control TLS requires CA, certificate and controller identities")
	}
	allowed := make(map[string]struct{}, len(controllers))
	for _, identity := range controllers {
		if !validIdentity(identity) {
			return nil, errors.New("invalid controller identity")
		}
		allowed[identity] = struct{}{}
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, VerifyConnection: func(state tls.ConnectionState) error { return verifyIdentity(state, allowed) }}, nil
}

// Client verifies both the worker's DNS name and its exact URI identity in
// addition to the private CA chain. It never disables certificate validation.
func Client(roots *x509.CertPool, certificate tls.Certificate, workerDNS, workerIdentity string) (*tls.Config, error) {
	if roots == nil || len(certificate.Certificate) == 0 || workerDNS == "" || strings.ContainsAny(workerDNS, "\r\n ") || !validIdentity(workerIdentity) {
		return nil, errors.New("control TLS requires CA, certificate, worker DNS name and identity")
	}
	allowed := map[string]struct{}{workerIdentity: {}}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, RootCAs: roots, ServerName: workerDNS, VerifyConnection: func(state tls.ConnectionState) error { return verifyIdentity(state, allowed) }}, nil
}

// Identity extracts the one authorized URI identity from an already verified
// connection. The caller still maps it to the database-issued controller lease.
func Identity(state tls.ConnectionState, allowed []string) (string, error) {
	set := make(map[string]struct{}, len(allowed))
	for _, item := range allowed {
		if !validIdentity(item) {
			return "", errors.New("invalid identity")
		}
		set[item] = struct{}{}
	}
	if err := verifyIdentity(state, set); err != nil {
		return "", err
	}
	for _, identity := range state.PeerCertificates[0].URIs {
		if _, ok := set[identity.String()]; ok {
			return identity.String(), nil
		}
	}
	return "", errors.New("unauthorized peer identity")
}
func verifyIdentity(state tls.ConnectionState, allowed map[string]struct{}) error {
	if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		return errors.New("peer certificate not verified")
	}
	matches := 0
	for _, identity := range state.PeerCertificates[0].URIs {
		if _, ok := allowed[identity.String()]; ok {
			matches++
		}
	}
	if matches != 1 {
		return errors.New("peer must have exactly one authorized identity")
	}
	return nil
}
func validIdentity(identity string) bool {
	u, err := url.Parse(identity)
	return err == nil && u.Scheme == "spiffe" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path != "" && !strings.ContainsAny(identity, "\r\n ")
}
