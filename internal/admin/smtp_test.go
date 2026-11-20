package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type smtpTestStore struct {
	*fakeStore
	policy      SMTPFromPolicy
	credentials []SMTPCredential
	primary     netip.Addr
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
func (s *smtpTestStore) SMTPConnection(_ context.Context, _ string, primary netip.Addr) (SMTPConnection, error) {
	s.primary = primary
	host, source := "smtp.example.org", SMTPHostPrimary
	return SMTPConnection{Host: &host, HostSource: &source, Ports: SMTPPorts(), ObservedPorts: []int{}}, nil
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
	primary := netip.MustParseAddr("45.0.1.22")
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, PrimarySource: primary}
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
		got.Credentials == nil || len(got.Credentials) != 0 || *got.Connection.Host != "smtp.example.org" || len(got.Connection.Ports) != 2 || store.primary != primary {
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
		`{"from":{"mode":"off","keep_origin":false}}`,
		`{"from":{"mode":"rewrite","keep_origin":false,"only_unauthorized":true}}`,
		`{"from":{"mode":"domain","keep_origin":false,"only_unauthorized":true}}`,
		`{"from":{"mode":"domain","domain":"Example.com","keep_origin":false,"only_unauthorized":true}}`,
		`{"from":{"mode":"address","address":"\"a b\"@example.com","keep_origin":false,"only_unauthorized":true}}`,
		`{"from":{"mode":"address","address":"a..b@example.com","keep_origin":false,"only_unauthorized":true}}`,
		`{"from":{"mode":"address","address":"é@example.com","keep_origin":false,"only_unauthorized":true}}`,
		`{"from":{"mode":"address","address":"a@example.com","keep_origin":true,"only_unauthorized":true}}`,
		`{"from":{"mode":"domain","domain":"example.com","address":"\"a b\"@example.com","keep_origin":false,"only_unauthorized":true}}`,
		`{"from":{"mode":"domain","domain":"example.com","address":"a..b@example.com","keep_origin":false,"only_unauthorized":true}}`,
		`{"from":{"mode":"domain","domain":"example.com","address":"é@example.com","keep_origin":false,"only_unauthorized":true}}`,
		`{"from":{"mode":"domain","domain":"example.com","address":"a@Example.com","keep_origin":false,"only_unauthorized":true}}`,
		`{"from":{"mode":"domain","domain":"example.com","address":"no-domain","keep_origin":false,"only_unauthorized":true}}`,
		`{"from":{"mode":"off","domain":"example.com","keep_origin":false,"only_unauthorized":true}}`,
		`{"from":{"mode":"off","keep_origin":false,"only_unauthorized":true},"extra":1}`,
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
	w = send(h, "PUT", put, `{"from":{"mode":"domain","domain":"example.com","keep_origin":true,"only_unauthorized":false}}`, "valid")
	if w.Code != 200 || store.sets != 1 || store.actor != "operator-1" || store.policy.Mode != "domain" || *store.policy.Domain != "example.com" ||
		!strings.Contains(w.Body.String(), `"signing_ready":[{"domain":"example.com","ready":true}]`) || len(ready.calls) != 1 || ready.calls[0] != app+" example.com" {
		t.Fatal("domain update", w.Code, w.Body.String(), ready.calls)
	}
	// Domain mode with a fallback address: stored, and both domains are checked for signing.
	ready.calls = nil
	w = send(h, "PUT", put, `{"from":{"mode":"domain","domain":"example.com","address":"no-reply@example.org","keep_origin":false,"only_unauthorized":true}}`, "valid")
	if w.Code != 200 || store.policy.Address == nil || *store.policy.Address != "no-reply@example.org" || *store.policy.Domain != "example.com" ||
		!strings.Contains(w.Body.String(), `"signing_ready":[{"domain":"example.com","ready":true},{"domain":"example.org","ready":true}]`) ||
		len(ready.calls) != 2 {
		t.Fatal("domain update with fallback", w.Code, w.Body.String(), ready.calls)
	}
	ready.calls = nil
	w = send(h, "PUT", put, `{"from":{"mode":"domain","domain":"example.com","address":"no-reply@example.com","keep_origin":false,"only_unauthorized":true}}`, "valid")
	if w.Code != 200 || len(ready.calls) != 1 || strings.Count(w.Body.String(), `"domain":"example.com","ready"`) != 1 {
		t.Fatal("same-domain fallback is checked once", w.Code, w.Body.String(), ready.calls)
	}
	store.policy = SMTPFromPolicy{}
	ready.err = errors.New("not ready")
	if w = send(h, "PUT", put, `{"from":{"mode":"address","address":"No-Reply+x@example.com","keep_origin":false,"only_unauthorized":true}}`, "valid"); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"signing_ready":[{"domain":"example.com","ready":false}]`) {
		t.Fatal("address update", w.Code, w.Body.String())
	}
	if w = send(h, "PUT", put, `{"from":{"mode":"off","keep_origin":false,"only_unauthorized":true}}`, "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"signing_ready":[]`) {
		t.Fatal("off update", w.Code, w.Body.String())
	}
	store.setErr = ErrConflict
	if w = send(h, "PUT", put, `{"from":{"mode":"domain","domain":"example.net","keep_origin":false,"only_unauthorized":true}}`, "valid"); w.Code != 409 {
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
	// Domain mode keeps an optional, validated fallback address.
	domain := "example.com"
	if p, ok := NormalizeSMTPFromPolicy(SMTPFromPolicy{Mode: "domain", Domain: &domain, Address: str("no-reply@example.org"), KeepOrigin: true}); !ok || p.Address == nil || *p.Address != "no-reply@example.org" || !p.KeepOrigin {
		t.Fatal("domain with fallback", p, ok)
	}
	if p, ok := NormalizeSMTPFromPolicy(SMTPFromPolicy{Mode: "domain", Domain: &domain}); !ok || p.Address != nil {
		t.Fatal("domain without fallback", p, ok)
	}
	if _, ok := NormalizeSMTPFromPolicy(SMTPFromPolicy{Mode: "domain", Domain: &domain, Address: str("a b@example.com")}); ok {
		t.Fatal("bad fallback accepted")
	}
	for _, address := range []string{strings.Repeat("a", 65) + "@example.com", "a@localhost", "a@127.0.0.1", ".a@example.com", "a.@example.com", "a b@example.com", "a@" + "-example.com"} {
		if _, ok := NormalizeSMTPFromPolicy(SMTPFromPolicy{Mode: "address", Address: &address}); ok {
			t.Fatal("address accepted", address)
		}
	}
}
