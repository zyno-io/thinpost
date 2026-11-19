package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

const (
	holdPostmark = `{"From":"sender@example.com","To":"one@example.net","Subject":"Hello","TextBody":"body"}`
	holdResend   = `{"from":"sender@example.com","to":"one@example.net","subject":"Hello","text":"body"}`
	holdGrid     = `{"from":{"email":"sender@example.com"},"personalizations":[{"to":[{"email":"one@example.net"}]}],"subject":"Hello","content":[{"type":"text/plain","value":"body"}]}`
)

func holdMailgun(t *testing.T, s *Server, hold string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v3/example.com/messages", strings.NewReader("from=sender%40example.com&to=one%40example.net&subject=Hello&text=body"))
	r.SetBasicAuth("api", "valid-key")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if hold != "" {
		r.Header.Set("X-Thinpost-Hold", hold)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestHoldHeaderOnSingleMessageRoutes(t *testing.T) {
	routes := []struct{ name, path, body, auth, value string }{
		{"postmark", "/email", holdPostmark, "X-Postmark-Server-Token", "valid-key"},
		{"resend", "/emails", holdResend, "Authorization", "Bearer valid-key"},
		{"sendgrid", "/v3/mail/send", holdGrid, "Authorization", "Bearer valid-key"},
		{"mailgun", "", "", "", ""},
	}
	for _, route := range routes {
		for _, value := range []string{"", "true", "TRUE", "false"} {
			f := &fakeService{app: submission.App{ID: "app"}}
			var w *httptest.ResponseRecorder
			if route.name == "mailgun" {
				w = holdMailgun(t, New(f), value)
			} else {
				headers := map[string]string{route.auth: route.value}
				if value != "" {
					headers["X-Thinpost-Hold"] = value
				}
				w = request(t, New(f), route.path, route.body, headers)
			}
			if w.Code != http.StatusOK && w.Code != http.StatusAccepted || len(f.requests) != 1 {
				t.Fatalf("%s %q: status %d: %s", route.name, value, w.Code, w.Body.String())
			}
			want := strings.EqualFold(value, "true")
			if f.requests[0].Hold != want {
				t.Fatalf("%s %q: hold %v", route.name, value, f.requests[0].Hold)
			}
			// The confirmation is present exactly when the hold was honored.
			if got := w.Header().Get("X-Thinpost-Hold"); want && got != "held" || !want && got != "" {
				t.Fatalf("%s %q: confirmation %q", route.name, value, got)
			}
		}
	}
}

func TestHoldHeaderRejectsInvalidValuesWithoutSubmitting(t *testing.T) {
	for _, values := range [][]string{{"yes"}, {"1"}, {""}, {"true", "true"}, {"true", "false"}} {
		f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}}
		for _, path := range []string{"/emails", "/emails/batch"} {
			body := holdResend
			if path == "/emails/batch" {
				body = "[" + holdResend + "]"
			}
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer valid-key")
			r.Header["X-Thinpost-Hold"] = values
			w := httptest.NewRecorder()
			New(f).ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || len(f.requests) != 0 || f.batchCalls != 0 || w.Header().Get("X-Thinpost-Hold") != "" {
				t.Fatalf("%s %q: status %d", path, values, w.Code)
			}
		}
	}
}

func TestHoldHeaderAppliesToEveryBatchMessage(t *testing.T) {
	f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"one", "two"}}}
	w := request(t, New(f), "/emails/batch", "["+holdResend+","+holdResend+"]", map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Hold": "true"})
	if w.Code != http.StatusOK || !f.batch.Hold || w.Header().Get("X-Thinpost-Hold") != "held" {
		t.Fatal("resend batch", w.Code, f.batch.Hold)
	}
	f = &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"one", "two"}}}
	w = request(t, New(f), "/v3/mail/send", gridPersonalizations, map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Hold": "true"})
	if w.Code != http.StatusAccepted || !f.batch.Hold || w.Header().Get("X-Thinpost-Hold") != "held" {
		t.Fatal("sendgrid personalizations", w.Code, f.batch.Hold)
	}
	// A Postmark batch commits each message separately; each one is held.
	g := &fakeService{app: submission.App{ID: "app"}}
	w = request(t, New(g), "/email/batch", "["+holdPostmark+","+holdPostmark+"]", map[string]string{"X-Postmark-Server-Token": "valid-key", "X-Thinpost-Hold": "true"})
	if w.Code != http.StatusOK || len(g.requests) != 2 || !g.requests[0].Hold || !g.requests[1].Hold || w.Header().Get("X-Thinpost-Hold") != "held" {
		t.Fatal("postmark batch", w.Code, len(g.requests))
	}
}

func TestHoldConfirmationAbsentOnFailure(t *testing.T) {
	f := &fakeService{app: submission.App{ID: "app"}, acceptErr: submission.ErrBacklogExceeded}
	w := request(t, New(f), "/emails", holdResend, map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Hold": "true"})
	if w.Code != http.StatusTooManyRequests || w.Header().Get("X-Thinpost-Hold") != "" || len(f.requests) != 1 || !f.requests[0].Hold {
		t.Fatal("a refused held submission must not confirm a hold", w.Code)
	}
}
