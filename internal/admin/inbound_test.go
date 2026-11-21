package admin

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"strings"
	"testing"
)

type inboundFake struct {
	*fakeStore
	domains   map[string]InboundDomain
	owner     bool // another app already owns the domain
	limits    InboundLimits
	mxStatus  string
	deleted   string
	enabledTo *bool
}

func (s *inboundFake) ListInboundDomains(context.Context, string) ([]InboundDomain, error) {
	var out []InboundDomain
	for _, d := range s.domains {
		out = append(out, d)
	}
	return out, nil
}
func (s *inboundFake) RequestInboundDomain(_ context.Context, actor, app, domain string) (InboundDomainChallenge, error) {
	s.actor, s.app = actor, app
	d := InboundDomain{Name: domain, State: InboundDomainPending}
	s.domains[domain] = d
	return InboundDomainChallenge{Domain: d, DNSName: "_thinpost." + domain, DNSValue: "thinpost-inbound-verification=test"}, nil
}
func (s *inboundFake) InboundDomainChallenge(_ context.Context, _, domain string) (InboundDomain, [32]byte, error) {
	d, ok := s.domains[domain]
	if !ok {
		return d, [32]byte{}, ErrNotFound
	}
	return d, sha256.Sum256([]byte("thinpost-inbound-verification=test")), nil
}
func (s *inboundFake) VerifyInboundDomain(_ context.Context, actor, app, domain string, _ [32]byte) (InboundDomain, error) {
	if s.owner {
		return InboundDomain{}, ErrConflict
	}
	s.actor, s.app = actor, app
	d := s.domains[domain]
	d.State = InboundDomainActive
	s.domains[domain] = d
	return d, nil
}
func (s *inboundFake) RecordInboundMX(_ context.Context, _, _, domain, status string) (InboundDomain, error) {
	s.mxStatus = status
	d := s.domains[domain]
	d.MXStatus = &status
	return d, nil
}
func (s *inboundFake) SetInboundDomainEnabled(_ context.Context, _, _, domain string, enabled bool) (InboundDomain, error) {
	s.enabledTo = &enabled
	d := s.domains[domain]
	d.State = map[bool]string{true: InboundDomainActive, false: InboundDomainDisabled}[enabled]
	return d, nil
}
func (s *inboundFake) DeleteInboundDomain(_ context.Context, _, _, domain string) error {
	s.deleted = domain
	return nil
}
func (s *inboundFake) GetInboundLimits(context.Context, string) (InboundLimits, error) {
	return s.limits, nil
}
func (s *inboundFake) SetInboundLimits(_ context.Context, actor, app string, l InboundLimits) (InboundLimits, error) {
	s.actor, s.app, s.limits = actor, app, l
	return l, nil
}

type fakeMX struct {
	records []*net.MX
	err     error
}

func (m fakeMX) LookupMX(context.Context, string) ([]*net.MX, error) { return m.records, m.err }

func mx(hosts ...string) []*net.MX {
	var out []*net.MX
	for i, host := range hosts {
		out = append(out, &net.MX{Host: host, Pref: uint16(10 * (i + 1))})
	}
	return out
}

func TestClassifyMXAndCheck(t *testing.T) {
	const host = "smtp.mx.example.net"
	notFound := &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}
	for _, test := range []struct {
		name    string
		records []*net.MX
		err     error
		want    string
	}{
		{"thinpost", mx("smtp.mx.example.net."), nil, MXThinpost},
		{"case and dot", mx("SMTP.MX.Example.NET"), nil, MXThinpost},
		{"two of ours", mx("smtp.mx.example.net.", "smtp.mx.example.net."), nil, MXThinpost},
		{"mixed", mx("smtp.mx.example.net.", "mail.other.example."), nil, MXMixed},
		{"other", mx("mail.other.example."), nil, MXOther},
		{"lookalike suffix", mx("evil-smtp.mx.example.net."), nil, MXOther},
		{"no records", nil, nil, MXNone},
		{"not found", nil, notFound, MXNone},
		{"null MX", mx("."), nil, MXNull},
		{"null MX with another is other", mx(".", "mail.other.example."), nil, MXOther},
		{"lookup failed", nil, errors.New("timeout"), MXError},
		{"temporary failure", nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}, MXError},
		{"partial answer with a soft error", mx("smtp.mx.example.net."), &net.DNSError{Err: "invalid target"}, MXThinpost},
	} {
		if got := CheckMX(context.Background(), fakeMX{test.records, test.err}, "customer.example", host); got != test.want {
			t.Errorf("%s: got %s, want %s", test.name, got, test.want)
		}
	}
	if ClassifyMX(mx("smtp.mx.example.net."), "") != MXOther {
		t.Error("an unconfigured host matched")
	}
}

