package customdomain

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// A tracking domain is a strict subdomain of a verified sender domain that
// routes HTTPS to this deployment's tracking handler. Tracking links are not
// bound to any hostname, so what a check has to establish is only that an HTTPS
// request to the name, with a normal certificate check, reaches this
// deployment. It proves that with the host self-check: control asks
// /t/h/v1/<random challenge> and verifies the body with the tracking link keys.
//
// The DNS check is informational. It says whether the name is a CNAME to the
// tracking host (or resolves to the same addresses) so people can see what to
// change, but it never decides whether the domain is used: a name behind a
// CDN proxy resolves to the proxy and still works. Setting up the route and the
// certificate is the operator's job.

// DNSResult is the informational DNS outcome, stored as tracking_domains.dns_result.
type DNSResult string

const (
	// DNSOK: the name is a CNAME to the tracking host, or resolves to the same
	// addresses.
	DNSOK DNSResult = "ok"
	// DNSMissing: the name does not exist.
	DNSMissing DNSResult = "missing"
	// DNSWrongTarget: the name exists but points elsewhere (which is expected
	// behind a proxy).
	DNSWrongTarget DNSResult = "wrong_target"
	// DNSLookupFailed: DNS did not answer.
	DNSLookupFailed DNSResult = "lookup_failed"
)

// Valid reports whether r is a stored DNS result.
func (r DNSResult) Valid() bool {
	switch r {
	case DNSOK, DNSMissing, DNSWrongTarget, DNSLookupFailed:
		return true
	}
	return false
}

// HTTPSResult is the outcome of the HTTPS self-check, stored as
// tracking_domains.https_result. It alone decides activation.
type HTTPSResult string

const (
	// HTTPSOK: a valid certificate, and the body proves this deployment.
	HTTPSOK HTTPSResult = "ok"
	// HTTPSUnreachable: no connection, a timeout, a name with no public
	// address, a DNS error other than NXDOMAIN, or a 5xx or 429 answer (usually
	// a transient gateway error). Indeterminate.
	HTTPSUnreachable HTTPSResult = "unreachable"
	// HTTPSCertificate: the TLS handshake failed certificate validation, or the
	// host does not speak TLS.
	HTTPSCertificate HTTPSResult = "certificate"
	// HTTPSRedirected: the host answered with a redirect; the route must serve
	// /t/ itself.
	HTTPSRedirected HTTPSResult = "redirected"
	// HTTPSNotThinpost: the host answered, but not with this deployment's proof
	// (a 4xx other than 429, or a 200 with the wrong body). Definitive.
	HTTPSNotThinpost HTTPSResult = "not_thinpost"
	// HTTPSNotFound: the name does not exist in DNS (NXDOMAIN) when the check
	// connects: the record was removed. Definitive.
	HTTPSNotFound HTTPSResult = "not_found"
)

// Valid reports whether r is a stored HTTPS result.
func (r HTTPSResult) Valid() bool {
	switch r {
	case HTTPSOK, HTTPSUnreachable, HTTPSCertificate, HTTPSRedirected, HTTPSNotThinpost, HTTPSNotFound:
		return true
	}
	return false
}

// Indeterminate: nothing definite is known about the route, so an active domain
// gets another try before it is taken out of use.
func (r HTTPSResult) Indeterminate() bool { return r == HTTPSUnreachable }

// StateRemoved is a removed tracking domain's tombstone state. It is never used
// for links but is still checked and listed.
const StateRemoved = "removed"

// RemovedEvery is how often a removed domain is checked.
const RemovedEvery = 24 * time.Hour

// NextTracking applies one HTTPS result to a row's status; the DNS result has
// no say. A removed domain stays removed.
func NextTracking(current Status, https HTTPSResult) Status {
	if current.State == StateRemoved {
		failures := current.Failures
		if https == HTTPSOK {
			failures = 0
		} else if failures < maxFailures {
			failures++
		}
		return Status{State: StateRemoved, Failures: failures}
	}
	return advance(current, https == HTTPSOK, https.Indeterminate())
}

// TrackingInterval is how long to wait before the next check: Interval for a
// live domain, and a day for a removed one.
func TrackingInterval(state string, age time.Duration) time.Duration {
	if state == StateRemoved {
		return RemovedEvery
	}
	return Interval(state, age)
}

