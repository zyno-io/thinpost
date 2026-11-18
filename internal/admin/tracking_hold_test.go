package admin

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeTrackingHolds struct {
	calls                         int
	actor, app, message, delivery string
	err                           error
}

func (s *fakeTrackingHolds) AbandonTrackingHold(_ context.Context, actor, app, message, delivery string) error {
	s.calls++
	s.actor, s.app, s.message, s.delivery = actor, app, message, delivery
	return s.err
}

func TestAbandonTrackingHoldAuthorizationAndScope(t *testing.T) {
	store := &fakeTrackingHolds{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, TrackingHolds: store}
	app, message, delivery := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	path := "/api/v1/apps/" + app + "/messages/" + message + "/deliveries/" + delivery + "/abandon-tracking-hold"
	if w := send(h, "POST", path, "", ""); w.Code != 401 {
		t.Fatal("unauthenticated abandonment", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "POST", path, "", "valid"); w.Code != 403 || store.calls != 0 {
		t.Fatal("non-admin abandonment", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", path, "", 405}, {"POST", path + "?x=1", "", 400}, {"POST", path, `{"override":true}`, 400},
	} {
		if w := send(h, test.method, test.path, test.body, "valid"); w.Code != test.status {
			t.Fatal("invalid request", w.Code)
		}
	}
	unknownLength := httptest.NewRequest("POST", path, io.NopCloser(strings.NewReader(`{"override":true}`)))
	unknownLength.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, unknownLength)
	if response.Code != 400 {
		t.Fatal("unknown-length request body accepted", response.Code)
	}
	if store.calls != 0 {
		t.Fatal("invalid request reached store")
	}
	w := send(h, "POST", path, "", "valid")
	if w.Code != 204 || store.actor != "operator-1" || store.app != app || store.message != message || store.delivery != delivery {
		t.Fatal("wrong scope or response", w.Code)
	}
	store.err = ErrConflict
	if w = send(h, "POST", path, "", "valid"); w.Code != 409 {
		t.Fatal("conflict not reported", w.Code)
	}
	store.err = ErrNotFound
	if w = send(h, "POST", path, "", "valid"); w.Code != 404 {
		t.Fatal("missing delivery not reported", w.Code)
	}
	h.TrackingHolds = nil
	if w = send(h, "POST", path, "", "valid"); w.Code != 503 {
		t.Fatal("missing store did not fail closed", w.Code)
	}
}
