package tracking

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	unsubOrigin   = "https://track.example.com"
	unsubDelivery = "00112233445566778899aabbccddeeff"
)

type fakeUnsubscribes struct {
	calls []string
	err   error
}

func (f *fakeUnsubscribes) RecordUnsubscribe(_ context.Context, id string) error {
	f.calls = append(f.calls, id)
	return f.err
}

func unsubLink(t *testing.T, keys *LinkKeys) string {
	t.Helper()
	link, err := keys.UnsubscribeLink(unsubOrigin, unsubDelivery)
	if err != nil {
		t.Fatal(err)
	}
	return link
}

func unsubRequest(h *Handler, method, link, contentType, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, link, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const formType = "application/x-www-form-urlencoded"

func TestUnsubscribeLinkShapeAndVerification(t *testing.T) {
	keys := testLinkKeys(t)
	link := unsubLink(t, keys)
	if !strings.HasPrefix(link, unsubOrigin+"/t/u/v3/"+keys.active+"/") || strings.Count(link, "/") != 7 {
		t.Fatal("unexpected link shape")
	}
	id, err := keys.verifyUnsubscribe(strings.TrimPrefix(link, unsubOrigin))
	if err != nil || id != unsubDelivery {
		t.Fatal("link did not verify", err)
	}
	for _, bad := range []string{"", "00", strings.ToUpper(unsubDelivery), unsubDelivery + "0"} {
		if _, err := keys.UnsubscribeLink(unsubOrigin, bad); err == nil {
			t.Fatal("signed a malformed delivery ID")
		}
	}
	// A link is not bound to its origin: it verifies at any host (the path is all that matters).
	other, err := keys.UnsubscribeLink("https://other.example.com", unsubDelivery)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := keys.verifyUnsubscribe(strings.TrimPrefix(other, "https://other.example.com")); err != nil || id != unsubDelivery {
		t.Fatal("an unsubscribe link minted for another origin did not verify", id, err)
	}
}

func TestValidateUnsubscribeOrigin(t *testing.T) {
	for _, origin := range []string{"https://track.example.com", "https://track.example.com:8443", "https://xn--trck-loa.example.com"} {
		if err := ValidateUnsubscribeOrigin(origin); err != nil {
			t.Fatal(origin, err)
		}
	}
	long, _ := NormalizeOrigin("https://" + strings.Repeat("a", MaxOriginBytes-len("https://")))
	for _, origin := range []string{"https://träck.example.com", long + "ü", ""} {
		if err := ValidateUnsubscribeOrigin(origin); err == nil {
			t.Fatal("accepted", origin)
		}
	}
}

func TestNormalizeOriginRequiresPlainASCIIHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://track.example.com":        "https://track.example.com",
		"https://Track.Example.COM/":       "https://Track.Example.COM",
		"https://track.example.com:8443":   "https://track.example.com:8443",
		"https://xn--trck-loa.example.com": "https://xn--trck-loa.example.com",
		"https://127.0.0.1":                "https://127.0.0.1",
	} {
		if got, err := NormalizeOrigin(in); err != nil || got != want {
			t.Fatalf("%s: got %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{
		"https://a&b.example.com", "https://a'b.example.com", "https://a<b.example.com", `https://a"b.example.com`,
		"https://träck.example.com", "https://[::1]", "https://[::1]:8443", "https://track.example.com:", "https://track.example.com:80a",
		"https://track.example.com:123456", "https://-track.example.com", "https://a_b.example.com", "https://a%26b.example.com",
	} {
		if got, err := NormalizeOrigin(bad); err == nil {
			t.Fatalf("accepted %q as %q", bad, got)
		}
	}
}

func TestSignaturesDoNotCrossKinds(t *testing.T) {
	keys := testLinkKeys(t)
	unsubscribe := strings.TrimPrefix(unsubLink(t, keys), unsubOrigin)
	// An open signature for the same 32-hex ID replayed under /t/u.
	open, err := keys.link(unsubOrigin, "open", unsubDelivery, "")
	if err != nil {
		t.Fatal(err)
	}
	openPath := strings.TrimPrefix(open, unsubOrigin)
	replayed := strings.Replace(openPath, "/t/o/", "/t/u/", 1)
	if _, err := keys.verifyUnsubscribe(replayed); err == nil {
		t.Fatal("open signature verified as unsubscribe")
	}
	// An unsubscribe signature replayed under /t/o and /t/c.
	for _, letter := range []string{"o", "c"} {
		path := strings.Replace(unsubscribe, "/t/u/", "/t/"+letter+"/", 1)
		if _, _, _, err := keys.verify(path); err == nil {
			t.Fatal("unsubscribe signature verified as /t/" + letter)
		}
	}
	// Through the handler: neither crosses over, and storage is never touched.
	v, u := &fakeVisitor{}, &fakeUnsubscribes{}
	h := &Handler{Links: keys, Visitor: v, Unsubscribes: u}
	for _, path := range []string{replayed, strings.Replace(unsubscribe, "/t/u/", "/t/o/", 1)} {
		w := unsubRequest(h, http.MethodPost, path, formType, "List-Unsubscribe=One-Click")
		if (w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed) || v.calls != 0 || len(u.calls) != 0 {
			t.Fatal("cross-kind replay reached a handler", w.Code)
		}
	}
}

func TestUnsubscribeGetAndHeadNeverChangeState(t *testing.T) {
	keys := testLinkKeys(t)
	link := unsubLink(t, keys)
	u := &fakeUnsubscribes{}
	h := &Handler{Links: keys, Unsubscribes: u}
	get := unsubRequest(h, http.MethodGet, link, "", "")
	if get.Code != 200 || len(u.calls) != 0 {
		t.Fatal("GET changed state or failed", get.Code)
	}
	body := get.Body.String()
	if !strings.Contains(body, `<form method="post">`) || !strings.Contains(body, `name="List-Unsubscribe" value="One-Click"`) || strings.Count(body, "<button") != 1 {
		t.Fatal("GET page lacks the single POST form")
	}
	// No address, app, subject, delivery ID or link in the page.
	for _, leak := range []string{unsubDelivery, "@", "example.com", "/t/u/"} {
		if strings.Contains(body, leak) {
			t.Fatal("page leaks", leak)
		}
	}
	head := unsubRequest(h, http.MethodHead, link, "", "")
	if head.Code != 200 || head.Body.Len() != 0 || len(u.calls) != 0 {
		t.Fatal("HEAD returned a body or changed state", head.Code)
	}
	for _, w := range []*httptest.ResponseRecorder{get, head} {
		assertUnsubscribeHeaders(t, w)
	}
	if put := unsubRequest(h, http.MethodPut, link, "", ""); put.Code != http.StatusMethodNotAllowed || put.Header().Get("Allow") != "GET, HEAD, POST" || len(u.calls) != 0 {
		t.Fatal("PUT was not refused", put.Code)
	}
}

func assertUnsubscribeHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	want := map[string]string{
		"Cache-Control":           "no-store, max-age=0",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'",
	}
	for name, value := range want {
		if w.Header().Get(name) != value {
			t.Fatalf("header %s = %q", name, w.Header().Get(name))
		}
	}
}

