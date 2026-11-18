package admin

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeAuth struct{ admin bool }

func (a fakeAuth) Authenticate(_ context.Context, token string) (Principal, error) {
	if token != "valid" {
		return Principal{}, errors.New("invalid")
	}
	return Principal{Subject: "operator-1", Administrator: a.admin}, nil
}

type fakeStore struct {
	calls           int
	actor, app, key string
	domainActive    bool
	query           MessageQuery
	searchErr       error
	page            MessagePage
	subject         string
	subjectErr      error
}

func (s *fakeStore) SearchMessages(_ context.Context, actor, app string, query MessageQuery) (MessagePage, error) {
	s.calls++
	s.actor, s.app, s.query = actor, app, query
	return s.page, s.searchErr
}
func (s *fakeStore) RevealSubject(_ context.Context, actor, app, message string) (string, error) {
	s.calls++
	s.actor, s.app, s.key = actor, app, message
	return s.subject, s.subjectErr
}

func (s *fakeStore) ListDomains(context.Context, string) ([]Domain, error) {
	s.calls++
	return []Domain{}, nil
}
func (s *fakeStore) RequestDomain(_ context.Context, actor, app, domain string) (DomainChallenge, error) {
	s.calls++
	s.actor, s.app = actor, app
	return DomainChallenge{Domain: Domain{Name: domain}, DNSName: "_thinpost." + domain, DNSValue: "thinpost-verification=test"}, nil
}
func (s *fakeStore) DomainChallenge(_ context.Context, app, domain string) (Domain, [32]byte, error) {
	s.calls++
	return Domain{Name: domain, Active: s.domainActive}, sha256.Sum256([]byte("thinpost-verification=test")), nil
}
func (s *fakeStore) VerifyDomain(_ context.Context, actor, app, domain string, digest [32]byte) (Domain, error) {
	s.calls++
	s.domainActive = true
	return Domain{Name: domain, Active: true}, nil
}

type fakeDNS struct{ records []string }

func (d fakeDNS) LookupTXT(context.Context, string) ([]string, error) { return d.records, nil }

func (s *fakeStore) ListMessages(_ context.Context, actor, app, cursor string, limit int) (MessagePage, error) {
	s.calls++
	s.actor, s.app = actor, app
	return MessagePage{Messages: []MessageSummary{}}, nil
}
func (s *fakeStore) GetMessage(_ context.Context, actor, app, message string) (MessageDetail, error) {
	s.calls++
	s.actor, s.app = actor, app
	return MessageDetail{Message: MessageSummary{ID: message}}, nil
}
func (s *fakeStore) RetryPreparation(_ context.Context, actor, app, message string) error {
	s.calls++
	s.actor, s.app, s.key = actor, app, message
	return nil
}

