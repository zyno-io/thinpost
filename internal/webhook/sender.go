// Package webhook sends webhook events built by package format.
package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

const maxPayload = 1 << 20
const maxResponse = 64 << 10

// Limits of the inbound lane: a payload carries the raw message (base64) and
// its decoded bodies, and the receiver may take a while to store it.
const (
	// MaxInboundPayload bounds one inbound request body: a 16 MiB message in
	// base64 plus two JSON-escaped 1 MiB bodies and headers.
	MaxInboundPayload = 32 << 20
	// InboundTimeout is the whole-request deadline of an inbound delivery.
	InboundTimeout = 120 * time.Second
)

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// Sender delivers events to console-managed endpoints. The zero value
// is ready to use and safe for concurrent use. Destinations are limited to
// public addresses on port 443 by PublicAddress, except an exact URL on the
// operator's Destinations list, which may use http or any port but must resolve
// inside a configured Service CIDR.
type Sender struct {
	Roots    *x509.CertPool
	Resolver Resolver
	// Destinations optionally admits the operator's exact-match internal
	// endpoints (see admin.WebhookDestinations). nil keeps the public-only rules.
	Destinations *admin.WebhookDestinations

	// MaxPayload and Timeout override the 1 MiB body limit and 15 second
	// whole-request deadline; the inbound lane sets both. Zero keeps the defaults.
	MaxPayload int
	Timeout    time.Duration

	// Test seams; production leaves them zero.
	allow func(netip.Addr) bool
	port  string
	// connect replaces the final TCP dial of an already vetted address.
	connect func(ctx context.Context, network, address string) (net.Conn, error)
}

// Result.Failure values, set only when no HTTP response arrived.
const (
	FailureBlocked    = "blocked_destination"
	FailureResolution = "resolution_failed"
	FailureConnection = "connection_failed"
	FailureTLS        = "tls_failed"
	FailureTimeout    = "timeout"
)

type Result struct {
	StatusCode int
	Success    bool
	Failure    string
}

var (
	errBlocked    = errors.New("webhook destination is not public")
	errResolution = errors.New("webhook resolution failed")
	errConnection = errors.New("webhook connection failed")
)

// Send never follows redirects or uses environment-configured HTTP proxies. It
// pins each connection to a resolved public numeric IP while retaining the
// hostname for TLS validation. Credentials, endpoint, payload and response body
// must not be logged. The caller owns durable attempts, retry and retention.
// An error means invalid input; a failed transport is a Result with Failure.
// The request's headers and signature come from the format; auth adds HTTP
// Basic credentials and custom headers, which may not replace a format header.
func (s *Sender) Send(ctx context.Context, endpoint string, payload format.Request, auth *admin.WebhookAuth) (Result, error) {
	limit, timeout := maxPayload, 15*time.Second
	if s.MaxPayload > 0 {
		limit = s.MaxPayload
	}
	if s.Timeout > 0 {
		timeout = s.Timeout
	}
	if len(payload.Body) == 0 || len(payload.Body) > limit {
		return Result{}, errors.New("invalid webhook event")
	}
	normalized, internal, ok := s.Destinations.NormalizeEndpoint(endpoint)
	if !ok {
		return Result{}, errors.New("invalid webhook endpoint")
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return Result{}, errors.New("invalid webhook endpoint")
	}
	host := u.Hostname()
	wantPort := "443"
	if internal {
		if wantPort = u.Port(); wantPort == "" {
			wantPort = "80"
			if u.Scheme == "https" {
				wantPort = "443"
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, normalized, bytes.NewReader(payload.Body))
	if err != nil {
		return Result{}, errors.New("invalid webhook request")
	}
	for name, values := range payload.Header {
		req.Header[name] = slices.Clone(values)
	}
	if auth != nil && internal && !auth.IsZero() {
		// No credentials over the plain in-cluster exception.
		return Result{}, errors.New("invalid webhook authentication")
	}
	if auth != nil {
		if !admin.ValidWebhookAuth(*auth) {
			return Result{}, errors.New("invalid webhook authentication")
		}
		if auth.Username() != "" {
			if hasHeader(req.Header, "Authorization") {
				return Result{}, errors.New("invalid webhook authentication")
			}
			req.SetBasicAuth(auth.Username(), auth.Password())
		}
		for _, h := range auth.Headers() {
			if hasHeader(req.Header, h.Name) {
				return Result{}, errors.New("invalid webhook authentication")
			}
			req.Header.Set(h.Name, h.Value)
		}
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, RootCAs: s.Roots}, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: max(10*time.Second, timeout-5*time.Second), MaxResponseHeaderBytes: 16 << 10}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return s.dial(ctx, network, address, host, wantPort, internal)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return Result{Failure: classify(err)}, nil
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponse))
	if response.StatusCode < 100 || response.StatusCode > 599 {
		// Go accepts any three-digit status; anything else is not valid HTTP.
		return Result{Failure: "connection_failed"}, nil
	}
	return Result{StatusCode: response.StatusCode, Success: response.StatusCode >= 200 && response.StatusCode < 300}, nil
}

// IsInternal reports whether an endpoint is on the operator's internal
// destination list (an exact match under the current configuration).
func (s *Sender) IsInternal(endpoint string) bool {
	_, internal, ok := s.Destinations.NormalizeEndpoint(endpoint)
	return ok && internal
}

// internalBlocked is the fail-closed check that dispatchers and the test send
// make before sending: an endpoint on the internal list may carry only native
// events and no credentials, whatever was stored.
func internalBlocked(sender Deliverer, endpoint, formatName string, hasAuth bool) bool {
	classifier, ok := sender.(interface{ IsInternal(string) bool })
	return ok && classifier.IsInternal(endpoint) && !admin.InternalEndpointPermitted(formatName, hasAuth)
}

func hasHeader(h http.Header, name string) bool {
	for k := range h {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

func classify(err error) string {
	var timeout net.Error
	var verify *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	switch {
	case errors.Is(err, errBlocked):
		return FailureBlocked
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout():
		return FailureTimeout
	case errors.Is(err, errResolution):
		return FailureResolution
	case errors.As(err, &verify) || errors.As(err, &record):
		return FailureTLS
	default:
		return FailureConnection
	}
}

func (s *Sender) dial(ctx context.Context, network, address, wantHost, wantPort string, internal bool) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !strings.EqualFold(host, wantHost) || port != wantPort {
		return nil, errBlocked
	}
	resolver := s.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	// Resolve the rooted name so cluster search domains (ndots) cannot turn a
	// short name into an in-cluster service.
	addresses, err := resolver.LookupNetIP(ctx, "ip", host+".")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errResolution, err)
	}
	// An internal destination is vetted against the Service CIDRs only, with
	// no test override; every other destination must be public.
	allow := s.allow
	if internal {
		allow = s.Destinations.AllowAddress
	} else if allow == nil {
		allow = PublicAddress
	}
	if s.port != "" {
		port = s.port
	}
	var dialer net.Dialer
	var lastErr error
	allowed := false
	for i, address := range addresses {
		if i >= 32 {
			break
		}
		address = address.Unmap()
		if !allow(address) {
			continue
		}
		allowed = true
		dial := dialer.DialContext
		if s.connect != nil {
			dial = s.connect
		}
		conn, err := dial(ctx, network, net.JoinHostPort(address.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if !allowed {
		return nil, errBlocked
	}
	return nil, fmt.Errorf("%w: %w", errConnection, lastErr)
}
