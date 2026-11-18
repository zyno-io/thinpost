package webhook

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

// request is a minimal body-and-header request for transport tests.
func request(body string) format.Request {
	return format.Request{Body: []byte(body), Header: http.Header{"Content-Type": {"application/json"}}}
}

func nativeRequest(t *testing.T, id string, now time.Time) format.Request {
	t.Helper()
	native, _ := format.Lookup(format.Thinpost)
	req, err := native.Build(format.Input{Event: format.Event{ID: id, MessageID: "m", Outcome: "accepted", SMTPCode: 250, OccurredAt: now}, Secret: testSecret, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return req
}

type fixedResolver struct {
	addresses []netip.Addr
	err       error
	name      string
}

func (r *fixedResolver) LookupNetIP(_ context.Context, _, name string) ([]netip.Addr, error) {
	r.name = name
	return r.addresses, r.err
}

// setupServer starts a TLS server and a sender that may dial loopback on the
// server's port. The endpoint names the certificate's DNS name on port 443.
func setupServer(t *testing.T, handler http.Handler) (*Sender, *fixedResolver, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cert := server.Certificate()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	resolver := &fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	sender := &Sender{Roots: roots, Resolver: resolver, allow: func(netip.Addr) bool { return true }, port: port}
	return sender, resolver, "https://" + cert.DNSNames[0] + "/hook"
}

func TestSignedWebhookFreshnessAndQuery(t *testing.T) {
	now := time.Now()
	var seen atomic.Bool
	sender, resolver, endpoint := setupServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		id, stamp, sig := r.Header.Get("X-Thinpost-Event-ID"), r.Header.Get("X-Thinpost-Timestamp"), r.Header.Get("X-Thinpost-Signature")
		if !format.VerifyThinpost(testSecret, id, stamp, body, sig, now, 5*time.Minute) {
			t.Error("invalid signature")
		}
		if format.VerifyThinpost(testSecret, id, stamp, []byte("tampered"), sig, now, 5*time.Minute) {
			t.Error("accepted tampered payload")
		}
		if format.VerifyThinpost(testSecret, id, stamp, body, sig, now.Add(10*time.Minute), 5*time.Minute) {
			t.Error("accepted stale signature")
		}
		if r.URL.Path != "/hook" || r.URL.RawQuery != "token=a%20b&x=1" {
			t.Errorf("request target %q", r.URL.RequestURI())
		}
		seen.Store(true)
		w.WriteHeader(204)
	}))
	result, err := sender.Send(context.Background(), endpoint+"?token=a%20b&x=1", nativeRequest(t, "event-1", now), nil)
	if err != nil || !result.Success || result.StatusCode != 204 || result.Failure != "" || !seen.Load() {
		t.Fatalf("result %+v %v", result, err)
	}
	if resolver.name == "" || resolver.name[len(resolver.name)-1] != '.' {
		t.Fatalf("lookup was not rooted: %q", resolver.name)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var calls atomic.Int32
	sender, _, endpoint := setupServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "https://example.com/other", http.StatusFound)
	}))
	result, err := sender.Send(context.Background(), endpoint, request(`{}`), nil)
	if err != nil || result.Success || result.StatusCode != 302 || calls.Load() != 1 {
		t.Fatalf("redirect: %+v %v", result, err)
	}
}

