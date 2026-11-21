package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

type smtpTestStore struct {
	*fakeStore
	policy      SMTPFromPolicy
	credentials []SMTPCredential
	setErr      error
	issueErr    error
	revokeErr   error
	sets        int
	revoked     string
}

func (s *smtpTestStore) GetSMTPSettings(_ context.Context, app string) (SMTPFromPolicy, error) {
	s.app = app
	return s.policy, nil
}
func (s *smtpTestStore) SetSMTPSettings(_ context.Context, actor, app string, p SMTPFromPolicy) (SMTPFromPolicy, error) {
	if s.setErr != nil {
		return SMTPFromPolicy{}, s.setErr
	}
	s.actor, s.app, s.policy = actor, app, p
	s.sets++
	return p, nil
}
func (s *smtpTestStore) ListSMTPCredentials(context.Context, string) ([]SMTPCredential, error) {
	return s.credentials, nil
}
func (s *smtpTestStore) IssueSMTPCredential(_ context.Context, actor, app, name string) (IssuedSMTPCredential, error) {
	if s.issueErr != nil {
		return IssuedSMTPCredential{}, s.issueErr
	}
	s.actor, s.app = actor, app
	id := strings.Repeat("c", 32)
	return IssuedSMTPCredential{Credential: SMTPCredential{ID: id, Name: name, Username: id, Active: true, CreatedAt: time.Unix(0, 0).UTC()}, Username: id, Password: "tps_" + strings.Repeat("x", 43)}, nil
}
func (s *smtpTestStore) RevokeSMTPCredential(_ context.Context, actor, app, id string) error {
	s.actor, s.app, s.revoked = actor, app, id
	return s.revokeErr
}
func (s *smtpTestStore) SMTPConnection(context.Context, string) (SMTPConnection, error) {
	host, source := "smtp.example.org", SMTPHostPrimary
	return SMTPConnection{Host: &host, HostAddress: "45.0.0.10", HostSource: &source, HostStatus: SMTPHostOK, Ports: SMTPPorts(), ObservedPorts: []int{}}, nil
}

type fakeReadiness struct {
	calls []string
	err   error
}

func (f *fakeReadiness) Ready(_ context.Context, app, domain string) error {
	f.calls = append(f.calls, app+" "+domain)
	return f.err
}

