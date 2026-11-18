package admin

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

const (
	testApp     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testInbound = "11111111111111111111111111111111"
)

type inboundMessageFake struct {
	*fakeStore
	listCursor string
	listLimit  int
	listCalls  int
	err        error
	delivCalls int
}

func (s *inboundMessageFake) ListInboundMessages(_ context.Context, actor, app, cursor string, limit int) (InboundMessagePage, error) {
	s.listCalls++
	s.actor, s.app, s.listCursor, s.listLimit = actor, app, cursor, limit
	return InboundMessagePage{}, s.err
}
func (s *inboundMessageFake) GetInboundMessage(_ context.Context, _, app, id string) (InboundMessageDetail, error) {
	if s.err != nil {
		return InboundMessageDetail{}, s.err
	}
	return InboundMessageDetail{Message: InboundMessageSummary{ID: id}}, nil
}
func (s *inboundMessageFake) ListInboundWebhookDeliveries(context.Context, string, string, string, string, int) (WebhookDeliveryPage, error) {
	s.delivCalls++
	return WebhookDeliveryPage{}, s.err
}

type rawFile struct {
	data   []byte
	closed *bool
}

func (r rawFile) Size() int64 { return int64(len(r.data)) }
func (r rawFile) WriteTo(w io.Writer) (int64, error) {
	*r.closed = true
	n, err := w.Write(r.data)
	return int64(n), err
}

type inboundContentFake struct {
	err    error
	calls  []string
	opened bool
	body   MessageBody
}

func (c *inboundContentFake) ReadInboundContent(_ context.Context, actor, app, id string) (MessageBody, error) {
	c.calls = append(c.calls, "body "+actor+" "+app+" "+id)
	return c.body, c.err
}
func (c *inboundContentFake) OpenInboundRaw(_ context.Context, actor, app, id string) (InboundRaw, error) {
	c.calls = append(c.calls, "raw "+actor+" "+app+" "+id)
	if c.err != nil {
		return nil, c.err
	}
	return rawFile{data: []byte("From: a@b\r\n\r\nbody"), closed: &c.opened}, nil
}

func inboundHandler(store *inboundMessageFake, content *inboundContentFake) *Handler {
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
	if content != nil {
		h.InboundContent = content
	}
	return h
}

func TestInboundMessageListAcceptsOnlyPaging(t *testing.T) {
	store := &inboundMessageFake{fakeStore: &fakeStore{}}
	h := inboundHandler(store, nil)
	base := "/api/v1/apps/" + testApp + "/inbound-messages"
	if w := send(h, "GET", base, "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"messages":[]`) || store.listLimit != 50 || store.app != testApp || store.actor != "operator-1" {
		t.Fatal("list", w.Code, w.Body.String(), store.listLimit)
	}
	if w := send(h, "GET", base+"?limit=7&cursor=abc", "", "valid"); w.Code != 200 || store.listLimit != 7 || store.listCursor != "abc" {
		t.Fatal("paging", w.Code)
	}
	calls := store.listCalls
	// Search and filters are Phase 2: nothing but paging reaches the store, and
	// nothing sensitive can ride in a query string.
	for _, query := range []string{"?q=alice@example.com", "?from=a@b.example", "?subject=hello", "?limit=0", "?limit=101", "?limit=x", "?limit=1&limit=2", "?limit=5&%zz=x", "?limit=5;q=x", "?limit=5&cursor=", "?cursor=" + strings.Repeat("a", 513)} {
		if w := send(h, "GET", base+query, "", "valid"); w.Code != 400 {
			t.Fatal("accepted", query, w.Code)
		}
	}
	if store.listCalls != calls {
		t.Fatal("a rejected query reached the store")
	}
	if w := send(h, "POST", base, "{}", "valid"); w.Code != 405 {
		t.Fatal("method", w.Code)
	}
	if w := send(h, "GET", base, "", ""); w.Code != 401 {
		t.Fatal("anonymous", w.Code)
	}
	if w := send(&Handler{Authenticator: fakeAuth{admin: false}, Store: store}, "GET", base, "", "valid"); w.Code != 403 {
		t.Fatal("non-admin", w.Code)
	}
}

