package customdomain

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDNS answers the three lookups a tracking check makes.
type fakeDNS struct {
	cname    string
	cnameErr error
	hosts    map[string][]string
	hostErr  error
	netIPs   []netip.Addr
	netErr   error
}

func (d *fakeDNS) LookupCNAME(context.Context, string) (string, error) { return d.cname, d.cnameErr }
func (d *fakeDNS) LookupHost(_ context.Context, host string) ([]string, error) {
	if d.hostErr != nil {
		return nil, d.hostErr
	}
	return d.hosts[strings.TrimSuffix(host, ".")], nil
}
func (d *fakeDNS) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return d.netIPs, d.netErr
}

const proofPrefix = "proof for "

func verifyProof(challenge, body string) bool { return body == proofPrefix+challenge }

// tlsFixture is an HTTPS server standing in for the tracking handler behind a
// gateway, reached through the checker's test seams.
type tlsFixture struct {
	server  *httptest.Server
	checker *TrackingChecker
	hits    atomic.Int32
	host    atomic.Value
	path    atomic.Value
}

func newTLSFixture(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, challenge string)) *tlsFixture {
	t.Helper()
	f := &tlsFixture{}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.host.Store(r.Host)
		f.path.Store(r.URL.Path)
		handler(w, r, strings.TrimPrefix(r.URL.Path, "/t/h/v1/"))
	}))
	t.Cleanup(f.server.Close)
	pool := x509.NewCertPool()
	pool.AddCert(f.server.Certificate())
	_, port, _ := net.SplitHostPort(f.server.Listener.Addr().String())
	f.checker = &TrackingChecker{
		Resolver: &fakeDNS{netIPs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}, cname: "links.example.net.", hosts: map[string][]string{}},
		Target:   "links.example.net",
		Verify:   verifyProof,
		Allow:    func(netip.Addr) bool { return true },
		roots:    pool,
		port:     port,
	}
	return f
}

func okHandler(w http.ResponseWriter, _ *http.Request, challenge string) {
	_, _ = w.Write([]byte(proofPrefix + challenge))
}

// The test server's certificate is for example.com and 127.0.0.1.
const certHost = "example.com"

func TestHTTPSCheckOK(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	f := newTLSFixture(t, okHandler)
	got, expires := f.checker.checkHTTPS(context.Background(), certHost)
	if got != HTTPSOK {
		t.Fatal(got)
	}
	if !expires.Equal(f.server.Certificate().NotAfter) {
		t.Fatal("certificate expiry not recorded", expires)
	}
	if f.host.Load() != certHost {
		t.Fatal("Host sent:", f.host.Load())
	}
	if p, _ := f.path.Load().(string); !strings.HasPrefix(p, "/t/h/v1/") || len(p) != len("/t/h/v1/")+43 {
		t.Fatal("path", p)
	}
}

func TestHTTPSCheckResults(t *testing.T) {
	big := strings.Repeat("x", maxProofBytes+1)
	for name, tc := range map[string]struct {
		handler func(w http.ResponseWriter, r *http.Request, challenge string)
		want    HTTPSResult
	}{
		"a redirect": {func(w http.ResponseWriter, r *http.Request, _ string) {
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
		}, HTTPSRedirected},
		"a permanent redirect": {func(w http.ResponseWriter, r *http.Request, _ string) {
			http.Redirect(w, r, "/t/h/v1/x", http.StatusMovedPermanently)
		}, HTTPSRedirected},
		"the wrong body": {func(w http.ResponseWriter, _ *http.Request, _ string) { _, _ = w.Write([]byte("hello")) }, HTTPSNotThinpost},
		"a proof of another challenge": {func(w http.ResponseWriter, _ *http.Request, _ string) {
			_, _ = w.Write([]byte(proofPrefix + "someone-else"))
		}, HTTPSNotThinpost},
		"a not found":           {func(w http.ResponseWriter, _ *http.Request, _ string) { http.NotFound(w, nil) }, HTTPSNotThinpost},
		"a forbidden":           {func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(403) }, HTTPSNotThinpost},
		"a 5xx is transient":    {func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(500) }, HTTPSUnreachable},
		"a bad gateway":         {func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(502) }, HTTPSUnreachable},
		"a service unavailable": {func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(503) }, HTTPSUnreachable},
		"too many requests":     {func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(429) }, HTTPSUnreachable},
		"a 2xx without a body":  {func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(204) }, HTTPSNotThinpost},
		"an oversize body":      {func(w http.ResponseWriter, _ *http.Request, _ string) { _, _ = w.Write([]byte(big)) }, HTTPSNotThinpost},
		"a proof with trailing junk beyond the bound": {func(w http.ResponseWriter, _ *http.Request, c string) {
			_, _ = w.Write([]byte(proofPrefix + c + strings.Repeat(" ", maxProofBytes)))
		}, HTTPSNotThinpost},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTLSFixture(t, tc.handler)
			if got, _ := f.checker.checkHTTPS(context.Background(), certHost); got != tc.want {
				t.Fatalf("%v, want %v", got, tc.want)
			}
		})
	}
}

