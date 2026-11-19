package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/submission"
)

// nativeFake adds replay and operator authorization to fakeService.
type nativeFake struct {
	*fakeService
	replayed     bool
	authorizeErr error
	authorized   []string
}

func (f *nativeFake) Accept(ctx context.Context, app submission.App, req submission.Request) (submission.Receipt, error) {
	receipt, err := f.fakeService.Accept(ctx, app, req)
	receipt.Replayed = f.replayed
	return receipt, err
}

func (f *nativeFake) AuthorizeOperator(_ context.Context, appID, actor string) (submission.App, error) {
	f.authorized = append(f.authorized, appID+"|"+actor)
	if f.authorizeErr != nil {
		return submission.App{}, f.authorizeErr
	}
	return submission.App{ID: appID, Operator: actor}, nil
}

func newNativeFake() *nativeFake {
	return &nativeFake{fakeService: &fakeService{app: submission.App{ID: "app-a", KeyID: "key-1"}}}
}

const nativeValid = `{"from":"Sender <sender@example.com>","to":["first@example.net","Second <second@example.net>"],"subject":"Hello","text":"body"}`

var nativeAuth = map[string]string{"Authorization": "Bearer valid-key"}

func nativeRequest(t *testing.T, s *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, s, "/app/v1/messages", body, headers)
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body) != 1 {
		t.Fatalf("not an error body: %q", w.Body.String())
	}
	return body["error"]
}

func TestNativeSubmissionShape(t *testing.T) {
	f := newNativeFake()
	w := nativeRequest(t, New(f), `{"from":"Sender <sender@example.com>","to":["first@example.net"],"cc":["cc@example.net"],"bcc":["bcc@example.net"],"reply_to":["Reply <reply@example.com>"],"subject":"Hello","text":"body","html":"<p>body</p>","headers":{"In-Reply-To":"<id@example.com>","List-Unsubscribe":"<https://example.com/u>","List-Unsubscribe-Post":"List-Unsubscribe=One-Click"},"attachments":[{"filename":"a.txt","content":"aGk=","content_type":"text/plain"}],"enhanced_protection":true,"hold":true}`, mergeHeaders(nativeAuth, map[string]string{"Idempotency-Key": "key-1"}))
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(f.requests) != 1 {
		t.Fatal("not accepted once")
	}
	got := f.requests[0]
	m := got.Message
	if got.Adapter != submission.AdapterNative || got.IdempotencyKey != "key-1" || !got.Hold || f.apps[0].KeyID != "key-1" || f.apps[0].Operator != "" {
		t.Fatalf("incorrect request: %#v", got)
	}
	if m.From != "Sender <sender@example.com>" || len(m.To) != 1 || len(m.Cc) != 1 || len(m.Bcc) != 1 || len(m.ReplyTo) != 1 || m.Subject != "Hello" || m.Text != "body" || m.HTML != "<p>body</p>" || !m.EnhancedProtection || m.InReplyTo == "" || m.ListUnsubscribe == "" || !m.ListUnsubscribePost || len(m.Attachments) != 1 || string(m.Attachments[0].Content) != "hi" {
		t.Fatalf("incorrect message: %#v", m)
	}
	if w.Header().Get("Location") != "/app/v1/messages/message-1" || w.Header().Get("X-Thinpost-Hold") != "held" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("incorrect headers: %v", w.Header())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["id"] != "message-1" || body["accepted_at"] != "2026-01-02T03:04:05Z" || len(body) != 2 {
		t.Fatalf("incorrect body: %s", w.Body.String())
	}
}

func TestNativeReplayIs200WithSameBody(t *testing.T) {
	f := newNativeFake()
	f.replayed = true
	w := nativeRequest(t, New(f), nativeValid, mergeHeaders(nativeAuth, map[string]string{"Idempotency-Key": "key-1"}))
	if w.Code != http.StatusOK || w.Header().Get("Location") != "/app/v1/messages/message-1" || !strings.Contains(w.Body.String(), `"id":"message-1"`) {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Thinpost-Hold") != "" {
		t.Fatal("replay of a non-hold request claims hold")
	}
}

func TestNativeTrackingBodyField(t *testing.T) {
	f := newNativeFake()
	body := `{"from":"a@example.com","to":["b@example.net"],"subject":"s","html":"<p>x</p>","tracking":{"opens":true,"clicks":false}}`
	if w := nativeRequest(t, New(f), body, nativeAuth); w.Code != 400 || len(f.requests) != 0 {
		t.Fatalf("tracking accepted while disabled: %d", w.Code)
	}
	if w := nativeRequest(t, NewWithOptions(f, Options{Tracking: true}), body, nativeAuth); w.Code != 201 {
		t.Fatalf("tracking refused while enabled: %d %s", w.Code, w.Body.String())
	}
	m := f.requests[0].Message
	if m.Tracking == nil || !m.Tracking.Opens || m.TrackingOff == nil || !m.TrackingOff.Clicks {
		t.Fatalf("tracking choice lost: %#v %#v", m.Tracking, m.TrackingOff)
	}
}