func TestUnsubscribePostRecordsAfterSignatureOnly(t *testing.T) {
	keys := testLinkKeys(t)
	link := unsubLink(t, keys)
	u := &fakeUnsubscribes{}
	h := &Handler{Links: keys, Unsubscribes: u}
	w := unsubRequest(h, http.MethodPost, link, formType, "List-Unsubscribe=One-Click")
	if w.Code != 200 || len(u.calls) != 1 || u.calls[0] != unsubDelivery || !strings.Contains(w.Body.String(), "You're unsubscribed.") {
		t.Fatal("POST did not record", w.Code, u.calls)
	}
	assertUnsubscribeHeaders(t, w)
	// A duplicate is the same 200: idempotency is the store's contract.
	if w = unsubRequest(h, http.MethodPost, link, formType, "List-Unsubscribe=One-Click"); w.Code != 200 || len(u.calls) != 2 {
		t.Fatal("duplicate POST", w.Code)
	}
	// Multipart bodies are accepted too.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("List-Unsubscribe", "One-Click")
	_ = mw.Close()
	if w = unsubRequest(h, http.MethodPost, link, mw.FormDataContentType(), buf.String()); w.Code != 200 || len(u.calls) != 3 {
		t.Fatal("multipart POST", w.Code)
	}
	// Tampering is a 404 before storage, for every part of the path.
	parts := strings.Split(strings.TrimPrefix(link, unsubOrigin), "/")
	for i := 3; i < len(parts); i++ {
		tampered := append([]string(nil), parts...)
		tampered[i] = strings.Replace(tampered[i], tampered[i][:1], map[bool]string{true: "0", false: "1"}[tampered[i][:1] != "0"], 1)
		before := len(u.calls)
		if w = unsubRequest(h, http.MethodPost, strings.Join(tampered, "/"), formType, "List-Unsubscribe=One-Click"); w.Code != 404 || len(u.calls) != before {
			t.Fatal("tampered path reached storage", i, w.Code)
		}
		assertUnsubscribeHeaders(t, w)
	}
	for _, path := range []string{"/t/u/", "/t/u/x", link + "?a=b", link + "/extra", strings.Replace(link, "/v3/", "/V3/", 1)} {
		before := len(u.calls)
		if w = unsubRequest(h, http.MethodPost, path, formType, "List-Unsubscribe=One-Click"); w.Code != 404 || len(u.calls) != before {
			t.Fatal("malformed path reached storage", path, w.Code)
		}
	}
}

