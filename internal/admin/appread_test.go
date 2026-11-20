package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/submission"
)

const (
	appA, appB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	msgA, msgB = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
)

// appKeys authenticates "keyA" and "keyB"; "revoked" is a revoked key and
// "down" simulates an unavailable service.
type appKeys struct{ calls int }

func (k *appKeys) Authenticate(_ context.Context, key string) (submission.App, error) {
	k.calls++
	switch key {
	case "keyA":
		return submission.App{ID: appA, KeyID: "k1"}, nil
	case "keyB":
		return submission.App{ID: appB, KeyID: "k2"}, nil
	case "down":
		return submission.App{}, submission.ErrUnavailable
	}
	return submission.App{}, submission.ErrInvalidKey
}
func (*appKeys) Accept(context.Context, submission.App, submission.Request) (submission.Receipt, error) {
	return submission.Receipt{}, errors.New("not used")
}

// scopedReads owns one message per app and records the actor and app of reads.
type scopedReads struct {
	actor, app string
	calls      int
	attachment AttachmentContent
}

var ownership = map[string]string{msgA: appA, msgB: appB}

func (s *scopedReads) own(actor, app, msg string) error {
	s.calls++
	s.actor, s.app = actor, app
	if ownership[msg] != app {
		return ErrNotFound
	}
	return nil
}
func (s *scopedReads) GetMessage(_ context.Context, actor, app, msg string) (MessageDetail, error) {
	return MessageDetail{Message: MessageSummary{ID: msg}}, s.own(actor, app, msg)
}
func (s *scopedReads) ReadMessageBody(_ context.Context, actor, app, msg string) (MessageBody, error) {
	return MessageBody{Text: "private"}, s.own(actor, app, msg)
}
func (s *scopedReads) ReadAttachment(_ context.Context, actor, app, msg string, _ int) (AttachmentContent, error) {
	return s.attachment, s.own(actor, app, msg)
}
func (s *scopedReads) OpenMessageRaw(context.Context, string, string, string) ([]byte, error) {
	return nil, errors.New("not routed")
}
func (s *scopedReads) ReadDeliveryHeaders(context.Context, string, string, string, string) ([]HeaderField, error) {
	return nil, errors.New("not routed")
}

func appGet(h http.Handler, method, path, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func newAppRead() (*AppReadHandler, *scopedReads, *appKeys) {
	reads := &scopedReads{attachment: AttachmentContent{Filename: "a.html", ContentType: "text/html", Content: []byte("<b>x</b>")}}
	keys := &appKeys{}
	return &AppReadHandler{Service: keys, Store: reads, ContentReader: reads}, reads, keys
}

func TestAppReadScopedToKeyApp(t *testing.T) {
	h, reads, _ := newAppRead()
	for _, suffix := range []string{"", "/body", "/attachments/0"} {
		reads.calls = 0
		got := appGet(h, "GET", "/app/v1/messages/"+msgA+suffix, "keyA")
		if got.Code != 200 || reads.app != appA || reads.actor != "key:k1" {
			t.Fatalf("own message %q: %d app=%q actor=%q", suffix, got.Code, reads.app, reads.actor)
		}
		// The other app's message looks absent, and the lookup is scoped to the
		// authenticated key's app, never to anything in the request.
		got = appGet(h, "GET", "/app/v1/messages/"+msgB+suffix, "keyA")
		if got.Code != 404 || reads.app != appA || strings.Contains(got.Body.String(), msgB) {
			t.Fatalf("cross-app %q: %d", suffix, got.Code)
		}
		if got = appGet(h, "GET", "/app/v1/messages/"+msgB+suffix, "keyB"); got.Code != 200 || reads.actor != "key:k2" {
			t.Fatalf("second app %q: %d", suffix, got.Code)
		}
	}
}

func TestAppReadResponseShapesAndHeaders(t *testing.T) {
	h, _, _ := newAppRead()
	got := appGet(h, "GET", "/app/v1/messages/"+msgA, "keyA")
	if !strings.Contains(got.Body.String(), `"deliveries":[]`) || got.Header().Get("Content-Type") != "application/json" {
		t.Fatal("message shape", got.Body.String())
	}
	got = appGet(h, "GET", "/app/v1/messages/"+msgA+"/body", "keyA")
	if !strings.Contains(got.Body.String(), `"attachments":[]`) || got.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("body shape", got.Body.String())
	}
	got = appGet(h, "GET", "/app/v1/messages/"+msgA+"/attachments/3", "keyA")
	for name, want := range map[string]string{
		"Content-Type":                 "application/octet-stream",
		"X-Content-Type-Options":       "nosniff",
		"Cache-Control":                "no-store",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'; sandbox",
		"Content-Length":               "8",
	} {
		if got.Header().Get(name) != want {
			t.Fatal("attachment header", name, got.Header().Get(name))
		}
	}
	if !strings.HasPrefix(got.Header().Get("Content-Disposition"), "attachment;") || got.Body.String() != "<b>x</b>" {
		t.Fatal("attachment not an opaque download")
	}
	// Provider renderings of the message ID resolve to the 32-hex form.
	if got = appGet(h, "GET", "/app/v1/messages/0123456789abcdef-0123-456789abcdef", "keyA"); got.Code != 404 {
		t.Fatal("malformed ID accepted")
	}
	if got = appGet(h, "GET", "/app/v1/messages/01234567-89ab-cdef-0123-456789abcdef", "keyA"); got.Code != 200 {
		t.Fatal("UUID rendering rejected", got.Code)
	}
}

