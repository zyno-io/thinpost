package admin

import (
	"context"
	"strings"
	"testing"
	"time"
)

type feedbackLoopTestStore struct {
	*fakeStore
	mailbox   *FeedbackMailbox
	reporters []string
	rotations int
	saves     int
}

func (s *feedbackLoopTestStore) FeedbackLoops(context.Context) (FeedbackLoops, error) {
	return FeedbackLoops{Mailbox: s.mailbox, Reporters: s.reporters, Counts: []FeedbackCount{{Day: "2026-11-11", Channel: "arf", Result: "accepted", Count: 2}}}, nil
}
func (s *feedbackLoopTestStore) CreateFeedbackMailbox(_ context.Context, actor string, rotate bool) (FeedbackMailbox, error) {
	s.actor = actor
	if s.mailbox != nil && !rotate {
		return FeedbackMailbox{}, ErrConflict
	}
	if s.mailbox != nil {
		s.rotations++
	}
	s.mailbox = &FeedbackMailbox{Token: strings.Repeat("ab", 16), CreatedAt: time.Unix(0, 0).UTC()}
	return *s.mailbox, nil
}
func (s *feedbackLoopTestStore) SetFeedbackReporters(_ context.Context, actor string, domains []string) ([]string, error) {
	s.actor = actor
	s.saves++
	s.reporters = domains
	return domains, nil
}

func TestFeedbackLoopRoutes(t *testing.T) {
	store := &feedbackLoopTestStore{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, BounceDomain: "mx.example.net"}
	if w := send(h, "GET", "/api/v1/feedback-loops", "", "bad"); w.Code != 401 {
		t.Fatal("unauthenticated read", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "POST", "/api/v1/feedback-loops/mailbox", `{}`, "valid"); w.Code != 403 || store.mailbox != nil {
		t.Fatal("non-admin created a mailbox", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	if w := send(h, "GET", "/api/v1/feedback-loops", "", "valid"); w.Code != 200 || strings.Contains(w.Body.String(), "mailbox") || !strings.Contains(w.Body.String(), `"reporters":[]`) || !strings.Contains(w.Body.String(), `"receiving":true`) {
		t.Fatal("empty state", w.Code, w.Body.String())
	}
	if w := send(h, "POST", "/api/v1/feedback-loops/mailbox", `{}`, "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"address":"b+`+strings.Repeat("ab", 16)+`@mx.example.net"`) || store.actor != "operator-1" {
		t.Fatal("create", w.Code, w.Body.String())
	}
	// A second create needs an explicit rotate.
	for _, body := range []string{`{}`, `{"rotate":false}`} {
		if w := send(h, "POST", "/api/v1/feedback-loops/mailbox", body, "valid"); w.Code != 409 || store.rotations != 0 {
			t.Fatal("create over an existing mailbox", body, w.Code)
		}
	}
	for _, body := range []string{`{"rotate":"yes"}`, `{"extra":1}`, `[]`} {
		if w := send(h, "POST", "/api/v1/feedback-loops/mailbox", body, "valid"); w.Code != 400 {
			t.Fatal("invalid body accepted", body, w.Code)
		}
	}
	if w := send(h, "POST", "/api/v1/feedback-loops/mailbox", `{"rotate":true}`, "valid"); w.Code != 200 || store.rotations != 1 {
		t.Fatal("rotate", w.Code)
	}
	if w := send(h, "GET", "/api/v1/feedback-loops", "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"address":"b+`) || !strings.Contains(w.Body.String(), `"count":2`) {
		t.Fatal("read", w.Code, w.Body.String())
	}
	if w := send(h, "GET", "/api/v1/feedback-loops?x=1", "", "valid"); w.Code != 503 {
		t.Fatal("query accepted", w.Code)
	}
	if w := send(h, "DELETE", "/api/v1/feedback-loops/mailbox", "", "valid"); w.Code != 405 {
		t.Fatal("wrong method", w.Code)
	}
	// Without a bounce domain no mailbox is created and none is shown as an address.
	h.BounceDomain = ""
	store.mailbox = nil
	if w := send(h, "POST", "/api/v1/feedback-loops/mailbox", `{}`, "valid"); w.Code != 409 || store.mailbox != nil {
		t.Fatal("created without a bounce domain", w.Code)
	}
}

func TestFeedbackReportersRoute(t *testing.T) {
	store := &feedbackLoopTestStore{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, BounceDomain: "mx.example.net"}
	const path = "/api/v1/feedback-loops/reporters"
	for _, body := range []string{``, `{}`, `{"domains":null}`, `{"domains":["bad domain"]}`, `{"domains":["localhost"]}`, `{"domains":["10.0.0.1"]}`, `{"domains":["ü.example"]}`,
		`{"domains":[` + strings.Repeat(`"a.example",`, 32) + `"b.example"]}`, `{"domains":["a.example"],"x":1}`} {
		if w := send(h, "PUT", path, body, "valid"); w.Code != 400 || store.saves != 0 {
			t.Fatal("invalid body accepted", body, w.Code)
		}
	}
	if w := send(h, "PUT", path, `{"domains":["ARF.Mail.Yahoo.com","arf.mail.yahoo.com","x.example"]}`, "valid"); w.Code != 200 || strings.Join(store.reporters, ",") != "arf.mail.yahoo.com,x.example" || store.actor != "operator-1" {
		t.Fatal("normalized save", w.Code, store.reporters)
	}
	if w := send(h, "PUT", path, `{"domains":[]}`, "valid"); w.Code != 200 || len(store.reporters) != 0 {
		t.Fatal("clear", w.Code)
	}
	if w := send(h, "GET", path, "", "valid"); w.Code != 405 {
		t.Fatal("wrong method", w.Code)
	}
}
