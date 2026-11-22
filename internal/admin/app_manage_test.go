package admin

import (
	"context"
	"strings"
	"testing"
)

type fakeApps struct {
	missing          map[string]bool
	renames, deletes int
	actor, app, name string
	err              error
	existsErr        error
}

func (a *fakeApps) AppExists(_ context.Context, id string) (bool, error) {
	return !a.missing[id], a.existsErr
}
func (a *fakeApps) RenameApp(_ context.Context, actor, id, name string) (App, error) {
	a.renames++
	a.actor, a.app, a.name = actor, id, name
	return App{ID: id, Name: name, Active: true}, a.err
}
func (a *fakeApps) DeleteApp(_ context.Context, actor, id string) error {
	a.deletes++
	a.actor, a.app = actor, id
	return a.err
}

func TestAppRenameRoute(t *testing.T) {
	apps := &fakeApps{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, Apps: apps}
	app := strings.Repeat("a", 32)
	path := "/api/v1/apps/" + app
	if w := send(h, "PATCH", path, `{"name":"X"}`, ""); w.Code != 401 {
		t.Fatal("unauthenticated rename", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "PATCH", path, `{"name":"X"}`, "valid"); w.Code != 403 || apps.renames != 0 {
		t.Fatal("non-admin rename", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	for _, body := range []string{``, `{}`, `{"name":""}`, `{"name":" X"}`, `{"name":"X "}`, "{\"name\":\"a\\nb\"}", `{"name":"` + strings.Repeat("n", 129) + `"}`, `{"name":"X","extra":1}`, `{"name":7}`} {
		if w := send(h, "PATCH", path, body, "valid"); w.Code != 400 {
			t.Fatalf("body %q: %d", body, w.Code)
		}
	}
	if apps.renames != 0 {
		t.Fatal("invalid request reached the store")
	}
	if w := send(h, "PATCH", path, `{"name":"Renamed"}`, "valid"); w.Code != 200 || apps.renames != 1 || apps.actor != "operator-1" || apps.app != app || apps.name != "Renamed" || !strings.Contains(w.Body.String(), `"name":"Renamed"`) {
		t.Fatal("rename", w.Code, w.Body.String())
	}
	if w := send(h, "PUT", path, `{"name":"X"}`, "valid"); w.Code != 405 || w.Header().Get("Allow") != "PATCH, DELETE" {
		t.Fatal("method", w.Code)
	}
	if w := send(h, "GET", path, "", "valid"); w.Code != 405 {
		t.Fatal("get", w.Code)
	}
	apps.err = ErrNotFound
	if w := send(h, "PATCH", path, `{"name":"Renamed"}`, "valid"); w.Code != 404 {
		t.Fatal("missing app", w.Code)
	}
	h.Apps = nil
	if w := send(h, "PATCH", path, `{"name":"Renamed"}`, "valid"); w.Code != 503 {
		t.Fatal("missing store", w.Code)
	}
}

func TestAppDeleteRoute(t *testing.T) {
	apps := &fakeApps{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, Apps: apps}
	app := strings.Repeat("a", 32)
	path := "/api/v1/apps/" + app
	if w := send(h, "DELETE", path, "", ""); w.Code != 401 {
		t.Fatal("unauthenticated delete", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "DELETE", path, "", "valid"); w.Code != 403 || apps.deletes != 0 {
		t.Fatal("non-admin delete", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	for _, tc := range []struct{ path, body string }{{path + "?x=1", ""}, {path, `{"x":1}`}} {
		if w := send(h, "DELETE", tc.path, tc.body, "valid"); w.Code != 400 {
			t.Fatal("invalid delete", w.Code)
		}
	}
	if apps.deletes != 0 {
		t.Fatal("invalid request reached the store")
	}
	if w := send(h, "DELETE", path, "", "valid"); w.Code != 204 || apps.deletes != 1 || apps.actor != "operator-1" || apps.app != app {
		t.Fatal("delete", w.Code)
	}
	apps.err = ErrNotFound
	if w := send(h, "DELETE", path, "", "valid"); w.Code != 404 {
		t.Fatal("already deleted", w.Code)
	}
	h.Apps = nil
	if w := send(h, "DELETE", path, "", "valid"); w.Code != 503 {
		t.Fatal("missing store", w.Code)
	}
}

// A deleted app is unknown to every app-scoped route, checked once in the
// shared path before routing.
func TestDeletedAppIsNotFoundEverywhere(t *testing.T) {
	app := strings.Repeat("a", 32)
	apps := &fakeApps{missing: map[string]bool{app: true}}
	status := &fakeAppStatus{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, Apps: apps, AppStatus: status}
	base := "/api/v1/apps/" + app
	for _, tc := range []struct{ method, path, body string }{
		{"GET", base, ""}, {"PATCH", base, `{"name":"X"}`}, {"DELETE", base, ""},
		{"GET", base + "/keys", ""}, {"POST", base + "/keys", `{"name":"k"}`}, {"GET", base + "/messages", ""},
		{"GET", base + "/domains", ""}, {"POST", base + "/deactivate", ""}, {"POST", base + "/activate", ""},
		{"GET", base + "/webhooks", ""}, {"GET", base + "/smtp-credentials", ""}, {"GET", base + "/inbound-domains", ""},
		{"GET", base + "/suppressions", ""}, {"GET", base + "/recipient-quota", ""}, {"GET", base + "/nothing", ""},
	} {
		if w := send(h, tc.method, tc.path, tc.body, "valid"); w.Code != 404 {
			t.Errorf("%s %s: %d", tc.method, tc.path, w.Code)
		}
	}
	if apps.renames != 0 || apps.deletes != 0 || status.calls != 0 {
		t.Fatal("a deleted app reached a handler")
	}
	live := strings.Repeat("b", 32)
	if w := send(h, "GET", "/api/v1/apps/"+live+"/keys", "", "valid"); w.Code == 404 {
		t.Fatal("a live app is hidden")
	}
	apps.existsErr = context.DeadlineExceeded
	if w := send(h, "GET", "/api/v1/apps/"+live+"/keys", "", "valid"); w.Code < 500 {
		t.Fatal("a failed lookup did not fail closed", w.Code)
	}
}
