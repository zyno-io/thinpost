package controlapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDisabledTrackingPathsFailBeforeMuxNormalization(t *testing.T) {
	console := newTestConsole(t)
	router := Router(stubHandler(http.StatusNotFound, "operator"), nil, console, nil)
	const token = "00112233445566778899aabbccddeeff"
	for _, path := range []string{
		"/t", "/t/o/" + token, "/t/c/" + token,
		"/t//c/" + token, "/%74/c/" + token,
		"/x/../t/c/" + token, "/t/" + token + "/../../other",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(method, path, nil))
			if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" || strings.Contains(w.Body.String(), token) {
				t.Fatalf("disabled tracking path returned %d or exposed a capability", w.Code)
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("disabled tracking response is missing privacy headers")
			}
		}
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tracking", nil))
	if w.Code != http.StatusOK {
		t.Fatal("tracking prefix intercepted an unrelated console route")
	}
	w = httptest.NewRecorder()
	console.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/t/c/"+token, nil))
	if w.Code != http.StatusNotFound {
		t.Fatal("standalone console fell back on a tracking path")
	}
}
