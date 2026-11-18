package controltls

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"google.golang.org/grpc/credentials"
)

// VerifiedCertificateExpiry finds the latest expiry of a currently valid
// verified chain for the authenticated peer. Each chain expires at its first
// expiring certificate; multiple verified chains provide alternative paths.
func VerifiedCertificateExpiry(state tls.ConnectionState, now time.Time, skew time.Duration) (time.Time, error) {
	if skew < 0 || len(state.PeerCertificates) == 0 || state.PeerCertificates[0] == nil {
		return time.Time{}, errors.New("missing peer certificate or invalid clock margin")
	}
	var latest time.Time
	for _, chain := range state.VerifiedChains {
		if len(chain) == 0 || chain[0] == nil || !bytes.Equal(chain[0].Raw, state.PeerCertificates[0].Raw) {
			continue
		}
		var end time.Time
		valid := true
		for _, cert := range chain {
			if cert == nil || cert.NotBefore.After(now.Add(-skew)) || !now.Add(skew).Before(cert.NotAfter) {
				valid = false
				break
			}
			if end.IsZero() || cert.NotAfter.Before(end) {
				end = cert.NotAfter
			}
		}
		if valid && end.After(latest) {
			latest = end
		}
	}
	if latest.IsZero() {
		return time.Time{}, errors.New("peer certificate chain is not currently valid")
	}
	return latest, nil
}

// ExpiringClientCredentials closes an authenticated connection before its
// verified server certificate chain expires. It does not reload credentials or
// implement revocation. The independent timer cannot be extended by a caller
// changing a network deadline.
func ExpiringClientCredentials(config *tls.Config, margin time.Duration) (credentials.TransportCredentials, error) {
	if config == nil || margin <= 0 {
		return nil, errors.New("invalid TLS configuration or clock margin")
	}
	return &expiringCredentials{TransportCredentials: credentials.NewTLS(config), margin: margin}, nil
}

type expiringCredentials struct {
	credentials.TransportCredentials
	margin time.Duration
}

func (c *expiringCredentials) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, auth, err := c.TransportCredentials.ClientHandshake(ctx, authority, raw)
	if err != nil {
		return nil, nil, err
	}
	info, ok := auth.(credentials.TLSInfo)
	if !ok {
		_ = conn.Close()
		return nil, nil, errors.New("missing authenticated TLS peer")
	}
	now := time.Now()
	expires, err := VerifiedCertificateExpiry(info.State, now, c.margin)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	deadline := now.Add(expires.Sub(now) - c.margin)
	timer := time.AfterFunc(time.Until(deadline), func() { _ = raw.Close() })
	return &expiringConn{Conn: conn, timer: timer}, auth, nil
}

func (c *expiringCredentials) Clone() credentials.TransportCredentials {
	return &expiringCredentials{TransportCredentials: c.TransportCredentials.Clone(), margin: c.margin}
}

type expiringConn struct {
	net.Conn
	timer *time.Timer
}

func (c *expiringConn) Close() error {
	c.timer.Stop()
	return c.Conn.Close()
}
