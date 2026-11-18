package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/submission"
)

type fakeService struct {
	app       submission.App
	authErr   error
	acceptErr error
	requests  []submission.Request
	apps      []submission.App
}

func (f *fakeService) Authenticate(_ context.Context, key string) (submission.App, error) {
	if key != "valid-key" {
		return submission.App{}, submission.ErrInvalidKey
	}
	return f.app, f.authErr
}
func (f *fakeService) Accept(_ context.Context, app submission.App, request submission.Request) (submission.Receipt, error) {
	f.apps = append(f.apps, app)
	f.requests = append(f.requests, request)
	if f.acceptErr != nil {
		return submission.Receipt{}, f.acceptErr
	}
	return submission.Receipt{ID: "message-1", AcceptedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}, nil
}

func request(t *testing.T, s *Server, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestProviderSubmissions(t *testing.T) {
	tests := []struct{ name, path, body, authHeader, authValue string }{
		{"postmark", "/email", `{"From":"Sender <sender@example.com>","To":"First <first@example.net>, second@example.net","Subject":"Hello","TextBody":"body"}`, "X-Postmark-Server-Token", "valid-key"},
		{"resend", "/emails", `{"from":"Sender <sender@example.com>","to":["First <first@example.net>","second@example.net"],"subject":"Hello","text":"body"}`, "Authorization", "Bearer valid-key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeService{app: submission.App{ID: "app-a"}}
			headers := map[string]string{tc.authHeader: tc.authValue, "X-Thinpost-Enhanced-Protection": "true"}
			if tc.name == "resend" {
				headers["Idempotency-Key"] = "key-1"
			}
			w := request(t, New(f), tc.path, tc.body, headers)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if len(f.requests) != 1 || f.apps[0].ID != "app-a" {
				t.Fatalf("incorrect application or count: %#v", f)
			}
			got := f.requests[0]
			if !got.Message.EnhancedProtection || got.Message.Subject != "Hello" || len(got.Message.To) != 2 || got.Adapter != tc.name {
				t.Fatalf("incorrect submission: %#v", got)
			}
			if tc.name == "resend" && got.IdempotencyKey != "key-1" {
				t.Fatal("idempotency key lost")
			}
			var response map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if tc.name == "resend" && response["id"] != "message-1" {
				t.Fatalf("bad response: %#v", response)
			}
			if tc.name == "postmark" && response["MessageID"] != "message-1" {
				t.Fatalf("bad response: %#v", response)
			}
		})
	}
}

func TestRejectsWithoutAccepting(t *testing.T) {
	valid := `{"from":"sender@example.com","to":"to@example.net","subject":"hello","text":"body"}`
	tests := []struct {
		name, body, key, tls string
		expected             int
	}{
		{"bad credential", valid, "bad", "", 401},
		{"unknown option", `{"from":"sender@example.com","to":"to@example.net","subject":"hello","text":"body","attachments":[{}]}`, "valid-key", "", 400},
		{"header injection", `{"from":"sender@example.com","to":"to@example.net","subject":"hello\r\nBcc: victim@example.com","text":"body"}`, "valid-key", "", 400},
		{"control character in subject", `{"from":"sender@example.com","to":"to@example.net","subject":"hello\u0000","text":"body"}`, "valid-key", "", 400},
		{"missing body", `{"from":"sender@example.com","to":"to@example.net","subject":"hello"}`, "valid-key", "", 400},
		{"bad tls", valid, "valid-key", "yes", 400},
		{"trailing JSON", valid + `{}`, "valid-key", "", 400},
		{"oversized", valid + strings.Repeat(" ", SingleBodyLimit(submission.DefaultMaxAttachmentBytes)), "valid-key", "", 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeService{app: submission.App{ID: "app-a"}}
			h := map[string]string{"Authorization": "Bearer " + tc.key}
			if tc.tls != "" {
				h["X-Thinpost-Enhanced-Protection"] = tc.tls
			}
			w := request(t, New(f), "/emails", tc.body, h)
			if w.Code != tc.expected || len(f.requests) != 0 {
				t.Fatalf("status %d, requests %d: %s", w.Code, len(f.requests), w.Body.String())
			}
		})
	}
}

func TestDurabilityErrorsNeverReturnSuccess(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{submission.ErrInvalidKey, 401}, {submission.ErrUnavailable, 503}, {errors.New("db connection failed"), 503}, {submission.ErrIdempotencyConflict, 409}, {submission.ErrInvalidMessage, 400}, {submission.ErrQuotaExceeded, 429}, {submission.ErrRateLimited, 429}, {submission.ErrBacklogExceeded, 429}} {
		f := &fakeService{app: submission.App{ID: "app-a"}, acceptErr: tc.err}
		w := request(t, New(f), "/emails", `{"from":"a@example.com","to":"b@example.com","subject":"x","text":"y"}`, map[string]string{"Authorization": "Bearer valid-key"})
		if w.Code != tc.status {
			t.Fatalf("%v: got status %d", tc.err, w.Code)
		}
		if strings.Contains(w.Body.String(), "db connection failed") {
			t.Fatal("internal error leaked")
		}
	}
}
func TestUnavailableWithoutService(t *testing.T) {
	w := request(t, New(nil), "/email", `{}`, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", w.Code)
	}
}

func TestSendGridBasicSubmission(t *testing.T) {
	f := &fakeService{app: submission.App{ID: "grid-app"}}
	body := `{"from":{"email":"sender@example.com","name":"Sender"},"personalizations":[{"to":[{"email":"to@example.net"}]}],"subject":"hello","content":[{"type":"text/plain","value":"body"}]}`
	w := request(t, New(f), "/v3/mail/send", body, map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Enhanced-Protection": "true"})
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(f.requests) != 1 || f.requests[0].Adapter != "sendgrid" || !f.requests[0].Message.EnhancedProtection || f.apps[0].ID != "grid-app" {
		t.Fatalf("bad request: %#v", f.requests)
	}
}

func TestMailgunMultipartAndDomain(t *testing.T) {
	f := &fakeService{app: submission.App{ID: "mailgun-app"}}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{"from": "sender@example.com", "to": "one@example.net, two@example.net", "subject": "hello", "text": "body"} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v3/Example.COM/messages", &body)
	r.SetBasicAuth("api", "valid-key")
	r.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	New(f).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(f.requests) != 1 || f.requests[0].SendingDomain != "example.com" || len(f.requests[0].Message.To) != 2 {
		t.Fatalf("bad request: %#v", f.requests)
	}
}

func TestUnsupportedSendGridAndMailgunAreRejected(t *testing.T) {
	f := &fakeService{app: submission.App{ID: "app"}}
	body := `{"from":{"email":"a@example.com"},"personalizations":[{"to":[{"email":"b@example.com"}],"custom_args":{"x":"unsupported"}}],"subject":"hello","content":[{"type":"text/plain","value":"body"}]}`
	w := request(t, New(f), "/v3/mail/send", body, map[string]string{"Authorization": "Bearer valid-key"})
	if w.Code != 400 {
		t.Fatalf("sendgrid: status %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/v3/example.com/messages", strings.NewReader("from=a%40example.com&to=b%40example.com&subject=x&text=y&o%3Atestmode=yes"))
	r.SetBasicAuth("api", "valid-key")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	New(f).ServeHTTP(w, r)
	if w.Code != 400 || len(f.requests) != 0 {
		t.Fatalf("mailgun status %d, requests %d", w.Code, len(f.requests))
	}
}