func TestAppReadAuthentication(t *testing.T) {
	h, reads, _ := newAppRead()
	path := "/app/v1/messages/" + msgA
	for name, key := range map[string]string{"missing": "", "wrong": "nope", "revoked": "revoked"} {
		if got := appGet(h, "GET", path+"/body", key); got.Code != 401 || reads.calls != 0 {
			t.Fatal(name, "key accepted", got.Code)
		}
	}
	for _, header := range [][]string{{"Basic keyA"}, {"Bearer keyA", "Bearer keyA"}, {"Bearer"}, {"Bearer a b"}} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header["Authorization"] = header
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 || reads.calls != 0 {
			t.Fatal("malformed credential accepted", header)
		}
	}
	if got := appGet(h, "GET", path, "down"); got.Code != 503 || reads.calls != 0 {
		t.Fatal("authentication outage must be 503", got.Code)
	}
	// An operator token is not an app key.
	if got := appGet(h, "GET", path, "valid"); got.Code != 401 {
		t.Fatal("non-app credential accepted")
	}
}

func TestAppReadRejectsQueryMethodAndPaths(t *testing.T) {
	h, reads, keys := newAppRead()
	base := "/app/v1/messages/" + msgA
	for _, suffix := range []string{"?x=1", "?", "/body?x=1", "/attachments/0?download=1"} {
		keys.calls = 0
		if got := appGet(h, "GET", base+suffix, "keyA"); got.Code != 400 || reads.calls != 0 || keys.calls != 0 {
			t.Fatal("query accepted:", suffix, got.Code)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		got := appGet(h, method, base, "keyA")
		if got.Code != 405 || got.Header().Get("Allow") != "GET" || reads.calls != 0 {
			t.Fatal("method accepted:", method, got.Code)
		}
	}
	for _, path := range []string{
		"/app/v1/messages/", "/app/v1/messages", "/app/v1/messages/x", base + "/", base + "/body/", base + "/headers",
		base + "/attachments", base + "/attachments/", base + "/attachments/20", base + "/attachments/01",
		base + "/attachments/-1", base + "/attachments/0/x", "/app/v2/messages/" + msgA, "/app/v1/other",
	} {
		if got := appGet(h, "GET", path, "keyA"); got.Code != 404 || reads.calls != 0 {
			t.Fatal("path accepted:", path, got.Code)
		}
	}
}

func TestAppReadFailsClosedAndMapsErrors(t *testing.T) {
	h, reads, _ := newAppRead()
	body := "/app/v1/messages/" + msgA + "/body"
	h.ContentReader = nil
	if got := appGet(h, "GET", body, "keyA"); got.Code != 503 {
		t.Fatal("missing storage must fail closed", got.Code)
	}
	if got := appGet(h, "GET", "/app/v1/messages/"+msgA+"/attachments/0", "keyA"); got.Code != 503 {
		t.Fatal("missing storage must fail closed", got.Code)
	}
	if got := appGet(&AppReadHandler{Store: reads}, "GET", body, "keyA"); got.Code != 503 {
		t.Fatal("missing service must fail closed")
	}
	errs := &errReader{}
	h = &AppReadHandler{Service: &appKeys{}, Store: errs, ContentReader: errs}
	for _, tc := range []struct {
		err  error
		code int
	}{{ErrNotFound, 404}, {ErrConflict, 409}, {errors.New("db"), 503}} {
		errs.err = tc.err
		for _, suffix := range []string{"", "/body", "/attachments/0"} {
			got := appGet(h, "GET", "/app/v1/messages/"+msgA+suffix, "keyA")
			if got.Code != tc.code || strings.Contains(got.Body.String(), "db") || got.Header().Get("Content-Type") == "application/octet-stream" {
				t.Fatal("wrong status", suffix, tc.code, got.Code)
			}
		}
	}
}

type errReader struct{ err error }

func (e *errReader) GetMessage(context.Context, string, string, string) (MessageDetail, error) {
	return MessageDetail{}, e.err
}
func (e *errReader) ReadMessageBody(context.Context, string, string, string) (MessageBody, error) {
	return MessageBody{}, e.err
}
func (e *errReader) ReadAttachment(context.Context, string, string, string, int) (AttachmentContent, error) {
	return AttachmentContent{}, e.err
}
func (e *errReader) OpenMessageRaw(context.Context, string, string, string) ([]byte, error) {
	return nil, e.err
}
func (e *errReader) ReadDeliveryHeaders(context.Context, string, string, string, string) ([]HeaderField, error) {
	return nil, e.err
}

func TestAppReadRejectsPercentEncodedPaths(t *testing.T) {
	h, reads, _ := newAppRead()
	for _, path := range []string{
		"/%61pp/v1/messages/" + msgA, "/app/%76%31/messages/" + msgA, "/app/v1/%6Dessages/" + msgA,
		"/app/v1/messages/" + msgA + "%2Fbody", "/app/v1/messages/" + msgA + "/%62ody",
		"/app/v1/messages/" + msgA + "/attachments/%30", "/app/v1/messages/%30123456789abcdef0123456789abcdef",
		"/app/v1/messages/%3C" + msgA + "@example.com%3E", "/app%2Fv1/messages/" + msgA,
	} {
		if got := appGet(h, "GET", path, "keyA"); got.Code != 404 || reads.calls != 0 {
			t.Fatal("encoded path accepted:", path, got.Code)
		}
	}
}

// gatedReads blocks content reads until released, to hold the read budget.
type gatedReads struct {
	scopedReads
	entered chan struct{}
	release chan struct{}
}

func (g *gatedReads) ReadMessageBody(ctx context.Context, actor, app, msg string) (MessageBody, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.scopedReads.ReadMessageBody(ctx, actor, app, msg)
}
func (g *gatedReads) ReadAttachment(ctx context.Context, actor, app, msg string, i int) (AttachmentContent, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.scopedReads.ReadAttachment(ctx, actor, app, msg, i)
}

func TestAppContentReadsAreBudgeted(t *testing.T) {
	g := &gatedReads{entered: make(chan struct{}, 4), release: make(chan struct{})}
	h := &AppReadHandler{Service: &appKeys{}, Store: &g.scopedReads, ContentReader: g, readBudget: 16, readWeight: 16, waitTimeout: 50 * time.Millisecond}
	done := make(chan int, 1)
	go func() { done <- appGet(h, "GET", "/app/v1/messages/"+msgA+"/body", "keyA").Code }()
	<-g.entered
	for _, suffix := range []string{"/body", "/attachments/0"} {
		got := appGet(h, "GET", "/app/v1/messages/"+msgA+suffix, "keyA")
		if got.Code != 503 || got.Header().Get("Retry-After") != "5" || got.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("busy read not shed:", suffix, got.Code)
		}
	}
	// Status reads carry no content and are not budgeted.
	if got := appGet(h, "GET", "/app/v1/messages/"+msgA, "keyA"); got.Code != 200 {
		t.Fatal("status read blocked by the content budget", got.Code)
	}
	close(g.release)
	if code := <-done; code != 200 {
		t.Fatal("held read failed", code)
	}
	// The budget is returned: later reads (including after an error) succeed.
	if got := appGet(h, "GET", "/app/v1/messages/"+msgA+"/body", "keyA"); got.Code != 200 {
		t.Fatal("budget not released", got.Code)
	}
	if got := appGet(h, "GET", "/app/v1/messages/"+msgB+"/body", "keyA"); got.Code != 404 {
		t.Fatal("cross-app", got.Code)
	}
	if got := appGet(h, "GET", "/app/v1/messages/"+msgA+"/attachments/0", "keyA"); got.Code != 200 {
		t.Fatal("budget leaked after an error", got.Code)
	}
}