func TestUnsubscribePostBodyVariants(t *testing.T) {
	keys := testLinkKeys(t)
	link := unsubLink(t, keys)
	u := &fakeUnsubscribes{}
	h := &Handler{Links: keys, Unsubscribes: u}
	var extra bytes.Buffer
	mw := multipart.NewWriter(&extra)
	_ = mw.WriteField("List-Unsubscribe", "One-Click")
	_ = mw.WriteField("other", "x")
	_ = mw.Close()
	for _, tc := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"empty", formType, "", 400},
		{"wrong value", formType, "List-Unsubscribe=Other", 400},
		{"wrong field", formType, "List-Unsub=One-Click", 400},
		{"extra field", formType, "List-Unsubscribe=One-Click&a=b", 400},
		{"repeated field", formType, "List-Unsubscribe=One-Click&List-Unsubscribe=One-Click", 400},
		{"bad escape", formType, "List-Unsubscribe=%zz", 400},
		{"no content type", "", "List-Unsubscribe=One-Click", 415},
		{"json", "application/json", `{"List-Unsubscribe":"One-Click"}`, 415},
		{"oversize", formType, "List-Unsubscribe=One-Click&pad=" + strings.Repeat("x", 2000), 413},
		{"multipart extra", mw.FormDataContentType(), extra.String(), 400},
		{"multipart no boundary", "multipart/form-data", "x", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := unsubRequest(h, http.MethodPost, link, tc.contentType, tc.body)
			if w.Code != tc.status || len(u.calls) != 0 {
				t.Fatal("status", w.Code, "calls", len(u.calls))
			}
			assertUnsubscribeHeaders(t, w)
		})
	}
}

func TestUnsubscribePostStorageOutcomes(t *testing.T) {
	keys := testLinkKeys(t)
	link := unsubLink(t, keys)
	for _, tc := range []struct {
		name   string
		err    error
		status int
		text   string
	}{
		{"missing delivery or recipient", ErrNotFound, 404, "This link is no longer valid."},
		{"gate closed or database down", errors.New("runtime closed"), 503, "Your request was not recorded."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{Links: keys, Unsubscribes: &fakeUnsubscribes{err: tc.err}}
			w := unsubRequest(h, http.MethodPost, link, formType, "List-Unsubscribe=One-Click")
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.text) {
				t.Fatal("status", w.Code)
			}
			assertUnsubscribeHeaders(t, w)
			for _, leak := range []string{unsubDelivery, tc.err.Error()} {
				if strings.Contains(w.Body.String(), leak) {
					t.Fatal("response leaks", leak)
				}
			}
		})
	}
	// No recorder configured: a verified POST is a 503, GET and HEAD still work.
	h := &Handler{Links: keys}
	if w := unsubRequest(h, http.MethodPost, link, formType, "List-Unsubscribe=One-Click"); w.Code != 503 {
		t.Fatal("POST without a recorder", w.Code)
	}
	if w := unsubRequest(h, http.MethodGet, link, "", ""); w.Code != 200 {
		t.Fatal("GET without a recorder", w.Code)
	}
	// No keys at all: every path is a 404.
	if w := unsubRequest(&Handler{}, http.MethodGet, link, "", ""); w.Code != 404 {
		t.Fatal("GET without keys", w.Code)
	}
}

func TestUnsubscribePostStaysUnsupportedForOpenAndClick(t *testing.T) {
	// POST stays unsupported for open and click links.
	v := &fakeVisitor{}
	h := &Handler{Visitor: v, Unsubscribes: &fakeUnsubscribes{}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/t/o/v3/"+strings.Repeat("0", 32)+"/AAAA", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" || v.calls != 0 {
		t.Fatal("POST to an open link", w.Code)
	}
}