func TestInboundMessageDetailAndErrors(t *testing.T) {
	store := &inboundMessageFake{fakeStore: &fakeStore{}}
	h := inboundHandler(store, nil)
	base := "/api/v1/apps/" + testApp + "/inbound-messages/"
	w := send(h, "GET", base+testInbound, "", "valid")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"recipients":[]`) || !strings.Contains(w.Body.String(), `"dkim":[]`) || !strings.Contains(w.Body.String(), `"arc":{"present":false,"evaluated":false}`) {
		t.Fatal("detail", w.Code, w.Body.String())
	}
	if w = send(h, "GET", base+testInbound+"?x=1", "", "valid"); w.Code != 400 {
		t.Fatal("query on detail", w.Code)
	}
	for _, bad := range []string{"not-an-id", strings.Repeat("A", 32), testInbound + "/other", testInbound + "/content/extra"} {
		if w = send(h, "GET", base+bad, "", "valid"); w.Code != 404 {
			t.Fatal("bad path", bad, w.Code)
		}
	}
	store.err = ErrNotFound
	if w = send(h, "GET", base+testInbound, "", "valid"); w.Code != 404 {
		t.Fatal("another app's id", w.Code)
	}
	store.err = errors.New("database down")
	if w = send(h, "GET", base+testInbound, "", "valid"); w.Code != 503 || strings.Contains(w.Body.String(), "database") {
		t.Fatal("store failure", w.Code, w.Body.String())
	}
	store.err = nil
	if w = send(h, "GET", base+testInbound+"/webhook-deliveries?limit=10", "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"deliveries":[]`) || store.delivCalls != 1 {
		t.Fatal("deliveries", w.Code, w.Body.String())
	}
	for _, query := range []string{"?bogus=1", "?limit=5;q=x", "?limit=5&%zz=x"} {
		if w = send(h, "GET", base+testInbound+"/webhook-deliveries"+query, "", "valid"); w.Code != 400 {
			t.Fatal("deliveries query", query, w.Code)
		}
	}
}

func TestInboundContentAndRawRoutes(t *testing.T) {
	store := &inboundMessageFake{fakeStore: &fakeStore{}}
	content := &inboundContentFake{body: MessageBody{Subject: "s", Text: "t"}}
	h := inboundHandler(store, content)
	base := "/api/v1/apps/" + testApp + "/inbound-messages/" + testInbound
	w := send(h, "GET", base+"/content", "", "valid")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"attachments":[]`) || !strings.Contains(w.Body.String(), `"text":"t"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("content", w.Code, w.Body.String())
	}
	if len(content.calls) != 1 || content.calls[0] != "body operator-1 "+testApp+" "+testInbound {
		t.Fatal("call", content.calls)
	}
	w = send(h, "GET", base+"/raw", "", "valid")
	if w.Code != 200 || w.Body.String() != "From: a@b\r\n\r\nbody" || !content.opened {
		t.Fatal("raw", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("Content-Disposition") != `attachment; filename="inbound-`+testInbound+`.eml"` ||
		w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Length") != "17" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatal("raw headers", w.Header())
	}
	for _, path := range []string{"/content", "/raw"} {
		if w = send(h, "GET", base+path+"?x=1", "", "valid"); w.Code != 400 {
			t.Fatal("query accepted", path)
		}
		if w = send(h, "POST", base+path, "", "valid"); w.Code != 405 {
			t.Fatal("method", path)
		}
	}
	// Expired retention is 409 with a constant body, and no bytes follow it.
	content.err = ErrContentExpired
	for _, path := range []string{"/content", "/raw"} {
		w = send(h, "GET", base+path, "", "valid")
		if w.Code != 409 || strings.TrimSpace(w.Body.String()) != `{"error":"content_expired"}` || w.Header().Get("Content-Disposition") != "" {
			t.Fatal("expired", path, w.Code, w.Body.String())
		}
	}
	content.err = ErrNotFound
	if w = send(h, "GET", base+"/raw", "", "valid"); w.Code != 404 {
		t.Fatal("another app's raw", w.Code)
	}
	content.err = errors.New("storage")
	if w = send(h, "GET", base+"/content", "", "valid"); w.Code != 503 || strings.Contains(w.Body.String(), "storage") {
		t.Fatal("storage", w.Code, w.Body.String())
	}
	// Without object storage the content routes are unavailable, not empty.
	if w = send(inboundHandler(store, nil), "GET", base+"/content", "", "valid"); w.Code != 503 {
		t.Fatal("no content reader", w.Code)
	}
}

func TestDeriveInboundWebhookStatus(t *testing.T) {
	for _, c := range []struct {
		delivered, pending, failed int
		want                       string
	}{{0, 0, 0, InboundWebhookNone}, {2, 0, 0, InboundWebhookDelivered}, {1, 1, 1, InboundWebhookPending}, {0, 0, 2, InboundWebhookFailed}, {1, 0, 1, InboundWebhookPartial}} {
		if got := DeriveInboundWebhookStatus(c.delivered, c.pending, c.failed); got != c.want {
			t.Fatal(c, got)
		}
	}
}
