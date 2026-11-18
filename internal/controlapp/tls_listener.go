package controlapp

import (
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// CertReloader serves a certificate and key read from files and re-reads them
// when either changes, so a renewal (for example a cert-manager Secret volume
// update) applies to new handshakes without a restart. The files are checked
// with a stat per handshake; a replaced Secret volume swaps a symlink, which
// os.Stat follows, so the target's identity and mtime change.
type CertReloader struct {
	certFile, keyFile string
	mu                sync.Mutex
	cert              *tls.Certificate
	certInfo, keyInfo os.FileInfo
	lastFailure       time.Time
}

// NewCertReloader fails closed: the files must exist and form a valid pair now.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile}
	if _, err := r.reload(); err != nil {
		return nil, errors.New("control TLS certificate or key is missing or invalid")
	}
	return r, nil
}

func (r *CertReloader) reload() (*tls.Certificate, error) {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return nil, err
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return nil, err
	}
	r.cert, r.certInfo, r.keyInfo = &cert, certInfo, keyInfo
	return r.cert, nil
}

func unchanged(old, current os.FileInfo) bool {
	return old != nil && os.SameFile(old, current) && old.ModTime().Equal(current.ModTime()) && old.Size() == current.Size()
}

// GetCertificate keeps serving the last good certificate if a replacement is
// unreadable or the pair is mid-update; it retries on the next handshake (at
// most once per second, to bound log volume).
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	certInfo, certErr := os.Stat(r.certFile)
	keyInfo, keyErr := os.Stat(r.keyFile)
	if certErr == nil && keyErr == nil && unchanged(r.certInfo, certInfo) && unchanged(r.keyInfo, keyInfo) {
		return r.cert, nil
	}
	if time.Since(r.lastFailure) < time.Second {
		return r.cert, nil
	}
	cert, err := r.reload()
	if err != nil {
		r.lastFailure = time.Now()
		log.Print("control TLS certificate reload failed; serving the previous certificate")
		return r.cert, nil
	}
	log.Print("control TLS certificate reloaded")
	return cert, nil
}

// NewTLSServer returns an HTTPS server for the same handler as the plain
// listener. Serve it with ListenAndServeTLS("", "") (certificates come from
// the reloader).
func NewTLSServer(addr string, handler http.Handler, certs *CertReloader) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.GetCertificate},
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
}