func TestHTTPSCheckFollowsNoRedirect(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { followed.Store(true); okHandler(w, nil, "") }))
	defer target.Close()
	f := newTLSFixture(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	if got, _ := f.checker.checkHTTPS(context.Background(), certHost); got != HTTPSRedirected || followed.Load() {
		t.Fatal(got, followed.Load())
	}
}

func TestHTTPSCheckCertificates(t *testing.T) {
	f := newTLSFixture(t, okHandler)
	t.Run("an untrusted certificate", func(t *testing.T) {
		f.checker.roots = x509.NewCertPool()
		if got, _ := f.checker.checkHTTPS(context.Background(), certHost); got != HTTPSCertificate {
			t.Fatal(got)
		}
	})
	t.Run("system roots do not trust the test certificate", func(t *testing.T) {
		f.checker.roots = nil
		if got, _ := f.checker.checkHTTPS(context.Background(), certHost); got != HTTPSCertificate {
			t.Fatal(got)
		}
	})
	t.Run("a certificate for another name", func(t *testing.T) {
		pool := x509.NewCertPool()
		pool.AddCert(f.server.Certificate())
		f.checker.roots = pool
		if got, _ := f.checker.checkHTTPS(context.Background(), "links.customer.test"); got != HTTPSCertificate {
			t.Fatal(got)
		}
	})
	t.Run("a host that does not speak TLS", func(t *testing.T) {
		plain := httptest.NewServer(http.HandlerFunc(okHandler2))
		defer plain.Close()
		_, port, _ := net.SplitHostPort(plain.Listener.Addr().String())
		f.checker.port = port
		if got, _ := f.checker.checkHTTPS(context.Background(), certHost); got != HTTPSCertificate {
			t.Fatal(got)
		}
	})
	// No certificate is ever seen on a failed handshake.
	if _, expires := f.checker.checkHTTPS(context.Background(), certHost); !expires.IsZero() {
		t.Fatal("expiry recorded without a handshake")
	}
}

func okHandler2(w http.ResponseWriter, _ *http.Request) { okHandler(w, nil, "") }

