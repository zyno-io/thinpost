package controltls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"
)

type authority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	roots       *x509.CertPool
}

func newAuthority(t *testing.T) authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return authority{cert, key, roots}
}
func (a authority) issue(t *testing.T, identity, dns string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	if dns != "" {
		template.DNSNames = []string{dns}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}))
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
func handshake(client, server *tls.Config) (error, error) {
	a, b := net.Pipe()
	_ = a.SetDeadline(time.Now().Add(time.Second))
	_ = b.SetDeadline(time.Now().Add(time.Second))
	defer a.Close()
	defer b.Close()
	serverResult := make(chan error, 1)
	go func() {
		conn := tls.Server(b, server)
		if err := conn.Handshake(); err != nil {
			serverResult <- err
			return
		}
		if _, err := conn.Write([]byte{1}); err != nil {
			serverResult <- err
			return
		}
		buf := make([]byte, 1)
		_, err := conn.Read(buf)
		serverResult <- err
	}()
	conn := tls.Client(a, client)
	err := conn.Handshake()
	if err == nil {
		buf := make([]byte, 1)
		_, err = conn.Read(buf)
		if err == nil {
			_, err = conn.Write([]byte{2})
		}
	}
	_ = a.Close()
	return err, <-serverResult
}
func TestMutualTLSVerifiesNamesAndIdentities(t *testing.T) {
	ca := newAuthority(t)
	foreign := newAuthority(t)
	controller := "spiffe://thinpost.test/production/controller/one"
	worker := "spiffe://thinpost.test/production/worker/one"
	serverCert := ca.issue(t, worker, "edge.internal", x509.ExtKeyUsageServerAuth)
	clientCert := ca.issue(t, controller, "", x509.ExtKeyUsageClientAuth)
	foreignCert := foreign.issue(t, controller, "", x509.ExtKeyUsageClientAuth)
	server, err := Server(ca.roots, serverCert, []string{controller})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, dns, identity string
		cert                tls.Certificate
		ok                  bool
	}{
		{"valid", "edge.internal", worker, clientCert, true},
		{"wrong DNS", "wrong.internal", worker, clientCert, false},
		{"wrong worker identity", "edge.internal", "spiffe://thinpost.test/production/worker/other", clientCert, false},
		{"untrusted controller CA", "edge.internal", worker, foreignCert, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := Client(ca.roots, tc.cert, tc.dns, tc.identity)
			if err != nil {
				t.Fatal(err)
			}
			clientErr, serverErr := handshake(client, server)
			if tc.ok && (clientErr != nil || serverErr != nil) {
				t.Fatalf("client %v server %v", clientErr, serverErr)
			}
			if !tc.ok && clientErr == nil && serverErr == nil {
				t.Fatal("unauthorized connection succeeded")
			}
		})
	}
	unauthorized, err := Server(ca.roots, serverCert, []string{"spiffe://thinpost.test/production/controller/other"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := Client(ca.roots, clientCert, "edge.internal", worker)
	if err != nil {
		t.Fatal(err)
	}
	if c, s := handshake(client, unauthorized); c == nil && s == nil {
		t.Fatal("unauthorized client identity succeeded")
	}
}
func TestRejectsIncompleteConfiguration(t *testing.T) {
	if _, err := Server(nil, tls.Certificate{}, nil); err == nil {
		t.Fatal("accepted missing server trust")
	}
	if _, err := Client(nil, tls.Certificate{}, "", ""); err == nil {
		t.Fatal("accepted missing client trust")
	}
	if validIdentity("https://example.com/test") || validIdentity("spiffe://example.com") || !validIdentity("spiffe://thinpost.test/staging/worker/one") {
		t.Fatal(fmt.Errorf("identity validation"))
	}
}