func TestSMTPRoutes(t *testing.T) {
	store := &smtpTestStore{fakeStore: &fakeStore{}, policy: SMTPFromPolicy{Mode: "off", OnlyUnauthorized: true}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, ReverseDNS: &fakeReverse{names: []string{"smtp.example.org."}, hosts: []string{"45.0.0.10"}}}
	app := strings.Repeat("a", 32)
	base := "/api/v1/apps/" + app + "/smtp"

	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "GET", base, "", "valid"); w.Code != 403 {
		t.Fatal("non-admin read", w.Code)
	}
	if w := send(h, "POST", base+"/credentials", `{"name":"x"}`, "valid"); w.Code != 403 || store.actor != "" {
		t.Fatal("non-admin create", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}

	w := send(h, "GET", base, "", "valid")
	var got struct {
		Settings struct {
			From         SMTPFromPolicy      `json:"from"`
			SigningReady *[]SMTPSigningReady `json:"signing_ready"`
		} `json:"settings"`
		Credentials []SMTPCredential `json:"credentials"`
		Connection  SMTPConnection   `json:"connection"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Settings.From.Mode != "off" || got.Settings.SigningReady != nil ||
		got.Credentials == nil || len(got.Credentials) != 0 || *got.Connection.Host != "smtp.example.org" || len(got.Connection.Ports) != 2 || got.Connection.HostStatus != SMTPHostOK {
		t.Fatal("read", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"signing_ready":null`) || !strings.Contains(w.Body.String(), `"credentials":[]`) {
		t.Fatal("null/empty shapes", w.Body.String())
	}
	if w = send(h, "POST", base, "", "valid"); w.Code != 405 {
		t.Fatal("POST /smtp", w.Code)
	}

	// Settings.
	put := base + "/settings"
	for _, body := range []string{
		``, `{}`, `{"from":null}`,
		`{"from":{"mode":"off"}}`,
		`{"from":{"mode":"off","only_unauthorized":true,"keep_origin":false}}`,
		`{"from":{"mode":"rewrite","only_unauthorized":true}}`,
		`{"from":{"mode":"domain","only_unauthorized":true}}`,
		`{"from":{"mode":"domain","domain":"Example.com","only_unauthorized":true}}`,
		`{"from":{"mode":"address","address":"\"a b\"@example.com","only_unauthorized":true}}`,
		`{"from":{"mode":"address","address":"a..b@example.com","only_unauthorized":true}}`,
		`{"from":{"mode":"address","address":"é@example.com","only_unauthorized":true}}`,
		`{"from":{"mode":"domain","domain":"example.com","address":"a@example.com","only_unauthorized":true}}`,
		`{"from":{"mode":"domain","domain":"example.com","keep_origin":true,"only_unauthorized":true}}`,
		`{"from":{"mode":"off","domain":"example.com","only_unauthorized":true}}`,
		`{"from":{"mode":"off","only_unauthorized":true},"extra":1}`,
	} {
		if w = send(h, "PUT", put, body, "valid"); w.Code != 400 || store.sets != 0 {
			t.Fatal("bad body accepted", body, w.Code)
		}
	}
	if w = send(h, "GET", put, "", "valid"); w.Code != 405 {
		t.Fatal("GET settings", w.Code)
	}
	ready := &fakeReadiness{}
	h.SigningReadiness = ready
	w = send(h, "PUT", put, `{"from":{"mode":"domain","domain":"example.com","only_unauthorized":false}}`, "valid")
	if w.Code != 200 || store.sets != 1 || store.actor != "operator-1" || store.policy.Mode != "domain" || *store.policy.Domain != "example.com" ||
		!strings.Contains(w.Body.String(), `"signing_ready":[{"domain":"example.com","ready":true}]`) || len(ready.calls) != 1 || ready.calls[0] != app+" example.com" {
		t.Fatal("domain update", w.Code, w.Body.String(), ready.calls)
	}
	ready.err = errors.New("not ready")
	if w = send(h, "PUT", put, `{"from":{"mode":"address","address":"No-Reply+x@example.com","only_unauthorized":true}}`, "valid"); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"signing_ready":[{"domain":"example.com","ready":false}]`) {
		t.Fatal("address update", w.Code, w.Body.String())
	}
	if w = send(h, "PUT", put, `{"from":{"mode":"off","only_unauthorized":true}}`, "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"signing_ready":[]`) {
		t.Fatal("off update", w.Code, w.Body.String())
	}
	store.setErr = ErrConflict
	if w = send(h, "PUT", put, `{"from":{"mode":"domain","domain":"example.net","only_unauthorized":true}}`, "valid"); w.Code != 409 {
		t.Fatal("unverified domain", w.Code)
	}
	store.setErr = nil

	// Credentials.
	cred := base + "/credentials"
	for _, body := range []string{``, `{}`, `{"name":""}`, `{"name":" x"}`, `{"name":"` + strings.Repeat("n", 129) + `"}`, `{"name":"x","extra":1}`} {
		if w = send(h, "POST", cred, body, "valid"); w.Code != 400 {
			t.Fatal("bad name accepted", body, w.Code)
		}
	}
	w = send(h, "POST", cred, `{"name":"Printer"}`, "valid")
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"username":"`+strings.Repeat("c", 32)+`"`) || !strings.Contains(w.Body.String(), `"password":"tps_`) || !strings.Contains(w.Body.String(), `"credential":{`) {
		t.Fatal("create", w.Code, w.Body.String())
	}
	store.issueErr = ErrConflict
	if w = send(h, "POST", cred, `{"name":"Printer"}`, "valid"); w.Code != 409 {
		t.Fatal("limit", w.Code)
	}
	if w = send(h, "GET", cred, "", "valid"); w.Code != 405 {
		t.Fatal("GET credentials", w.Code)
	}
	id := strings.Repeat("c", 32)
	if w = send(h, "DELETE", cred+"/"+id, "", "valid"); w.Code != 204 || store.revoked != id || store.app != app {
		t.Fatal("revoke", w.Code)
	}
	store.revokeErr = ErrNotFound
	if w = send(h, "DELETE", cred+"/"+id, "", "valid"); w.Code != 404 {
		t.Fatal("revoke unknown", w.Code)
	}
	if w = send(h, "DELETE", cred+"/bad", "", "valid"); w.Code != 404 {
		t.Fatal("malformed id", w.Code)
	}
	if w = send(h, "GET", cred+"/"+id, "", "valid"); w.Code != 405 {
		t.Fatal("GET credential", w.Code)
	}
	if w = send(h, "GET", base+"/other", "", "valid"); w.Code != 404 {
		t.Fatal("unknown sub-route", w.Code)
	}
}

func TestSMTPUnavailableWithoutStore(t *testing.T) {
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}}
	if w := send(h, "GET", "/api/v1/apps/"+strings.Repeat("a", 32)+"/smtp", "", "valid"); w.Code != 503 {
		t.Fatal("expected 503", w.Code)
	}
}