func TestInvalidEndpointsAreErrors(t *testing.T) {
	sender, _, endpoint := setupServer(t, http.NotFoundHandler())
	for _, bad := range []string{"https://example.com:8443/hook", "http://example.com/hook", "https://127.0.0.1/hook", "https://localhost/hook", "https://user@example.com/hook", "https://example.com/hook#x", "https://example.com/a b"} {
		if _, err := sender.Send(context.Background(), bad, request(`{}`), nil); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := sender.Send(context.Background(), endpoint, format.Request{}, nil); err == nil {
		t.Error("accepted an empty body")
	}
	if _, err := sender.Send(context.Background(), endpoint, format.Request{Body: make([]byte, 1<<20+1)}, nil); err == nil {
		t.Error("accepted an oversized body")
	}
}

func TestFailureClassification(t *testing.T) {
	var calls atomic.Int32
	sender, resolver, endpoint := setupServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(300 * time.Millisecond)
	}))
	send := func(s *Sender, ctx context.Context) Result {
		t.Helper()
		result, err := s.Send(ctx, endpoint, request(`{}`), nil)
		if err != nil || result.StatusCode != 0 || result.Success {
			t.Fatalf("unexpected result %+v %v", result, err)
		}
		return result
	}
	// Default policy refuses loopback without dialing.
	blocked := &Sender{Roots: sender.Roots, Resolver: resolver, port: sender.port}
	if got := send(blocked, context.Background()).Failure; got != FailureBlocked || calls.Load() != 0 {
		t.Errorf("loopback: %q, %d calls", got, calls.Load())
	}
	resolver.err = errors.New("no such host")
	if got := send(sender, context.Background()).Failure; got != FailureResolution {
		t.Errorf("resolution: %q", got)
	}
	resolver.err = nil
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, closedPort, _ := net.SplitHostPort(closed.Addr().String())
	closed.Close()
	unreachable := &Sender{Roots: sender.Roots, Resolver: resolver, allow: sender.allow, port: closedPort}
	if got := send(unreachable, context.Background()).Failure; got != FailureConnection {
		t.Errorf("connection: %q", got)
	}
	untrusted := &Sender{Roots: x509.NewCertPool(), Resolver: resolver, allow: sender.allow, port: sender.port}
	if got := send(untrusted, context.Background()).Failure; got != FailureTLS {
		t.Errorf("tls: %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if got := send(sender, ctx).Failure; got != FailureTimeout {
		t.Errorf("timeout: %q", got)
	}
}

func TestOutOfRangeStatusIsAConnectionFailure(t *testing.T) {
	sender, _, endpoint := setupServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(999)
	}))
	result, err := sender.Send(context.Background(), endpoint, request(`{}`), nil)
	if err != nil || result.Success || result.StatusCode != 0 || result.Failure != "connection_failed" {
		t.Fatalf("status 999: %+v %v", result, err)
	}
}

func TestAuthIsAppliedToRequest(t *testing.T) {
	var seen atomic.Bool
	sender, _, endpoint := setupServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "hook-user" || pass != "p:ass word" {
			t.Errorf("basic auth %q %q %v", user, pass, ok)
		}
		if r.Header.Get("X-Token") != "abc" || r.Header.Get("X-Other") != "def" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("headers missing", r.Header)
		}
		seen.Store(true)
		w.WriteHeader(204)
	}))
	a := admin.NewWebhookAuth("hook-user", "p:ass word", []admin.WebhookHeader{{Name: "X-Token", Value: "abc"}, {Name: "x-other", Value: "def"}})
	auth := &a
	result, err := sender.Send(context.Background(), endpoint, request(`{}`), auth)
	if err != nil || !result.Success || !seen.Load() {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestAuthCannotOverrideFormatHeadersOrBeInvalid(t *testing.T) {
	var calls atomic.Int32
	sender, _, endpoint := setupServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	withHeader := request(`{}`)
	withHeader.Header["X-Custom"] = []string{"from-format"}
	withAuthorization := request(`{}`)
	withAuthorization.Header.Set("Authorization", "from-format")
	for name, tc := range map[string]struct {
		req  format.Request
		auth admin.WebhookAuth
	}{
		"format header":        {withHeader, admin.NewWebhookAuth("", "", []admin.WebhookHeader{{Name: "x-custom", Value: "x"}})},
		"format authorization": {withAuthorization, admin.NewWebhookAuth("u", "p", nil)},
		"host":                 {request(`{}`), admin.NewWebhookAuth("", "", []admin.WebhookHeader{{Name: "Host", Value: "evil.example"}})},
		"crlf value":           {request(`{}`), admin.NewWebhookAuth("", "", []admin.WebhookHeader{{Name: "X-A", Value: "a\r\nB: c"}})},
		"signature header":     {request(`{}`), admin.NewWebhookAuth("", "", []admin.WebhookHeader{{Name: "X-Thinpost-Signature", Value: "x"}})},
		"username only":        {request(`{}`), admin.NewWebhookAuth("u", "", nil)},
	} {
		auth := tc.auth
		_, err := sender.Send(context.Background(), endpoint, tc.req, &auth)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "evil") || strings.Contains(err.Error(), "from-format") {
			t.Errorf("%s: error echoes a value", name)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("a rejected request was sent")
	}
}
