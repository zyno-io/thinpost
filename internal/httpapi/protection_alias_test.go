package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

// X-Thinpost-Enhanced-Protection applies on every adapter and accepts exactly
// one "true" or "false" value. The removed X-Thinpost-Require-TLS name is
// rejected so it can never be silently ignored.
func TestEnhancedProtectionHeader(t *testing.T) {
	single := []struct{ name, path, body, authHeader, authValue string }{
		{"postmark", "/email", `{"From":"sender@example.com","To":"a@example.net","Subject":"Hello","TextBody":"body"}`, "X-Postmark-Server-Token", "valid-key"},
		{"resend", "/emails", `{"from":"sender@example.com","to":["a@example.net"],"subject":"Hello","text":"body"}`, "Authorization", "Bearer valid-key"},
	}
	cases := []struct {
		name    string
		headers map[string][]string
		want    bool
		status  int
	}{
		{"true", map[string][]string{"X-Thinpost-Enhanced-Protection": {"true"}}, true, http.StatusOK},
		{"false", map[string][]string{"X-Thinpost-Enhanced-Protection": {"false"}}, false, http.StatusOK},
		{"removed legacy header", map[string][]string{"X-Thinpost-Require-TLS": {"true"}}, false, http.StatusBadRequest},
		{"removed legacy header with canonical", map[string][]string{"X-Thinpost-Enhanced-Protection": {"true"}, "X-Thinpost-Require-TLS": {"true"}}, false, http.StatusBadRequest},
		{"neither", nil, false, http.StatusOK},
		{"duplicate values", map[string][]string{"X-Thinpost-Enhanced-Protection": {"true", "true"}}, false, http.StatusBadRequest},
		{"invalid value", map[string][]string{"X-Thinpost-Enhanced-Protection": {"yes"}}, false, http.StatusBadRequest},
	}
	for _, adapter := range single {
		for _, tc := range cases {
			t.Run(adapter.name+"/"+tc.name, func(t *testing.T) {
				f := &fakeService{app: submission.App{ID: "app-a"}}
				r := httptest.NewRequest(http.MethodPost, adapter.path, strings.NewReader(adapter.body))
				r.Header.Set(adapter.authHeader, adapter.authValue)
				for name, values := range tc.headers {
					for _, value := range values {
						r.Header.Add(name, value)
					}
				}
				w := httptest.NewRecorder()
				New(f).ServeHTTP(w, r)
				if w.Code != tc.status {
					t.Fatalf("status %d", w.Code)
				}
				if tc.status == http.StatusOK && (len(f.requests) != 1 || f.requests[0].Message.EnhancedProtection != tc.want) {
					t.Fatal("protection flag not applied")
				}
				if tc.status != http.StatusOK && len(f.requests) != 0 {
					t.Fatal("invalid flag reached acceptance")
				}
			})
		}
	}
	t.Run("mailgun", func(t *testing.T) {
		form := url.Values{"from": {"sender@example.com"}, "to": {"to@example.net"}, "subject": {"hello"}, "text": {"body"}}
		f := &fakeService{app: submission.App{ID: "app-a"}}
		r := httptest.NewRequest(http.MethodPost, "/v3/example.com/messages", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Thinpost-Enhanced-Protection", "true")
		r.SetBasicAuth("api", "valid-key")
		w := httptest.NewRecorder()
		New(f).ServeHTTP(w, r)
		if w.Code != http.StatusOK || len(f.requests) != 1 || !f.requests[0].Message.EnhancedProtection {
			t.Fatal("mailgun header not applied", w.Code)
		}
	})
	t.Run("resend batch", func(t *testing.T) {
		f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app-a", KeyID: "key-a"}}, receipt: submission.BatchReceipt{IDs: []string{"one", "two"}}}
		w := request(t, New(f), "/emails/batch", validResendBatch, map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Enhanced-Protection": "true"})
		if w.Code != http.StatusOK || len(f.batch.Messages) != 2 || !f.batch.Messages[0].EnhancedProtection || !f.batch.Messages[1].EnhancedProtection {
			t.Fatal("resend batch header not applied", w.Code)
		}
		w = request(t, New(f), "/emails/batch", validResendBatch, map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Enhanced-Protection": "sometimes"})
		if w.Code != http.StatusBadRequest {
			t.Fatal("invalid batch header accepted", w.Code)
		}
	})
	t.Run("sendgrid batch", func(t *testing.T) {
		f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app", KeyID: "key"}}, receipt: submission.BatchReceipt{IDs: []string{"one", "two"}}}
		w := request(t, New(f), "/v3/mail/send", gridPersonalizations, map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Enhanced-Protection": "true"})
		if w.Code != http.StatusAccepted || len(f.batch.Messages) != 2 || !f.batch.Messages[0].EnhancedProtection || !f.batch.Messages[1].EnhancedProtection {
			t.Fatal("sendgrid batch header not applied", w.Code)
		}
	})
	t.Run("postmark batch", func(t *testing.T) {
		f := &fakeService{app: submission.App{ID: "app-a"}}
		body := `[{"From":"sender@example.com","To":"a@example.net","Subject":"A","TextBody":"body"}]`
		w := request(t, New(f), "/email/batch", body, map[string]string{"X-Postmark-Server-Token": "valid-key", "X-Thinpost-Enhanced-Protection": "true"})
		if w.Code != http.StatusOK || len(f.requests) != 1 || !f.requests[0].Message.EnhancedProtection {
			t.Fatal("postmark batch header not applied", w.Code)
		}
	})
}
