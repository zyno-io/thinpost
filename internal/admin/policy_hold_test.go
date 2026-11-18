package admin

import (
	"context"
	"strings"
	"testing"
)

type fakePolicyHolds struct {
	calls                         int
	actor, app, message, delivery string
	err                           error
}

func (s *fakePolicyHolds) RetryPolicyHold(_ context.Context, actor, app, message, delivery string) error {
	s.calls++
	s.actor, s.app, s.message, s.delivery = actor, app, message, delivery
	return s.err
}

func TestRetryPolicyHoldAuthorizationAndScope(t *testing.T) {
	s := &fakePolicyHolds{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, PolicyHolds: s}
	app, message, delivery := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	path := "/api/v1/apps/" + app + "/messages/" + message + "/deliveries/" + delivery + "/retry-policy-hold"
	if w := send(h, "POST", path, "", ""); w.Code != 401 {
		t.Fatal("unauthenticated retry", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "POST", path, "", "valid"); w.Code != 403 || s.calls != 0 {
		t.Fatal("non-admin retry", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", path, "", 405}, {"POST", path + "?x=1", "", 400}, {"POST", path, `{"override":true}`, 400},
	} {
		if w := send(h, tc.method, tc.path, tc.body, "valid"); w.Code != tc.status {
			t.Fatal("invalid request", w.Code)
		}
	}
	if s.calls != 0 {
		t.Fatal("invalid request reached store")
	}
	w := send(h, "POST", path, "", "valid")
	if w.Code != 204 || s.actor != "operator-1" || s.app != app || s.message != message || s.delivery != delivery {
		t.Fatal("wrong scope or response", w.Code)
	}
	s.err = ErrConflict
	if w = send(h, "POST", path, "", "valid"); w.Code != 409 {
		t.Fatal("conflict not reported", w.Code)
	}
	s.err = ErrNotFound
	if w = send(h, "POST", path, "", "valid"); w.Code != 404 {
		t.Fatal("missing delivery not reported", w.Code)
	}
	h.PolicyHolds = nil
	if w = send(h, "POST", path, "", "valid"); w.Code != 503 {
		t.Fatal("missing store did not fail closed", w.Code)
	}
}