func TestHTTPSCheckUnreachable(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		f := newTLSFixture(t, okHandler)
		f.checker.connect = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") }
		if got, _ := f.checker.checkHTTPS(context.Background(), certHost); got != HTTPSUnreachable {
			t.Fatal(got)
		}
	})
	t.Run("no public address is ever dialed", func(t *testing.T) {
		f := newTLSFixture(t, okHandler)
		f.checker.Allow = func(netip.Addr) bool { return false }
		f.checker.connect = func(context.Context, string, string) (net.Conn, error) {
			t.Error("dialed a blocked address")
			return nil, errors.New("no")
		}
		if got, _ := f.checker.checkHTTPS(context.Background(), certHost); got != HTTPSUnreachable || f.hits.Load() != 0 {
			t.Fatal(got, f.hits.Load())
		}
	})
	t.Run("a nil allow function allows nothing", func(t *testing.T) {
		f := newTLSFixture(t, okHandler)
		f.checker.Allow = nil
		if got, _ := f.checker.checkHTTPS(context.Background(), certHost); got != HTTPSUnreachable || f.hits.Load() != 0 {
			t.Fatal(got, f.hits.Load())
		}
	})
	t.Run("NXDOMAIN is definitive, other DNS errors are not", func(t *testing.T) {
		f := newTLSFixture(t, okHandler)
		f.checker.Resolver = &fakeDNS{netErr: &net.DNSError{IsNotFound: true}}
		if got, _ := f.checker.checkHTTPS(context.Background(), certHost); got != HTTPSNotFound {
			t.Fatal("nxdomain", got)
		}
		for name, err := range map[string]error{"servfail": &net.DNSError{IsTemporary: true}, "a timeout": &net.DNSError{IsTimeout: true}, "plain": errors.New("boom")} {
			f.checker.Resolver = &fakeDNS{netErr: err}
			if got, _ := f.checker.checkHTTPS(context.Background(), certHost); got != HTTPSUnreachable {
				t.Fatal(name, got)
			}
		}
	})
	t.Run("a blackholed address does not use the whole budget", func(t *testing.T) {
		f := newTLSFixture(t, okHandler)
		f.checker.dialTimeout = 100 * time.Millisecond
		f.checker.Resolver = &fakeDNS{netIPs: []netip.Addr{netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("127.0.0.1")}}
		var dialer net.Dialer
		_, port, _ := net.SplitHostPort(f.server.Listener.Addr().String())
		f.checker.connect = func(ctx context.Context, network, address string) (net.Conn, error) {
			if strings.HasPrefix(address, "203.0.113.1:") {
				<-ctx.Done() // never answers
				return nil, ctx.Err()
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		}
		start := time.Now()
		if got, _ := f.checker.checkHTTPS(context.Background(), certHost); got != HTTPSOK || time.Since(start) > 3*time.Second {
			t.Fatal(got, time.Since(start))
		}
	})
	t.Run("a slow server times out", func(t *testing.T) {
		f := newTLSFixture(t, func(w http.ResponseWriter, r *http.Request, _ string) { <-r.Context().Done() })
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if got, _ := f.checker.checkHTTPS(ctx, certHost); got != HTTPSUnreachable {
			t.Fatal(got)
		}
	})
	t.Run("only the checked name may be dialed", func(t *testing.T) {
		c := &TrackingChecker{Resolver: &fakeDNS{}, Allow: func(netip.Addr) bool { return true }}
		for _, address := range []string{"other.example:443", "example.com:8443", "example.com"} {
			if _, err := c.dial(context.Background(), "tcp", address, "example.com"); !errors.Is(err, errBlockedAddress) {
				t.Fatal(address, err)
			}
		}
	})
	t.Run("without a verifier", func(t *testing.T) {
		f := newTLSFixture(t, okHandler)
		f.checker.Verify = nil
		if got, _ := f.checker.checkHTTPS(context.Background(), certHost); got != HTTPSUnreachable || f.hits.Load() != 0 {
			t.Fatal(got)
		}
	})
}