func TestNativeRejectsWithoutAccepting(t *testing.T) {
	tests := map[string]string{
		"duplicate field":          `{"from":"a@example.com","from":"b@example.com","to":["c@example.net"],"subject":"s","text":"t"}`,
		"case-folded duplicate":    `{"from":"a@example.com","From":"b@example.com","to":["c@example.net"],"subject":"s","text":"t"}`,
		"unknown field":            `{"from":"a@example.com","to":["c@example.net"],"subject":"s","text":"t","extra":1}`,
		"string recipient":         `{"from":"a@example.com","to":"c@example.net","subject":"s","text":"t"}`,
		"null tracking":            `{"from":"a@example.com","to":["c@example.net"],"subject":"s","html":"h","tracking":null}`,
		"null tracking member":     `{"from":"a@example.com","to":["c@example.net"],"subject":"s","html":"h","tracking":{"opens":null}}`,
		"unknown tracking member":  `{"from":"a@example.com","to":["c@example.net"],"subject":"s","html":"h","tracking":{"links":true}}`,
		"null headers":             `{"from":"a@example.com","to":["c@example.net"],"subject":"s","text":"t","headers":null}`,
		"reply-to header":          `{"from":"a@example.com","to":["c@example.net"],"subject":"s","text":"t","headers":{"Reply-To":"r@example.com"}}`,
		"cc header":                `{"from":"a@example.com","to":["c@example.net"],"subject":"s","text":"t","headers":{"Cc":"x@example.net"}}`,
		"attachments not a list":   `{"from":"a@example.com","to":["c@example.net"],"subject":"s","text":"t","attachments":{}}`,
		"bad base64":               `{"from":"a@example.com","to":["c@example.net"],"subject":"s","text":"t","attachments":[{"filename":"a","content":"!!"}]}`,
		"no recipients":            `{"from":"a@example.com","to":[],"subject":"s","text":"t"}`,
		"no content":               `{"from":"a@example.com","to":["c@example.net"],"subject":"s"}`,
		"header injection":         `{"from":"a@example.com","to":["c@example.net"],"subject":"s\r\nBcc: v@example.com","text":"t"}`,
		"upper-case key":           `{"FROM":"a@example.com","to":["c@example.net"],"subject":"s","text":"t"}`,
		"capitalized key":          `{"from":"a@example.com","to":["c@example.net"],"Subject":"s","text":"t"}`,
		"null hold":                `{"from":"a@example.com","to":["c@example.net"],"subject":"s","text":"t","hold":null}`,
		"null enhanced protection": `{"from":"a@example.com","to":["c@example.net"],"subject":"s","text":"t","enhanced_protection":null}`,
		"null cc":                  `{"from":"a@example.com","to":["c@example.net"],"subject":"s","text":"t","cc":null}`,
		"null to":                  `{"from":"a@example.com","to":null,"subject":"s","text":"t"}`,
		"null html":                `{"from":"a@example.com","to":["c@example.net"],"subject":"s","text":"t","html":null}`,
		"null attachments":         `{"from":"a@example.com","to":["c@example.net"],"subject":"s","text":"t","attachments":null}`,
		"null body":                `null`,
		"trailing JSON":            nativeValid + `{}`,
		"array body":               `[]`,
		"empty body":               ``,
		"wrong type":               `{"from":"a@example.com","to":["c@example.net"],"subject":5,"text":"t"}`,
		"opens without html":       `{"from":"a@example.com","to":["c@example.net"],"subject":"s","text":"t","tracking":{"opens":true}}`,
		"oversized":                nativeValid + strings.Repeat(" ", SingleBodyLimit(submission.DefaultMaxAttachmentBytes)),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			f := newNativeFake()
			w := nativeRequest(t, NewWithOptions(f, Options{Tracking: true}), body, nativeAuth)
			if w.Code != http.StatusBadRequest || len(f.requests) != 0 || errorCode(t, w) != "Bad Request" {
				t.Fatalf("status %d requests %d: %s", w.Code, len(f.requests), w.Body.String())
			}
		})
	}
}

