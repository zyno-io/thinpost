package admin

import (
	"context"
	"strings"
	"testing"
)

type fakeAppStatus struct {
	calls      int
	actor, app string
	active     bool
	err        error
}

func (s *fakeAppStatus) SetAppActive(_ context.Context, actor, app string, active bool) error {
	s.calls++
	s.actor, s.app, s.active = actor, app, active
	return s.err
}

func TestAppStatusRequiresAdminAndExactOperation(t *testing.T) {
	s := &fakeAppStatus{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, AppStatus: s}
	app := strings.Repeat("a", 32)
	path := "/api/v1/apps/" + app + "/deactivate"
	if w := send(h, "POST", path, "", ""); w.Code != 401 {
		t.Fatal("unauthenticated pause", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "POST", path, "", "valid"); w.Code != 403 || s.calls != 0 {
		t.Fatal("non-admin pause", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{{"GET", path, "", 405}, {"POST", path + "?x=1", "", 400}, {"POST", path, `{"active":true}`, 400}} {
		if w := send(h, tc.method, tc.path, tc.body, "valid"); w.Code != tc.status {
			t.Fatal("invalid status request", w.Code)
		}
	}
	if s.calls != 0 {
		t.Fatal("invalid request reached store")
	}
	if w := send(h, "POST", path, "", "valid"); w.Code != 204 || s.active || s.actor != "operator-1" || s.app != app {
		t.Fatal("pause routing", w.Code)
	}
	if w := send(h, "POST", "/api/v1/apps/"+app+"/activate", "", "valid"); w.Code != 204 || !s.active {
		t.Fatal("resume routing", w.Code)
	}
	s.err = ErrNotFound
	if w := send(h, "POST", path, "", "valid"); w.Code != 404 {
		t.Fatal("missing app", w.Code)
	}
	h.AppStatus = nil
	if w := send(h, "POST", path, "", "valid"); w.Code != 503 {
		t.Fatal("missing status store", w.Code)
	}
}
