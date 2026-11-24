package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
	pendingKey      bool
	switched        bool
	cancelled       string
	query           MessageQuery
	searchErr       error
	page            MessagePage
	subject         string
	subjectErr      error
	identity        EdgeIdentity
	identityErr     error
	removal         DomainRemoval
	removeErr       error
	removed         string
}

// EdgeIdentity is what the sending worker reported; the zero value means it has
// not reported yet.
func (s *fakeStore) EdgeIdentity(context.Context) (EdgeIdentity, error) {
	return s.identity, s.identityErr
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
	return DomainChallenge{Domain: Domain{Name: domain}, DNSName: "thinpost1._domainkey." + domain, DNSValue: "v=DKIM1; k=rsa; p=" + testKeyB64}, nil
}
func (s *fakeStore) CreateDKIMKey(_ context.Context, actor, app, domain string) (DomainChallenge, error) {
	s.calls++
	s.actor, s.app = actor, app
	if s.pendingKey {
		return DomainChallenge{}, ErrConflict
	}
	s.pendingKey = true
	return DomainChallenge{Domain: Domain{Name: domain}, DNSName: "thinpost2._domainkey." + domain, DNSValue: "v=DKIM1; k=rsa; p=" + testKeyB64}, nil
}
func (s *fakeStore) PendingDKIMKey(_ context.Context, app, domain, selector string) (DKIMKeyRef, error) {
	s.calls++
	if selector != "" && selector != "thinpost1" {
		return DKIMKeyRef{}, ErrNotFound
	}
	return DKIMKeyRef{Selector: "thinpost1", PublicKey: testKey}, nil
}
func (s *fakeStore) ActivateDKIMKey(_ context.Context, actor, app, domain string, key DKIMKeyRef, switchActive bool) (Domain, error) {
	s.calls++
	s.domainActive = true
	s.switched = switchActive
	return Domain{Name: domain, Active: true}, nil
}
func (s *fakeStore) CancelDKIMKey(_ context.Context, actor, app, domain, selector string) error {
	s.calls++
	s.cancelled = selector
	return nil
}

func (s *fakeStore) DeleteDomain(_ context.Context, actor, app, domain string) (DomainRemoval, error) {
	s.calls++
	s.actor, s.app, s.removed = actor, app, domain
	return s.removal, s.removeErr
}

// testKey stands in for a public key; the handler only passes it to the checker.
var testKey = bytes.Repeat([]byte{0x30, 0x82, 0x01, 0x22}, 70)
var testKeyB64 = base64.StdEncoding.EncodeToString(testKey)

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
	raw        []byte
}

