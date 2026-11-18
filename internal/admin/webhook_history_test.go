package admin

import (
	"context"
	"strings"
	"testing"
)

type historyStore struct {
	calls                                   int
	actor, app, event, subscription, cursor string
	limit                                   int
	err                                     error
}

func (s *historyStore) ListWebhookDeliveries(_ context.Context, actor, app, cursor string, limit int) (WebhookDeliveryPage, error) {
	s.calls++
	s.actor, s.app, s.cursor, s.limit = actor, app, cursor, limit
	return WebhookDeliveryPage{}, s.err
}
func (s *historyStore) ListWebhookAttempts(_ context.Context, actor, app, event, subscription string) (WebhookAttemptPage, error) {
	s.calls++
	s.actor, s.app, s.event, s.subscription = actor, app, event, subscription
	return WebhookAttemptPage{}, s.err
}
func (s *historyStore) ReplayWebhookDelivery(_ context.Context, actor, app, event, subscription string) error {
	s.calls++
	s.actor, s.app, s.event, s.subscription = actor, app, event, subscription
	return s.err
}

func TestWebhookHistoryAuthorizationAndValidation(t *testing.T) {
	s := &historyStore{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, WebhookHistory: s}
	app, event, sub := strings.Repeat("a", 32), strings.Repeat("c", 32), strings.Repeat("b", 32)
	base := "/api/v1/apps/" + app + "/webhook-deliveries"
	action := base + "/" + event + "/" + sub
	if w := send(h, "GET", base, "", ""); w.Code != 401 {
		t.Fatal("missing authentication", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "POST", action+"/replay", "", "valid"); w.Code != 403 || s.calls != 0 {
		t.Fatal("non-admin reached history", w.Code, s.calls)
	}
	h.Authenticator = fakeAuth{admin: true}
	for _, query := range []string{"?limit=0", "?limit=101", "?limit=1&limit=2", "?cursor=", "?unknown=x", "?cursor=" + strings.Repeat("x", 513)} {
		if w := send(h, "GET", base+query, "", "valid"); w.Code != 400 {
			t.Errorf("invalid query %q: %d", query, w.Code)
		}
	}
	if s.calls != 0 {
		t.Fatal("invalid query reached store")
	}
	w := send(h, "GET", base+"?cursor=abc&limit=4", "", "valid")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"deliveries":[]`) || s.actor != "operator-1" || s.app != app || s.cursor != "abc" || s.limit != 4 {
		t.Fatal("incorrect list response or scope", w.Code, w.Body.String())
	}
	w = send(h, "GET", action+"/attempts", "", "valid")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"attempts":[]`) || s.event != event || s.subscription != sub {
		t.Fatal("incorrect attempt scope", w.Code)
	}
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", action + "/attempts?extra=x", "", 400},
		{"POST", action + "/replay", `{"endpoint":"https://evil.example"}`, 400},
		{"GET", action + "/replay", "", 405},
		{"POST", base, "", 405},
	} {
		if w := send(h, tc.method, tc.path, tc.body, "valid"); w.Code != tc.status {
			t.Errorf("%s %s: got %d, want %d", tc.method, tc.path, w.Code, tc.status)
		}
	}
	w = send(h, "POST", action+"/replay", "", "valid")
	if w.Code != 204 || s.event != event || s.subscription != sub {
		t.Fatal("replay not scoped to the requested delivery", w.Code)
	}
	s.err = ErrConflict
	if w = send(h, "POST", action+"/replay", "", "valid"); w.Code != 409 {
		t.Fatal("conflict not reported", w.Code)
	}
	s.err = ErrNotFound
	if w = send(h, "GET", action+"/attempts", "", "valid"); w.Code != 404 {
		t.Fatal("missing delivery not reported", w.Code)
	}
	h.WebhookHistory = nil
	if w = send(h, "GET", base, "", "valid"); w.Code != 503 {
		t.Fatal("missing history store did not fail closed", w.Code)
	}
}