// TrackingOutcome is what one tracking check found.
type TrackingOutcome struct {
	DNS DNSResult
	// Observed is the name a CNAME resolved to when it was not the tracking
	// host, for showing people what is in DNS.
	Observed string
	HTTPS    HTTPSResult
	// CertificateExpires is the leaf certificate's NotAfter, when a handshake
	// completed.
	CertificateExpires time.Time
}

// TrackingResolver is the DNS the check needs. *net.Resolver satisfies it.
type TrackingResolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Limits of the HTTPS check.
const (
	// httpsTimeout bounds the whole HTTPS request.
	httpsTimeout = 10 * time.Second
	// dnsTimeout bounds the informational DNS lookups together.
	dnsTimeout = 5 * time.Second
	// maxProofBytes bounds the body read; a real proof is about 90 bytes.
	maxProofBytes = 512
	// addressDialTimeout bounds one connection attempt, so a blackholed address
	// cannot use the whole budget before the next address is tried.
	addressDialTimeout = 4 * time.Second
	// maxDialAddresses bounds the addresses tried for one name.
	maxDialAddresses = 8
)

// TrackingChecker runs tracking-domain checks. Build it with every field a
// production check needs; the unexported ones are test seams.
type TrackingChecker struct {
	Resolver TrackingResolver
	// Target is the deployment's tracking host (the name a CNAME should point
	// at).
	Target string
	// Verify reports whether body is a valid host-check proof of challenge
	// (tracking.LinkKeys.VerifyHostCheck).
	Verify func(challenge, body string) bool
	// Allow decides which addresses the HTTPS check may connect to; production
	// passes webhook.PublicAddress so a name pointing inside the network is
	// never fetched. A nil Allow allows nothing.
	Allow func(netip.Addr) bool

	roots   *x509.CertPool
	connect func(ctx context.Context, network, address string) (net.Conn, error)
	port    string
	// dialTimeout overrides addressDialTimeout in tests.
	dialTimeout time.Duration
}

// Check runs the DNS and HTTPS checks for domain. Neither depends on the
// other.
func (c *TrackingChecker) Check(ctx context.Context, domain string) TrackingOutcome {
	dnsCtx, cancel := context.WithTimeout(ctx, dnsTimeout)
	var out TrackingOutcome
	out.DNS, out.Observed = c.checkDNS(dnsCtx, domain)
	cancel()
	out.HTTPS, out.CertificateExpires = c.checkHTTPS(ctx, domain)
	return out
}

func (c *TrackingChecker) checkDNS(ctx context.Context, domain string) (DNSResult, string) {
	if c.Resolver == nil || c.Target == "" {
		return DNSLookupFailed, ""
	}
	rooted := strings.TrimSuffix(domain, ".") + "."
	canonical, err := c.Resolver.LookupCNAME(ctx, rooted)
	if err != nil {
		return classifyDNSError(err), ""
	}
	observed := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(canonical)), ".")
	if len(observed) > 253 {
		observed = observed[:253]
	}
	if observed == c.Target {
		return DNSOK, ""
	}
	if observed == domain {
		observed = ""
	}
	// Flattened records (a CNAME that resolves at the provider) show as the
	// same addresses as the tracking host.
	have, err := c.Resolver.LookupHost(ctx, rooted)
	if err != nil {
		return classifyDNSError(err), observed
	}
	want, err := c.Resolver.LookupHost(ctx, c.Target+".")
	if err != nil {
		return DNSLookupFailed, observed
	}
	if len(have) > 0 && subset(have, want) {
		return DNSOK, ""
	}
	return DNSWrongTarget, observed
}

func classifyDNSError(err error) DNSResult {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return DNSMissing
	}
	return DNSLookupFailed
}

func subset(have, want []string) bool {
	set := make(map[string]bool, len(want))
	for _, w := range want {
		set[w] = true
	}
	for _, h := range have {
		if !set[h] {
			return false
		}
	}
	return true
}

var (
	errBlockedAddress = errors.New("no public address")
	errNameNotFound   = errors.New("name not found")
)