func TestDNSIsInformationalOnly(t *testing.T) {
	f := newTLSFixture(t, okHandler)
	for name, dns := range map[string]*fakeDNS{
		"missing":       {cnameErr: &net.DNSError{IsNotFound: true}, netIPs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		"lookup failed": {cnameErr: errors.New("servfail"), netIPs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		"a proxy":       {cname: certHost + ".", hosts: map[string][]string{certHost: {"198.51.100.1"}, "links.example.net": {"198.51.100.7"}}, netIPs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
	} {
		t.Run(name, func(t *testing.T) {
			f.checker.Resolver = dns
			out := f.checker.Check(context.Background(), certHost)
			if out.HTTPS != HTTPSOK || out.DNS == DNSOK {
				t.Fatalf("%+v: DNS must not block the HTTPS result", out)
			}
		})
	}
}

func TestCheckDNS(t *testing.T) {
	const domain = "links.customer.example"
	c := func(d *fakeDNS) *TrackingChecker { return &TrackingChecker{Resolver: d, Target: "links.example.net"} }
	for name, tc := range map[string]struct {
		dns      *fakeDNS
		want     DNSResult
		observed string
	}{
		"a CNAME to the tracking host":    {&fakeDNS{cname: "Links.Example.Net."}, DNSOK, ""},
		"NXDOMAIN":                        {&fakeDNS{cnameErr: &net.DNSError{IsNotFound: true}}, DNSMissing, ""},
		"a lookup error":                  {&fakeDNS{cnameErr: &net.DNSError{IsTimeout: true}}, DNSLookupFailed, ""},
		"a CNAME elsewhere":               {&fakeDNS{cname: "cdn.other.example.", hosts: map[string][]string{domain: {"198.51.100.1"}, "links.example.net": {"198.51.100.2"}}}, DNSWrongTarget, "cdn.other.example"},
		"flattened to the same addresses": {&fakeDNS{cname: domain + ".", hosts: map[string][]string{domain: {"198.51.100.2"}, "links.example.net": {"198.51.100.2", "198.51.100.3"}}}, DNSOK, ""},
		"a partial match":                 {&fakeDNS{cname: domain + ".", hosts: map[string][]string{domain: {"198.51.100.2", "198.51.100.9"}, "links.example.net": {"198.51.100.2"}}}, DNSWrongTarget, ""},
		"no addresses":                    {&fakeDNS{cname: domain + ".", hosts: map[string][]string{}}, DNSWrongTarget, ""},
		"the address lookup fails":        {&fakeDNS{cname: domain + ".", hostErr: errors.New("boom")}, DNSLookupFailed, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, observed := c(tc.dns).checkDNS(context.Background(), domain)
			if got != tc.want || observed != tc.observed {
				t.Fatalf("%v %q, want %v %q", got, observed, tc.want, tc.observed)
			}
		})
	}
	if got, _ := (&TrackingChecker{}).checkDNS(context.Background(), domain); got != DNSLookupFailed {
		t.Fatal("no resolver", got)
	}
}

func TestNextTracking(t *testing.T) {
	for name, tc := range map[string]struct {
		from  Status
		https HTTPSResult
		want  Status
	}{
		"pending passes":                             {Status{State: StatePending, Failures: 3}, HTTPSOK, Status{State: StateActive}},
		"pending stays on a failure":                 {Status{State: StatePending}, HTTPSCertificate, Status{State: StatePending, Failures: 1}},
		"active stays on a pass":                     {Status{State: StateActive}, HTTPSOK, Status{State: StateActive}},
		"active fails at once on a certificate":      {Status{State: StateActive}, HTTPSCertificate, Status{State: StateFailing, Failures: 1}},
		"active fails at once on a redirect":         {Status{State: StateActive}, HTTPSRedirected, Status{State: StateFailing, Failures: 1}},
		"active fails at once on not thinpost":       {Status{State: StateActive}, HTTPSNotThinpost, Status{State: StateFailing, Failures: 1}},
		"active waits out one unreachable":           {Status{State: StateActive}, HTTPSUnreachable, Status{State: StateActive, Failures: 1}},
		"active fails on a second unreachable":       {Status{State: StateActive, Failures: 1}, HTTPSUnreachable, Status{State: StateFailing, Failures: 2}},
		"failing recovers":                           {Status{State: StateFailing, Failures: 5}, HTTPSOK, Status{State: StateActive}},
		"active fails at once when the name is gone": {Status{State: StateActive}, HTTPSNotFound, Status{State: StateFailing, Failures: 1}},
		"removed stays removed on a pass":            {Status{State: StateRemoved, Failures: 2}, HTTPSOK, Status{State: StateRemoved}},
		"removed stays removed on a failure":         {Status{State: StateRemoved}, HTTPSCertificate, Status{State: StateRemoved, Failures: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := NextTracking(tc.from, tc.https); got != tc.want {
				t.Fatalf("%+v, want %+v", got, tc.want)
			}
		})
	}
	if TrackingInterval(StateRemoved, 0) != 24*time.Hour || TrackingInterval(StateActive, 0) != ActiveEvery || TrackingInterval(StatePending, time.Hour) != 10*time.Minute {
		t.Fatal("intervals")
	}
}