func (s *fakeStore) ListApps(context.Context) ([]App, error) {
	s.calls++
	return []App{{ID: strings.Repeat("a", 32), Name: "App", Active: true}}, nil
}
func (s *fakeStore) CreateApp(_ context.Context, actor, name string) (App, error) {
	s.calls++
	s.actor = actor
	return App{ID: strings.Repeat("a", 32), Name: name, Active: true, CreatedAt: time.Now()}, nil
}
func (s *fakeStore) ListKeys(_ context.Context, app string) ([]Key, error) {
	s.calls++
	s.app = app
	return []Key{{ID: strings.Repeat("b", 32), Name: "Key", Active: true}}, nil
}
func (s *fakeStore) IssueKeyForOperator(_ context.Context, actor, app, name string) (IssuedKey, error) {
	s.calls++
	s.actor, s.app = actor, app
	return IssuedKey{Key: Key{ID: strings.Repeat("b", 32), Name: name, Active: true}, Secret: "tp_one_time"}, nil
}
func (s *fakeStore) RevokeKeyForOperator(_ context.Context, actor, app, key string) error {
	s.calls++
	s.actor, s.app, s.key = actor, app, key
	return nil
}
func send(h *Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestRequiresAuthenticatedAdministrator(t *testing.T) {
	store := &fakeStore{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
	if got := send(h, "GET", "/api/v1/apps", "", ""); got.Code != 401 {
		t.Fatal("accepted missing credential")
	}
	if got := send(h, "GET", "/api/v1/apps", "", "bad"); got.Code != 401 {
		t.Fatal("accepted invalid credential")
	}
	h.Authenticator = fakeAuth{admin: false}
	if got := send(h, "GET", "/api/v1/apps", "", "valid"); got.Code != 403 {
		t.Fatal("accepted non-admin")
	}
	if store.calls != 0 {
		t.Fatal("store accessed without authorization")
	}
	if got := send(&Handler{}, "GET", "/api/v1/apps", "", "valid"); got.Code != 503 {
		t.Fatal("missing auth did not fail closed")
	}
}
func TestAppAndKeyOperations(t *testing.T) {
	store := &fakeStore{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
	app := strings.Repeat("a", 32)
	key := strings.Repeat("b", 32)
	w := send(h, "POST", "/api/v1/apps", `{"name":"Payments"}`, "valid")
	if w.Code != 201 || store.actor != "operator-1" {
		t.Fatal("app creation failed")
	}
	w = send(h, "POST", "/api/v1/apps/"+app+"/keys", `{"name":"Production"}`, "valid")
	if w.Code != 201 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "tp_one_time") || store.app != app {
		t.Fatal("key issuance failed")
	}
	w = send(h, "GET", "/api/v1/apps/"+app+"/keys", "", "valid")
	if w.Code != 200 || strings.Contains(w.Body.String(), "tp_one_time") {
		t.Fatal("key list leaked a secret")
	}
	w = send(h, "DELETE", "/api/v1/apps/"+app+"/keys/"+key, "", "valid")
	if w.Code != 204 || store.key != key || store.actor != "operator-1" {
		t.Fatal("key revocation failed")
	}
	w = send(h, "GET", "/api/v1/apps/"+app+"/messages?limit=25", "", "valid")
	if w.Code != 200 || store.actor != "operator-1" || store.app != app {
		t.Fatal("message history failed")
	}
	w = send(h, "GET", "/api/v1/apps/"+app+"/messages/"+key, "", "valid")
	if w.Code != 200 || store.actor != "operator-1" {
		t.Fatal("message detail failed")
	}
	w = send(h, "POST", "/api/v1/apps/"+app+"/messages/"+key+"/retry-preparation", "", "valid")
	if w.Code != 204 || store.actor != "operator-1" || store.app != app || store.key != key {
		t.Fatal("audited preparation retry was not routed")
	}
	beforeRetry := store.calls
	w = send(h, "POST", "/api/v1/apps/"+app+"/messages/"+key+"/retry-preparation?unexpected=1", "", "valid")
	if w.Code != 400 || store.calls != beforeRetry {
		t.Fatal("unexpected retry query accepted")
	}
	w = send(h, "POST", "/api/v1/apps/"+app+"/messages/"+key+"/retry-preparation", "", "")
	if w.Code != 401 || store.calls != beforeRetry {
		t.Fatal("unauthenticated preparation retry reached the store")
	}
	beforeInvalid := store.calls
	w = send(h, "GET", "/api/v1/apps/"+app+"/messages?limit=101", "", "valid")
	if w.Code != 400 || store.calls != beforeInvalid {
		t.Fatal("invalid page size accepted")
	}
	before := store.calls
	w = send(h, "POST", "/api/v1/apps", `{"name":"App","unsupported":true}`, "valid")
	if w.Code != 400 || store.calls != before {
		t.Fatal("unsupported input was accepted")
	}
}

func TestPublicOIDCConfigurationFailsClosed(t *testing.T) {
	h := &Handler{}
	if got := send(h, "GET", "/api/v1/config", "", ""); got.Code != 503 {
		t.Fatal("missing OIDC configuration accepted")
	}
	h.AuthConfig = &PublicAuthConfig{Issuer: "https://issuer.example/tenant", ClientID: "public-client", Scopes: []string{"openid", "api://thinpost/admin"}}
	got := send(h, "GET", "/api/v1/config", "", "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), "public-client") || got.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("public OIDC config unavailable")
	}
	h.AuthConfig.Issuer = "http://issuer.example"
	if got = send(h, "GET", "/api/v1/config", "", ""); got.Code != 503 {
		t.Fatal("accepted insecure issuer")
	}
}

type fakeBodyReader struct {
	calls      int
	attachment AttachmentContent
	headers    []HeaderField
	err        error
	index      int
}

func (f *fakeBodyReader) ReadAttachment(_ context.Context, _, _, _ string, index int) (AttachmentContent, error) {
	f.calls++
	f.index = index
	return f.attachment, f.err
}
func (f *fakeBodyReader) ReadDeliveryHeaders(context.Context, string, string, string, string) ([]HeaderField, error) {
	f.calls++
	return f.headers, f.err
}

func (f *fakeBodyReader) ReadMessageBody(context.Context, string, string, string) (MessageBody, error) {
	f.calls++
	return MessageBody{Text: "private", ReplyTo: []string{"help@example.com"}}, nil
}
func TestBodyRequiresAdminAndIsNotCached(t *testing.T) {
	body := &fakeBodyReader{}
	store := &fakeStore{}
	app := strings.Repeat("a", 32)
	msg := strings.Repeat("b", 32)
	path := "/api/v1/apps/" + app + "/messages/" + msg + "/body"
	h := &Handler{Authenticator: fakeAuth{admin: false}, Store: store, ContentReader: body}
	if got := send(h, "GET", path, "", "valid"); got.Code != 403 || body.calls != 0 {
		t.Fatal("unauthorized body read")
	}
	h.Authenticator = fakeAuth{admin: true}
	got := send(h, "GET", path, "", "valid")
	if got.Code != 200 || got.Header().Get("Cache-Control") != "no-store" || body.calls != 1 || !strings.Contains(got.Body.String(), `"reply_to":["help@example.com"]`) {
		t.Fatal("body read failed")
	}
	h.ContentReader = nil
	if got = send(h, "GET", path, "", "valid"); got.Code != 503 {
		t.Fatal("missing object store did not fail closed")
	}
}

func TestDomainVerificationRequiresMatchingTXT(t *testing.T) {
	store := &fakeStore{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, DNS: fakeDNS{records: []string{"unrelated"}}}
	app := strings.Repeat("a", 32)
	w := send(h, "POST", "/api/v1/apps/"+app+"/domains", `{"domain":"Example.COM"}`, "valid")
	if w.Code != 201 || !strings.Contains(w.Body.String(), "_thinpost.example.com") {
		t.Fatal("domain challenge failed")
	}
	path := "/api/v1/apps/" + app + "/domains/example.com/verify"
	w = send(h, "POST", path, "", "valid")
	if w.Code != 409 || store.domainActive {
		t.Fatal("verified without matching TXT")
	}
	h.DNS = fakeDNS{records: []string{"unrelated", "thinpost-verification=test"}}
	w = send(h, "POST", path, "", "valid")
	if w.Code != 200 || !store.domainActive {
		t.Fatal("matching TXT did not verify")
	}
	if _, ok := NormalizeDomain("127.0.0.1"); ok {
		t.Fatal("accepted IP as domain")
	}
	if _, ok := NormalizeDomain("example.com."); ok {
		t.Fatal("accepted malformed domain")
	}
}

func TestAPIResponsesCarrySecurityHeaders(t *testing.T) {
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, AuthConfig: &PublicAuthConfig{Issuer: "https://issuer.example", ClientID: "c", Scopes: []string{"openid"}}}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"config":       send(h, "GET", "/api/v1/config", "", ""),
		"unauthorized": send(h, "GET", "/api/v1/apps", "", ""),
		"ok":           send(h, "GET", "/api/v1/apps", "", "valid"),
		"unavailable":  send(&Handler{}, "GET", "/api/v1/apps", "", "valid"),
	} {
		for header, want := range map[string]string{
			"X-Content-Type-Options":  "nosniff",
			"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'; sandbox",
			"Referrer-Policy":         "no-referrer",
			"Cache-Control":           "no-store",
		} {
			if got := w.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", name, header, got, want)
			}
		}
	}
}

func TestMessageRoutesAcceptEveryProviderIDForm(t *testing.T) {
	app := strings.Repeat("a", 32)
	const id = "0123456789abcdef0123456789abcdef"
	for _, form := range []string{id, "01234567-89ab-cdef-0123-456789abcdef", id + "@example.com", "%3C" + id + "@example.com%3E"} {
		store := &fakeStore{subject: "s"}
		h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
		w := send(h, "GET", "/api/v1/apps/"+app+"/messages/"+form+"/subject", "", "valid")
		if w.Code != 200 || store.key != id {
			t.Errorf("%q: status %d, key %q", form, w.Code, store.key)
		}
	}
	for _, bad := range []string{"0123456789abcdef", "01234567-89ab-cdef-0123", "not-an-id"} {
		store := &fakeStore{}
		h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
		if w := send(h, "GET", "/api/v1/apps/"+app+"/messages/"+bad+"/subject", "", "valid"); w.Code != 404 || store.calls != 0 {
			t.Errorf("%q accepted", bad)
		}
	}
}