func TestNormalizeSMTPFromPolicy(t *testing.T) {
	str := func(s string) *string { return &s }
	if p, ok := NormalizeSMTPFromPolicy(SMTPFromPolicy{Mode: "off", OnlyUnauthorized: true}); !ok || p.Domain != nil || p.Address != nil {
		t.Fatal("off", p, ok)
	}
	if p, ok := NormalizeSMTPFromPolicy(SMTPFromPolicy{Mode: "address", Address: str("A.b+c@Example.com")}); ok {
		t.Fatal("address is not normalized before storing", p)
	}
	for _, address := range []string{strings.Repeat("a", 65) + "@example.com", "a@localhost", "a@127.0.0.1", ".a@example.com", "a.@example.com", "a b@example.com", "a@" + "-example.com"} {
		if _, ok := NormalizeSMTPFromPolicy(SMTPFromPolicy{Mode: "address", Address: &address}); ok {
			t.Fatal("address accepted", address)
		}
	}
}

// The host is client-facing and comes from the untrusted sending worker, so it
// is shown only when public DNS maps it to the address the worker reported.
func TestSMTPHostIsHiddenUnlessDNSConfirmsIt(t *testing.T) {
	store := &smtpTestStore{fakeStore: &fakeStore{}, policy: SMTPFromPolicy{Mode: "off", OnlyUnauthorized: true}}
	path := "/api/v1/apps/" + strings.Repeat("a", 32) + "/smtp"
	timeout := &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	ptr := []string{"smtp.example.org."}
	for name, test := range map[string]struct {
		dns    *fakeReverse
		nilDNS bool
		shown  bool
	}{
		"matches":                              {dns: &fakeReverse{names: ptr, hosts: []string{"45.0.0.10"}}, shown: true},
		"reverse name differs in case and dot": {dns: &fakeReverse{names: []string{"SMTP.Example.org"}, hosts: []string{"45.0.0.10"}}, shown: true},
		"among several":                        {dns: &fakeReverse{names: []string{"other.example.net.", "smtp.example.org."}, hosts: []string{"2001:db8::1", "45.0.0.99", "45.0.0.10"}}, shown: true},
		"another address":                      {dns: &fakeReverse{names: ptr, hosts: []string{"45.0.0.99"}}},
		"only IPv6":                            {dns: &fakeReverse{names: ptr, hosts: []string{"2001:db8::1"}}},
		"no records":                           {dns: &fakeReverse{names: ptr}},
		"nxdomain":                             {dns: &fakeReverse{names: ptr, hostErr: &net.DNSError{IsNotFound: true}}},
		"servfail":                             {dns: &fakeReverse{names: ptr, hostErr: &net.DNSError{Err: "server misbehaving", IsTemporary: true}}},
		"timeout":                              {dns: &fakeReverse{names: ptr, hostErr: timeout}},
		// A name the worker's operator owns can point at the address, but only
		// the address block's owner sets reverse DNS.
		"forward only, reverse names another host": {dns: &fakeReverse{names: []string{"mail.provider.example."}, hosts: []string{"45.0.0.10"}}},
		"forward only, no reverse":                 {dns: &fakeReverse{hosts: []string{"45.0.0.10"}}},
		"reverse lookup fails":                     {dns: &fakeReverse{addrErr: timeout, hosts: []string{"45.0.0.10"}}},
		"no resolver configured":                   {nilDNS: true},
	} {
		h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
		if !test.nilDNS {
			h.ReverseDNS = test.dns
		}
		w := send(h, "GET", path, "", "valid")
		if test.dns != nil && len(test.dns.lookedUp) > 0 && !test.dns.deadlined {
			t.Errorf("%s: DNS lookup without a deadline", name)
		}
		if test.dns != nil && slices.Contains(test.dns.lookedUp, "smtp.example.org") {
			t.Errorf("%s: relative name looked up: %v", name, test.dns.lookedUp)
		}
		var got struct {
			Connection SMTPConnection `json:"connection"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatal(name, w.Code, w.Body.String())
		}
		if test.shown && (got.Connection.Host == nil || *got.Connection.Host != "smtp.example.org" || got.Connection.HostStatus != SMTPHostOK || got.Connection.HostSource == nil) {
			t.Errorf("%s: host hidden: %s", name, w.Body.String())
		}
		if !test.shown && (got.Connection.Host != nil || got.Connection.HostSource != nil || got.Connection.HostStatus != SMTPHostDNSUnconfirmed || strings.Contains(w.Body.String(), "smtp.example.org") || strings.Contains(w.Body.String(), "45.0.0.10")) {
			t.Errorf("%s: host shown or wrong status: %s", name, w.Body.String())
		}
	}
	// A status that already says there is no host is left alone.
	store2 := &smtpNoHostStore{smtpTestStore: store}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store2, ReverseDNS: &fakeReverse{hostErr: timeout}}
	if w := send(h, "GET", path, "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"host_status":"not_reported"`) {
		t.Fatal("not_reported was overwritten", w.Body.String())
	}
}

type smtpNoHostStore struct{ *smtpTestStore }

func (smtpNoHostStore) SMTPConnection(context.Context, string) (SMTPConnection, error) {
	return SMTPConnection{HostStatus: SMTPHostNotReported, Ports: SMTPPorts(), ObservedPorts: []int{}}, nil
}