func (c *TrackingChecker) checkHTTPS(ctx context.Context, domain string) (HTTPSResult, time.Time) {
	if c.Verify == nil {
		return HTTPSUnreachable, time.Time{}
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return HTTPSUnreachable, time.Time{}
	}
	challenge := base64.RawURLEncoding.EncodeToString(raw[:])
	transport := &http.Transport{
		// No proxy: the check must reach the name itself, and an environment
		// proxy would let a name be fetched from somewhere else.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return c.dial(ctx, network, address, domain)
		},
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, ServerName: domain, RootCAs: c.roots},
		DisableKeepAlives:      true,
		TLSHandshakeTimeout:    httpsTimeout,
		ResponseHeaderTimeout:  httpsTimeout,
		MaxResponseHeaderBytes: 8 << 10,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   httpsTimeout,
		// A redirect is reported, never followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx, cancel := context.WithTimeout(ctx, httpsTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+"/t/h/v1/"+challenge, nil)
	if err != nil {
		return HTTPSUnreachable, time.Time{}
	}
	request.Header.Set("User-Agent", "thinpost-domain-check")
	response, err := client.Do(request)
	if err != nil {
		return classifyHTTPSError(err), time.Time{}
	}
	defer response.Body.Close()
	var expires time.Time
	if response.TLS != nil && len(response.TLS.PeerCertificates) > 0 {
		expires = response.TLS.PeerCertificates[0].NotAfter
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return HTTPSRedirected, expires
	}
	// A 5xx or 429 is usually a transient proxy or gateway error: indeterminate.
	if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests {
		return HTTPSUnreachable, expires
	}
	if response.StatusCode != http.StatusOK {
		return HTTPSNotThinpost, expires
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProofBytes+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return HTTPSUnreachable, expires
		}
		return HTTPSNotThinpost, expires
	}
	if len(body) > maxProofBytes || !c.Verify(challenge, string(body)) {
		return HTTPSNotThinpost, expires
	}
	return HTTPSOK, expires
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// classifyHTTPSError maps a transport error to a fixed result. Certificate
// problems (an unknown issuer, a name mismatch, an expired certificate, a host
// that does not speak TLS) are definitive; anything else is indeterminate.
func classifyHTTPSError(err error) HTTPSResult {
	var verify *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.Is(err, errNameNotFound) {
		return HTTPSNotFound
	}
	switch {
	case errors.As(err, &verify), errors.As(err, &record), errors.As(err, &unknown), errors.As(err, &hostname), errors.As(err, &invalid):
		return HTTPSCertificate
	}
	// net/http reports a plain-HTTP answer to a TLS handshake only as text.
	if strings.Contains(err.Error(), "server gave HTTP response to HTTPS client") {
		return HTTPSCertificate
	}
	return HTTPSUnreachable
}

// dial connects to the name on port 443 and only to public addresses. The
// request's own host is the only name it will resolve, resolved rooted so a
// cluster's search domains never apply (the pattern of the webhook sender).
func (c *TrackingChecker) dial(ctx context.Context, network, address, wantHost string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !strings.EqualFold(host, wantHost) || port != "443" {
		return nil, errBlockedAddress
	}
	if c.Resolver == nil || c.Allow == nil {
		return nil, errBlockedAddress
	}
	addresses, err := c.Resolver.LookupNetIP(ctx, "ip", host+".")
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, errNameNotFound
		}
		return nil, fmt.Errorf("resolve: %w", err)
	}
	perAddress := addressDialTimeout
	if c.dialTimeout > 0 {
		perAddress = c.dialTimeout
	}
	if c.port != "" {
		port = c.port
	}
	var dialer net.Dialer
	dial := dialer.DialContext
	if c.connect != nil {
		dial = c.connect
	}
	var lastErr error
	allowed := false
	for i, address := range addresses {
		if i >= maxDialAddresses {
			break
		}
		address = address.Unmap()
		if !c.Allow(address) {
			continue
		}
		allowed = true
		attempt, cancel := context.WithTimeout(ctx, perAddress)
		conn, err := dial(attempt, network, net.JoinHostPort(address.String(), port))
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if !allowed {
		return nil, errBlockedAddress
	}
	return nil, fmt.Errorf("connect: %w", lastErr)
}