func (f *fakeBodyReader) OpenMessageRaw(context.Context, string, string, string) ([]byte, error) {
	f.calls++
	return f.raw, f.err
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

func TestRawDownloadIsAnOpaqueAttachment(t *testing.T) {
	body := &fakeBodyReader{raw: []byte("From: a@example.com\r\n\r\nhi\r\n")}
	app, msg := strings.Repeat("a", 32), strings.Repeat("b", 32)
	path := "/api/v1/apps/" + app + "/messages/" + msg + "/raw"
	h := &Handler{Authenticator: fakeAuth{admin: false}, Store: &fakeStore{}, ContentReader: body}
	if got := send(h, "GET", path, "", "valid"); got.Code != 403 || body.calls != 0 {
		t.Fatal("unauthorized raw read")
	}
	h.Authenticator = fakeAuth{admin: true}
	got := send(h, "GET", path, "", "valid")
	if got.Code != 200 || got.Body.String() != string(body.raw) || got.Header().Get("Content-Type") != "application/octet-stream" ||
		got.Header().Get("X-Content-Type-Options") != "nosniff" || got.Header().Get("Cache-Control") != "no-store" ||
		got.Header().Get("Content-Disposition") != `attachment; filename="message-`+msg+`.eml"` {
		t.Fatalf("raw download: %d %v", got.Code, got.Header())
	}
	if got = send(h, "POST", path, "", "valid"); got.Code != 405 {
		t.Fatal("raw accepted a POST", got.Code)
	}
	if got = send(h, "GET", path+"?x=1", "", "valid"); got.Code != 400 {
		t.Fatal("raw accepted a query string", got.Code)
	}
	body.err = ErrNotFound
	if got = send(h, "GET", path, "", "valid"); got.Code != 404 {
		t.Fatal("v1/v2 message must be 404", got.Code)
	}
	h.ContentReader = nil
	if got = send(h, "GET", path, "", "valid"); got.Code != 503 {
		t.Fatal("missing object store did not fail closed")
	}
}

func TestDomainVerificationRequiresMatchingDKIMRecord(t *testing.T) {
	store := &fakeStore{}
	value := "v=DKIM1; k=rsa; p=" + testKeyB64
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, DNS: fakeDNS{records: []string{"unrelated"}}}
	app := strings.Repeat("a", 32)
	w := send(h, "POST", "/api/v1/apps/"+app+"/domains", `{"domain":"Example.COM"}`, "valid")
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"dns_name":"thinpost1._domainkey.example.com"`) || !strings.Contains(w.Body.String(), "v=DKIM1; k=rsa; p=") {
		t.Fatal("domain challenge failed", w.Body.String())
	}
	path := "/api/v1/apps/" + app + "/domains/example.com/verify"
	for name, tc := range map[string]struct {
		records []string
		reason  string
	}{
		"unrelated record": {[]string{"unrelated"}, "unsupported"},
		"no record":        {nil, "not_found"},
		"two records":      {[]string{value, value}, "multiple_records"},
		"different key":    {[]string{"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString([]byte("another key"))}, "different_key"},
		"testing mode":     {[]string{value + "; t=y"}, "testing_mode"},
	} {
		h.DNS = fakeDNS{records: tc.records}
		w = send(h, "POST", path, "", "valid")
		if w.Code != 409 || store.domainActive || !strings.Contains(w.Body.String(), `"reason":"`+tc.reason+`"`) {
			t.Fatal(name, "verified or wrong reason", w.Code, w.Body.String())
		}
	}
	h.DNS = fakeDNS{records: []string{value}}
	if w = send(h, "POST", path, "", "valid"); w.Code != 200 || !store.domainActive || store.switched {
		t.Fatal("matching record did not verify", w.Code)
	}
	if _, ok := NormalizeDomain("127.0.0.1"); ok {
		t.Fatal("accepted IP as domain")
	}
	if _, ok := NormalizeDomain("example.com."); ok {
		t.Fatal("accepted malformed domain")
	}
}

func TestDKIMRotationRoutes(t *testing.T) {
	store := &fakeStore{}
	value := "v=DKIM1; k=rsa; p=" + testKeyB64
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, DNS: fakeDNS{}}
	base := "/api/v1/apps/" + strings.Repeat("a", 32) + "/domains/example.com/dkim-keys"
	if w := send(h, "POST", base, "", "valid"); w.Code != 201 || !strings.Contains(w.Body.String(), "thinpost2._domainkey.example.com") {
		t.Fatal("create", w.Code)
	}
	if w := send(h, "POST", base, "", "valid"); w.Code != 409 {
		t.Fatal("second pending key allowed", w.Code)
	}
	// Switching rechecks DNS first.
	if w := send(h, "POST", base+"/thinpost1/activate", "", "valid"); w.Code != 409 || store.switched {
		t.Fatal("switched without the record", w.Code)
	}
	h.DNS = fakeDNS{records: []string{value}}
	if w := send(h, "POST", base+"/thinpost1/activate", "", "valid"); w.Code != 200 || !store.switched {
		t.Fatal("switch failed", w.Code)
	}
	if w := send(h, "POST", base+"/other/activate", "", "valid"); w.Code != 404 {
		t.Fatal("unknown selector", w.Code)
	}
	if w := send(h, "POST", base+"/Bad_Selector/activate", "", "valid"); w.Code != 400 {
		t.Fatal("bad selector accepted", w.Code)
	}
	if w := send(h, "DELETE", base+"/thinpost1", "", "valid"); w.Code != 204 || store.cancelled != "thinpost1" {
		t.Fatal("cancel", w.Code)
	}
	if w := send(h, "GET", base, "", "valid"); w.Code != 405 {
		t.Fatal("GET allowed", w.Code)
	}
	if w := send(h, "POST", base, "", ""); w.Code != 401 {
		t.Fatal("unauthenticated", w.Code)
	}
	h2 := &Handler{Authenticator: fakeAuth{admin: false}, Store: store, DNS: fakeDNS{}}
	if w := send(h2, "POST", base, "", "valid"); w.Code != 403 {
		t.Fatal("non-admin", w.Code)
	}
}

func TestDeleteDomainRoute(t *testing.T) {
	app := strings.Repeat("a", 32)
	path := "/api/v1/apps/" + app + "/domains/example.com"
	until := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	bounce := "bounce.example.com"
	removal := DomainRemoval{
		Name:             "example.com",
		RemovedAt:        until.Add(-7 * 24 * time.Hour),
		DKIMRecords:      []RemovedDKIMRecord{{Selector: "thinpost1", DNSName: "thinpost1._domainkey.example.com", KeepUntil: &until}, {Selector: "thinpost2", DNSName: "thinpost2._domainkey.example.com"}},
		BounceDomain:     &bounce,
		KeepRecordsUntil: &until,
	}
	store := &fakeStore{removal: removal}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
	w := send(h, "DELETE", path, "", "valid")
	if w.Code != 200 || store.removed != "example.com" || store.app != app || store.actor != "operator-1" {
		t.Fatal("delete", w.Code, store.removed, store.app, store.actor)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"name", "removed_at", "dkim_records", "bounce_domain", "keep_records_until"} {
		if _, ok := body[field]; !ok {
			t.Error("missing field", field, w.Body.String())
		}
	}
	if _, ok := body["tracking_domain"]; ok {
		t.Error("absent tracking domain was serialized")
	}
	records := body["dkim_records"].([]any)
	if len(records) != 2 || records[0].(map[string]any)["selector"] != "thinpost1" || records[0].(map[string]any)["keep_until"] == nil || records[1].(map[string]any)["keep_until"] != nil {
		t.Error("dkim_records", w.Body.String())
	}
	// The name is normalized before the store sees it.
	store.removed = ""
	if w = send(h, "DELETE", "/api/v1/apps/"+app+"/domains/Example.COM", "", "valid"); w.Code != 200 || store.removed != "example.com" {
		t.Fatal("not normalized", w.Code, store.removed)
	}
	calls := store.calls
	for name, w := range map[string]*httptest.ResponseRecorder{
		"invalid domain": send(h, "DELETE", "/api/v1/apps/"+app+"/domains/not_a_domain", "", "valid"),
		"ip":             send(h, "DELETE", "/api/v1/apps/"+app+"/domains/127.0.0.1", "", "valid"),
		"query":          send(h, "DELETE", path+"?force=1", "", "valid"),
		"body":           send(h, "DELETE", path, `{"x":1}`, "valid"),
	} {
		if w.Code != 400 {
			t.Error(name, w.Code)
		}
	}
	for _, m := range []string{"GET", "POST", "PUT"} {
		if w := send(h, m, path, "", "valid"); w.Code != 405 || w.Header().Get("Allow") != "DELETE" {
			t.Error(m, w.Code, w.Header().Get("Allow"))
		}
	}
	if w := send(h, "DELETE", "/api/v1/apps/"+app+"/domains/", "", "valid"); w.Code != 404 {
		t.Error("trailing slash", w.Code)
	}
	if store.calls != calls {
		t.Error("store called for a request that should not reach it")
	}
	store.removeErr = ErrNotFound
	if w := send(h, "DELETE", path, "", "valid"); w.Code != 404 {
		t.Error("not found", w.Code)
	}
	store.removeErr = ErrDomainInSMTPPolicy
	if w := send(h, "DELETE", path, "", "valid"); w.Code != 409 || strings.TrimSpace(w.Body.String()) != `{"error":"Conflict","reason":"smtp_from_policy"}` {
		t.Error("policy conflict", w.Code, w.Body.String())
	}
	store.removeErr = errors.New("database down")
	if w := send(h, "DELETE", path, "", "valid"); w.Code != 503 {
		t.Error("store error", w.Code)
	}
	// A deleted app is unknown before the store is asked.
	store = &fakeStore{removal: removal}
	h = &Handler{Authenticator: fakeAuth{admin: true}, Store: store, Apps: &fakeApps{missing: map[string]bool{app: true}}}
	if w := send(h, "DELETE", path, "", "valid"); w.Code != 404 || store.calls != 0 {
		t.Error("deleted app", w.Code, store.calls)
	}
	if w := send(h, "DELETE", path, "", ""); w.Code != 401 {
		t.Error("unauthenticated", w.Code)
	}
	h = &Handler{Authenticator: fakeAuth{admin: false}, Store: store}
	if w := send(h, "DELETE", path, "", "valid"); w.Code != 403 || store.calls != 0 {
		t.Error("non-admin", w.Code)
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