func TestNativeRejectsExtensionHeaders(t *testing.T) {
	for _, header := range []string{"X-Thinpost-Hold", "X-Thinpost-Enhanced-Protection", "X-Thinpost-Require-TLS", "X-Thinpost-Track-Opens", "X-Thinpost-Track-Clicks"} {
		t.Run(header, func(t *testing.T) {
			f := newNativeFake()
			w := nativeRequest(t, New(f), nativeValid, mergeHeaders(nativeAuth, map[string]string{header: "true"}))
			if w.Code != http.StatusBadRequest || len(f.requests) != 0 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestNativeRouteRequestShape(t *testing.T) {
	f := newNativeFake()
	s := New(f)
	if w := request(t, s, "/app/v1/messages?x=1", nativeValid, nativeAuth); w.Code != 400 {
		t.Fatalf("query accepted: %d", w.Code)
	}
	for _, path := range []string{"/%61pp/v1/messages", "/app/v1/%6dessages", "/app/v1%2Fmessages"} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(nativeValid))
		r.URL.Path = strings.NewReplacer("%61", "a", "%6d", "m", "%2F", "/").Replace(path)
		r.URL.RawPath = path
		r.Header.Set("Authorization", "Bearer valid-key")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 404 || len(f.requests) != 0 {
			t.Fatalf("%s: encoded path served %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/app/v1/messages", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "POST" || errorCode(t, w) != "Method Not Allowed" {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	for name, headers := range map[string]map[string]string{"missing": nil, "wrong": {"Authorization": "Bearer nope"}} {
		if w := nativeRequest(t, s, nativeValid, headers); w.Code != 401 || errorCode(t, w) != "Unauthorized" {
			t.Fatalf("%s credential: %d", name, w.Code)
		}
	}
	if w := nativeRequest(t, s, nativeValid, mergeHeaders(nativeAuth, map[string]string{"Idempotency-Key": strings.Repeat("k", 257)})); w.Code != 400 {
		t.Fatalf("long idempotency key: %d", w.Code)
	}
	if len(f.requests) != 0 {
		t.Fatal("a rejected request was accepted")
	}
	if w := nativeRequest(t, New(nil), nativeValid, nativeAuth); w.Code != 503 {
		t.Fatalf("no service: %d", w.Code)
	}
}

func TestNativeErrorsUseTheSharedTable(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{submission.ErrInvalidRequest, 400, "Bad Request"},
		{submission.ErrInvalidKey, 401, "Unauthorized"},
		{admin.ErrNotFound, 404, "Not Found"},
		{submission.ErrAppPaused, 409, "app_paused"},
		{submission.ErrIdempotencyConflict, 409, "idempotency_conflict"},
		{submission.ErrInvalidMessage, 422, "message_rejected"},
		{submission.ErrQuotaExceeded, 429, "recipient_quota_exceeded"},
		{submission.ErrRateLimited, 429, "submission_rate_exceeded"},
		{submission.ErrBacklogExceeded, 429, "pending_limit_exceeded"},
		{submission.ErrUnavailable, 503, "Service Unavailable"},
		{errors.New("database down"), 503, "Service Unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.code, func(t *testing.T) {
			f := newNativeFake()
			f.acceptErr = tc.err
			w := nativeRequest(t, New(f), nativeValid, nativeAuth)
			if w.Code != tc.status || errorCode(t, w) != tc.code {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "database down") {
				t.Fatal("internal error leaked")
			}
			status, code := admin.SubmissionFailure(tc.err)
			if status != tc.status || code != tc.code {
				t.Fatalf("table: %d %s", status, code)
			}
		})
	}
}

func TestAuthenticatedOperatorPrincipalIsRefused(t *testing.T) {
	for _, tc := range []struct{ path, header, value, body string }{
		{"/app/v1/messages", "Authorization", "Bearer valid-key", nativeValid},
		{"/emails", "Authorization", "Bearer valid-key", `{"from":"a@example.com","to":"b@example.net","subject":"s","text":"t"}`},
		{"/email", "X-Postmark-Server-Token", "valid-key", `{"From":"a@example.com","To":"b@example.net","Subject":"s","TextBody":"t"}`},
	} {
		f := &fakeService{app: submission.App{ID: "app-a", Operator: "subject-1"}}
		w := request(t, New(f), tc.path, tc.body, map[string]string{tc.header: tc.value})
		if w.Code != http.StatusServiceUnavailable || len(f.requests) != 0 {
			t.Fatalf("%s: operator from Authenticate: %d, %d accepted", tc.path, w.Code, len(f.requests))
		}
	}
}

func TestSubmitTestMessage(t *testing.T) {
	const body = `{"from":"Sender <sender@example.com>","to":["first@example.net"],"subject":"Hello","text":"body","enhanced_protection":true}`
	f := newNativeFake()
	receipt, err := New(f).SubmitTestMessage(context.Background(), "subject-1", "app-a", []byte(body))
	if err != nil || receipt.ID != "message-1" {
		t.Fatal(err)
	}
	if len(f.authorized) != 1 || f.authorized[0] != "app-a|subject-1" || len(f.requests) != 1 {
		t.Fatalf("authorization: %v", f.authorized)
	}
	got, app := f.requests[0], f.apps[0]
	if got.Adapter != submission.AdapterConsole || got.IdempotencyKey != "" || got.Hold || app.Operator != "subject-1" || app.KeyID != "" || !got.Message.EnhancedProtection {
		t.Fatalf("incorrect request: %#v %#v", got, app)
	}
}

func TestSubmitTestMessageProfileRejections(t *testing.T) {
	base := func(extra string) string {
		return `{"from":"a@example.com","to":["b@example.net"],"subject":"s","text":"t"` + extra + `}`
	}
	tests := map[string]string{
		"no recipients":    `{"from":"a@example.com","to":[],"subject":"s","text":"t"}`,
		"six recipients":   `{"from":"a@example.com","to":["1@example.net","2@example.net","3@example.net","4@example.net","5@example.net","6@example.net"],"subject":"s","text":"t"}`,
		"cc":               base(`,"cc":["c@example.net"]`),
		"bcc":              base(`,"bcc":["c@example.net"]`),
		"reply_to":         base(`,"reply_to":["c@example.net"]`),
		"headers":          base(`,"headers":{"References":"<a@example.com>"}`),
		"attachments":      base(`,"attachments":[]`),
		"tracking":         base(`,"tracking":{}`),
		"hold":             base(`,"hold":true`),
		"unknown field":    base(`,"extra":1`),
		"duplicate field":  base(`,"text":"u"`),
		"not an object":    `[]`,
		"empty cc":         base(`,"cc":[]`),
		"empty bcc":        base(`,"bcc":[]`),
		"empty reply_to":   base(`,"reply_to":[]`),
		"empty headers":    base(`,"headers":{}`),
		"hold false":       base(`,"hold":false`),
		"null hold":        base(`,"hold":null`),
		"null cc":          base(`,"cc":null`),
		"null attachments": base(`,"attachments":null`),
		"null tracking":    base(`,"tracking":null`),
		"upper-case key":   `{"FROM":"a@example.com","to":["b@example.net"],"subject":"s","text":"t"}`,
		"invalid message":  `{"from":"not an address","to":["b@example.net"],"subject":"s","text":"t"}`,
		"oversized":        base(`,"html":"` + strings.Repeat("x", 64<<10) + `"`),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			f := newNativeFake()
			_, err := New(f).SubmitTestMessage(context.Background(), "subject-1", "app-a", []byte(body))
			if !errors.Is(err, submission.ErrInvalidRequest) || len(f.requests) != 0 {
				t.Fatalf("got %v, %d accepted", err, len(f.requests))
			}
		})
	}
}

func TestSubmitTestMessageErrors(t *testing.T) {
	const body = `{"from":"a@example.com","to":["b@example.net"],"subject":"s","text":"t"}`
	f := newNativeFake()
	f.authorizeErr = submission.ErrAppPaused
	if _, err := New(f).SubmitTestMessage(context.Background(), "subject-1", "app-a", []byte(body)); !errors.Is(err, submission.ErrAppPaused) || len(f.requests) != 0 {
		t.Fatalf("paused: %v", err)
	}
	f = newNativeFake()
	f.acceptErr = submission.ErrQuotaExceeded
	if _, err := New(f).SubmitTestMessage(context.Background(), "subject-1", "app-a", []byte(body)); !errors.Is(err, submission.ErrQuotaExceeded) {
		t.Fatalf("acceptance refusal not returned: %v", err)
	}
	// A service without the operator capability cannot submit one.
	plain := &fakeService{app: submission.App{ID: "app-a"}}
	if _, err := New(plain).SubmitTestMessage(context.Background(), "subject-1", "app-a", []byte(body)); !errors.Is(err, submission.ErrUnavailable) || len(plain.requests) != 0 {
		t.Fatalf("no capability: %v", err)
	}
	if _, err := New(nil).SubmitTestMessage(context.Background(), "subject-1", "app-a", []byte(body)); !errors.Is(err, submission.ErrUnavailable) {
		t.Fatalf("no service: %v", err)
	}
}

func mergeHeaders(a, b map[string]string) map[string]string {
	merged := map[string]string{}
	for k, v := range a {
		merged[k] = v
	}
	for k, v := range b {
		merged[k] = v
	}
	return merged
}