func inboundPath(app, rest string) string { return "/api/v1/apps/" + app + "/" + rest }

func TestInboundDomainRoutes(t *testing.T) {
	app := strings.Repeat("a", 32)
	store := &inboundFake{fakeStore: &fakeStore{identity: EdgeIdentity{MainHostname: "smtp.mx.example.net", MainAddress: "45.0.0.10", Hostnames: []string{"smtp.mx.example.net", "bounce.example.net"}}}, domains: map[string]InboundDomain{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, ReverseDNS: &fakeReverse{names: []string{"smtp.mx.example.net."}, hosts: []string{"45.0.0.10"}},
		DNS: fakeDNS{records: []string{"thinpost-inbound-verification=test"}}, MX: fakeMX{records: mx("smtp.mx.example.net.")}}
	if w := send(h, "GET", inboundPath(app, "inbound-domains"), "", ""); w.Code != 401 {
		t.Fatal("unauthenticated", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "POST", inboundPath(app, "inbound-domains"), `{"domain":"a.example.com"}`, "valid"); w.Code != 403 {
		t.Fatal("non-admin", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	// List carries the MX host.
	if w := send(h, "GET", inboundPath(app, "inbound-domains"), "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"domains":[]`) || !strings.Contains(w.Body.String(), `"mx_host":"smtp.mx.example.net"`) || !strings.Contains(w.Body.String(), `"mx_host_status":"ok"`) {
		t.Fatal("list", w.Code, w.Body.String())
	}
	// Create.
	w := send(h, "POST", inboundPath(app, "inbound-domains"), `{"domain":"Mail.Example.COM"}`, "valid")
	if w.Code != 201 || store.actor != "operator-1" || store.app != app || !strings.Contains(w.Body.String(), `"dns_name":"_thinpost.mail.example.com"`) || !strings.Contains(w.Body.String(), `"mx_host":"smtp.mx.example.net"`) {
		t.Fatal("create", w.Code, w.Body.String())
	}
	// Reserved and invalid names never reach the store.
	store.domains = map[string]InboundDomain{}
	for _, name := range []string{"bounce.example.net", "mail.bounce.example.net", "smtp.mx.example.net", "x.smtp.mx.example.net", "not a domain", "10.0.0.1", ""} {
		if w := send(h, "POST", inboundPath(app, "inbound-domains"), `{"domain":"`+name+`"}`, "valid"); w.Code != 400 {
			t.Fatal("accepted", name, w.Code)
		}
	}
	if len(store.domains) != 0 {
		t.Fatal("a rejected domain reached the store")
	}
	if w := send(h, "POST", inboundPath(app, "inbound-domains"), `{"domain":"notbounce.example.net"}`, "valid"); w.Code != 201 {
		t.Fatal("a sibling of the reserved name was refused", w.Code)
	}
	if w := send(h, "POST", inboundPath(app, "inbound-domains"), `{"domain":"a.example.com","extra":1}`, "valid"); w.Code != 400 {
		t.Fatal("unknown field accepted", w.Code)
	}

	// Verify: unknown, mismatch, owned elsewhere, success.
	if w := send(h, "POST", inboundPath(app, "inbound-domains/unknown.example.com/verify"), "", "valid"); w.Code != 404 {
		t.Fatal("verify of an unknown claim", w.Code)
	}
	send(h, "POST", inboundPath(app, "inbound-domains"), `{"domain":"mail.example.com"}`, "valid")
	h.DNS = fakeDNS{records: []string{"v=spf1 -all", "thinpost-inbound-verification=other"}}
	if w := send(h, "POST", inboundPath(app, "inbound-domains/mail.example.com/verify"), "", "valid"); w.Code != 409 {
		t.Fatal("a mismatched record verified", w.Code)
	}
	h.DNS = fakeDNS{records: []string{"v=spf1 -all", "thinpost-inbound-verification=test"}}
	store.owner = true
	if w := send(h, "POST", inboundPath(app, "inbound-domains/mail.example.com/verify"), "", "valid"); w.Code != 409 {
		t.Fatal("a domain owned by another app verified", w.Code)
	}
	store.owner = false
	if w := send(h, "POST", inboundPath(app, "inbound-domains/mail.example.com/verify"), `{}`, "valid"); w.Code != 400 {
		t.Fatal("verify accepted a body", w.Code)
	}
	if w := send(h, "POST", inboundPath(app, "inbound-domains/mail.example.com/verify"), "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"active"`) {
		t.Fatal("verify", w.Code, w.Body.String())
	}
	if w := send(h, "POST", inboundPath(app, "inbound-domains/mail.example.com/verify"), "", "valid"); w.Code != 200 {
		t.Fatal("verify of an active domain is not idempotent", w.Code)
	}
	h.DNS = nil
	store.domains["pending.example.com"] = InboundDomain{Name: "pending.example.com", State: InboundDomainPending}
	if w := send(h, "POST", inboundPath(app, "inbound-domains/pending.example.com/verify"), "", "valid"); w.Code != 503 {
		t.Fatal("verify without a resolver", w.Code)
	}

	// MX check.
	if w := send(h, "POST", inboundPath(app, "inbound-domains/mail.example.com/check-mx"), "", "valid"); w.Code != 200 || store.mxStatus != MXThinpost || !strings.Contains(w.Body.String(), `"mx_status":"thinpost"`) {
		t.Fatal("check-mx", w.Code, w.Body.String())
	}
	h.MX = fakeMX{err: errors.New("timeout")}
	if w := send(h, "POST", inboundPath(app, "inbound-domains/mail.example.com/check-mx"), "", "valid"); w.Code != 200 || store.mxStatus != MXError {
		t.Fatal("a failed lookup is recorded as an error", w.Code, store.mxStatus)
	}
	// Before the sending worker reports, there is no host to check against.
	h.MX = fakeMX{records: mx("smtp.mx.example.net.")}
	store.identity = EdgeIdentity{}
	if w := send(h, "POST", inboundPath(app, "inbound-domains/mail.example.com/check-mx"), "", "valid"); w.Code != 503 {
		t.Fatal("check-mx before a report", w.Code)
	}
	if w := send(h, "GET", inboundPath(app, "inbound-domains"), "", "valid"); strings.Contains(w.Body.String(), `"mx_host":`) || !strings.Contains(w.Body.String(), `"mx_host_status":"not_reported"`) {
		t.Fatal("a host was reported before any report", w.Body.String())
	}
	// A name public DNS does not map to the reported address is withheld, and
	// the MX check refuses to use it.
	store.identity = EdgeIdentity{MainHostname: "smtp.mx.example.net", MainAddress: "45.0.0.10", Hostnames: []string{"smtp.mx.example.net"}}
	timeout := &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	for name, dns := range map[string]*fakeReverse{
		"mismatch":     {names: []string{"smtp.mx.example.net."}, hosts: []string{"45.0.0.99"}},
		"nxdomain":     {names: []string{"smtp.mx.example.net."}, hostErr: &net.DNSError{IsNotFound: true}},
		"timeout":      {names: []string{"smtp.mx.example.net."}, hostErr: timeout},
		"reverse only": {names: []string{"other.example.net."}, hosts: []string{"45.0.0.10"}},
	} {
		h.ReverseDNS = dns
		if w := send(h, "GET", inboundPath(app, "inbound-domains"), "", "valid"); w.Code != 200 || strings.Contains(w.Body.String(), `"mx_host":`) || strings.Contains(w.Body.String(), "smtp.mx.example.net") || !strings.Contains(w.Body.String(), `"mx_host_status":"dns_unconfirmed"`) {
			t.Fatal(name, "host shown", w.Body.String())
		}
		if w := send(h, "POST", inboundPath(app, "inbound-domains"), `{"domain":"again.example.com"}`, "valid"); w.Code != 201 || strings.Contains(w.Body.String(), `"mx_host":`) || !strings.Contains(w.Body.String(), `"mx_host_status":"dns_unconfirmed"`) {
			t.Fatal(name, "challenge shows the host", w.Code, w.Body.String())
		}
		if w := send(h, "POST", inboundPath(app, "inbound-domains/mail.example.com/check-mx"), "", "valid"); w.Code != 503 {
			t.Fatal(name, "check-mx used an unconfirmed host", w.Code)
		}
		// Reserved names stay reserved whatever DNS says.
		if w := send(h, "POST", inboundPath(app, "inbound-domains"), `{"domain":"smtp.mx.example.net"}`, "valid"); w.Code != 400 {
			t.Fatal(name, "reserved name accepted", w.Code)
		}
	}

	// Disable, enable, delete.
	if w := send(h, "POST", inboundPath(app, "inbound-domains/mail.example.com/disable"), "", "valid"); w.Code != 200 || store.enabledTo == nil || *store.enabledTo {
		t.Fatal("disable", w.Code)
	}
	if w := send(h, "POST", inboundPath(app, "inbound-domains/mail.example.com/enable"), "", "valid"); w.Code != 200 || !*store.enabledTo {
		t.Fatal("enable", w.Code)
	}
	if w := send(h, "POST", inboundPath(app, "inbound-domains/mail.example.com/explode"), "", "valid"); w.Code != 404 {
		t.Fatal("unknown action", w.Code)
	}
	if w := send(h, "GET", inboundPath(app, "inbound-domains/mail.example.com/verify"), "", "valid"); w.Code != 405 {
		t.Fatal("GET verify", w.Code)
	}
	if w := send(h, "DELETE", inboundPath(app, "inbound-domains/mail.example.com"), "", "valid"); w.Code != 204 || store.deleted != "mail.example.com" {
		t.Fatal("delete", w.Code)
	}
	if w := send(h, "GET", inboundPath(app, "inbound-domains/mail.example.com"), "", "valid"); w.Code != 405 {
		t.Fatal("GET of one domain", w.Code)
	}
	if w := send(h, "DELETE", inboundPath(app, "inbound-domains/Bad_Domain"), "", "valid"); w.Code != 400 {
		t.Fatal("delete of an invalid name", w.Code)
	}
	// A store without inbound support is unavailable, not a 404.
	plain := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}}
	if w := send(plain, "GET", inboundPath(app, "inbound-domains"), "", "valid"); w.Code != 503 {
		t.Fatal("store without inbound", w.Code)
	}
}

func TestInboundLimitsValidation(t *testing.T) {
	app := strings.Repeat("a", 32)
	store := &inboundFake{fakeStore: &fakeStore{}, domains: map[string]InboundDomain{}, limits: InboundLimits{DailyLimit: 2000, PendingLimit: 500}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
	path := inboundPath(app, "inbound-limits")
	if w := send(h, "PUT", path, `{"daily_limit":10,"pending_limit":5}`, "bad"); w.Code != 401 {
		t.Fatal("unauthenticated", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "PUT", path, `{"daily_limit":10,"pending_limit":5}`, "valid"); w.Code != 403 {
		t.Fatal("non-admin", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	if w := send(h, "GET", path, "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"daily_limit":2000`) || !strings.Contains(w.Body.String(), `"pending_limit":500`) {
		t.Fatal("get", w.Code, w.Body.String())
	}
	for _, body := range []string{
		`{}`, `{"daily_limit":10}`, `{"pending_limit":5}`,
		`{"daily_limit":0,"pending_limit":5}`, `{"daily_limit":10,"pending_limit":0}`,
		`{"daily_limit":1000001,"pending_limit":5}`, `{"daily_limit":10,"pending_limit":100001}`,
		`{"daily_limit":-1,"pending_limit":5}`, `{"daily_limit":"10","pending_limit":5}`, `{"daily_limit":10,"pending_limit":5,"extra":1}`,
	} {
		if w := send(h, "PUT", path, body, "valid"); w.Code != 400 {
			t.Fatal("accepted", body, w.Code)
		}
	}
	if store.limits.DailyLimit != 2000 {
		t.Fatal("a rejected update reached the store")
	}
	for _, body := range []string{`{"daily_limit":1,"pending_limit":1}`, `{"daily_limit":1000000,"pending_limit":100000}`} {
		if w := send(h, "PUT", path, body, "valid"); w.Code != 200 {
			t.Fatal("rejected", body, w.Code)
		}
	}
	if store.actor != "operator-1" || store.app != app || store.limits.DailyLimit != 1000000 {
		t.Fatal("update not attributed", store.actor, store.limits)
	}
	if w := send(h, "DELETE", path, "", "valid"); w.Code != 405 {
		t.Fatal("DELETE", w.Code)
	}
}
