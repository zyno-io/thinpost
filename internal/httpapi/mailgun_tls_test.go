package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

func TestMailgunTLSOptions(t *testing.T) {
	tests := []struct {
		name, native, skip, extension string
		wantTLS                       bool
		wantStatus                    int
	}{
		{"default", "", "", "", false, http.StatusOK},
		{"native yes", "yes", "", "", true, http.StatusOK},
		{"native true", "true", "", "", true, http.StatusOK},
		{"native false", "false", "", "", false, http.StatusOK},
		{"extension requires TLS", "no", "no", "true", true, http.StatusOK},
		{"native cannot be weakened", "yes", "false", "false", true, http.StatusOK},
		{"invalid native option", "maybe", "", "", false, http.StatusBadRequest},
		{"skip verification yes", "yes", "yes", "", false, http.StatusBadRequest},
		{"skip verification true", "", "true", "", false, http.StatusBadRequest},
		{"invalid verification option", "", "other", "", false, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"from": {"sender@example.com"}, "to": {"to@example.net"}, "subject": {"hello"}, "text": {"body"}}
			if tc.native != "" {
				form.Set("o:require-tls", tc.native)
			}
			if tc.skip != "" {
				form.Set("o:skip-verification", tc.skip)
			}
			f := &fakeService{app: submission.App{ID: "app-a"}}
			r := httptest.NewRequest(http.MethodPost, "/v3/example.com/messages", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.extension != "" {
				r.Header.Set("X-Thinpost-Enhanced-Protection", tc.extension)
			}
			r.SetBasicAuth("api", "valid-key")
			w := httptest.NewRecorder()
			New(f).ServeHTTP(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("unexpected status %d", w.Code)
			}
			if tc.wantStatus != http.StatusOK {
				if len(f.requests) != 0 {
					t.Fatal("invalid TLS option reached acceptance")
				}
				return
			}
			if len(f.requests) != 1 || f.requests[0].Message.EnhancedProtection != tc.wantTLS {
				t.Fatal("TLS policy not preserved")
			}
		})
	}
}

func TestMailgunRejectsDuplicateTLSOptions(t *testing.T) {
	for _, key := range []string{"o:require-tls", "o:skip-verification"} {
		form := url.Values{"from": {"sender@example.com"}, "to": {"to@example.net"}, "subject": {"hello"}, "text": {"body"}, key: {"no", "yes"}}
		f := &fakeService{app: submission.App{ID: "app-a"}}
		r := httptest.NewRequest(http.MethodPost, "/v3/example.com/messages", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth("api", "valid-key")
		w := httptest.NewRecorder()
		New(f).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || len(f.requests) != 0 {
			t.Fatal("duplicate TLS option reached acceptance")
		}
	}
}
