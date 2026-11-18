package tracking

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"image/gif"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeVisitor struct {
	calls  int
	hash   [32]byte
	kind   string
	record bool
	err    error
}

func (v *fakeVisitor) Visit(_ context.Context, hash [32]byte, kind string, record bool) error {
	v.calls++
	v.hash, v.kind, v.record = hash, kind, record
	return v.err
}

func TestTrackingResponsesUseOnlyHashedCapabilities(t *testing.T) {
	const origin = "https://track.example.com"
	hash := sha256.Sum256([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15})
	keys := testLinkKeys(t)
	v := &fakeVisitor{}
	h := &Handler{Origin: origin, Links: keys, Visitor: v}
	request := func(method, link string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, link, nil))
		return w
	}
	open, _ := keys.link(origin, "open", linkToken, "")
	click, _ := keys.link(origin, "click", linkToken, "https://recipient.example/path?private=value")
	w := request(http.MethodGet, open)
	if w.Code != 200 || v.calls != 1 || v.hash != hash || v.kind != "open" || !v.record || w.Body.String() != string(pixel) || w.Header().Get("Content-Type") != "image/gif" {
		t.Fatal("open response or capability scope")
	}
	img, err := gif.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil || img.Bounds().Dx() != 1 || img.Bounds().Dy() != 1 {
		t.Fatal("invalid tracking pixel", err)
	}
	for _, header := range []struct{ name, value string }{{"Cache-Control", "no-store, max-age=0"}, {"Referrer-Policy", "no-referrer"}, {"X-Content-Type-Options", "nosniff"}} {
		if w.Header().Get(header.name) != header.value {
			t.Fatal("missing privacy header", header.name)
		}
	}
	w = request(http.MethodHead, open)
	if w.Code != 200 || v.record || w.Body.Len() != 0 {
		t.Fatal("HEAD recorded an open or returned a body")
	}
	w = request(http.MethodGet, click)
	if w.Code != 302 || v.kind != "click" || !v.record || w.Header().Get("Location") != "https://recipient.example/path?private=value" || w.Body.Len() != 0 {
		t.Fatal("click did not use the encrypted destination")
	}
	w = request(http.MethodHead, click)
	if w.Code != 302 || v.record || w.Body.Len() != 0 {
		t.Fatal("HEAD recorded a click or returned a body")
	}
}

func TestTrackingRejectsEverythingButV2AndDoesNotEchoSecrets(t *testing.T) {
	keys := testLinkKeys(t)
	v := &fakeVisitor{}
	h := &Handler{Origin: linkOrigin, Links: keys, Visitor: v}
	open, _ := keys.link(linkOrigin, "open", linkToken, "")
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"POST", strings.TrimPrefix(open, linkOrigin), 405},
		// The former token-only routes and malformed variants are all 404.
		{"GET", "/t/o/" + linkToken, 404},
		{"GET", "/t/c/" + linkToken, 404},
		{"HEAD", "/t/o/" + linkToken, 404},
		{"GET", "/t/o/" + strings.ToUpper(linkToken), 404},
		{"GET", "/t/o/%30" + linkToken[1:], 404},
		{"GET", "/t/x/v2/" + keys.active + "/AAAA", 404},
		{"GET", "/t/o", 404},
		{"GET", "/t/", 404},
		{"GET", "/", 404},
		{"GET", strings.TrimPrefix(open, linkOrigin) + "?redirect=https://evil.example", 404},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status || w.Body.Len() != 0 || w.Header().Get("Location") != "" {
			t.Fatal("invalid tracking request", tc.path, w.Code)
		}
	}
	if v.calls != 0 {
		t.Fatal("invalid input reached tracking store")
	}
	// A visitor failure never changes an authentic link's response.
	v.err = errors.New("private provider details")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", open, nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), "private") {
		t.Fatal("storage error changed the response", w.Code)
	}
	// No keys: nothing resolves.
	w = httptest.NewRecorder()
	(&Handler{Origin: linkOrigin}).ServeHTTP(w, httptest.NewRequest("GET", open, nil))
	if w.Code != 404 {
		t.Fatal("link resolved without keys", w.Code)
	}
}
