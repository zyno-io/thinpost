package admin

import (
	"context"
	"strings"
	"testing"
)

type listUnsubscribeTestStore struct {
	*fakeStore
	config ListUnsubscribe
	calls  int
}

func (s *listUnsubscribeTestStore) GetListUnsubscribe(_ context.Context, app string) (ListUnsubscribe, error) {
	s.app = app
	return s.config, nil
}
func (s *listUnsubscribeTestStore) SetListUnsubscribe(_ context.Context, actor, app string, enabled bool) (ListUnsubscribe, error) {
	s.actor, s.app, s.config = actor, app, ListUnsubscribe{Enabled: enabled}
	s.calls++
	return s.config, nil
}

func TestListUnsubscribeRoute(t *testing.T) {
	store := &listUnsubscribeTestStore{fakeStore: &fakeStore{}}
	tracking := &RuntimeInfo{Features: RuntimeFeatures{Tracking: true}, Tracking: &RuntimeTracking{Origin: "https://t.example", LinkEventLimit: 5}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, Runtime: tracking}
	path := "/api/v1/apps/" + strings.Repeat("a", 32) + "/list-unsubscribe"
	if w := send(h, "PUT", path, `{"enabled":true}`, "bad"); w.Code != 401 {
		t.Fatal("unauthenticated update accepted", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "PUT", path, `{"enabled":true}`, "valid"); w.Code != 403 || store.calls != 0 {
		t.Fatal("non-admin update accepted", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	if w := send(h, "GET", path, "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatal("default is not off", w.Code, w.Body.String())
	}
	for _, body := range []string{``, `{}`, `{"enabled":null}`, `{"enabled":"yes"}`, `{"enabled":true,"extra":1}`} {
		if w := send(h, "PUT", path, body, "valid"); w.Code != 400 || store.calls != 0 {
			t.Fatal("invalid body accepted", body, w.Code)
		}
	}
	if w := send(h, "PUT", path, `{"enabled":true}`, "valid"); w.Code != 200 || store.actor != "operator-1" || !store.config.Enabled {
		t.Fatal("enable failed", w.Code)
	}
	if w := send(h, "GET", path, "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatal("read failed")
	}
	if w := send(h, "DELETE", path, "", "valid"); w.Code != 405 {
		t.Fatal("DELETE", w.Code)
	}
}

func TestListUnsubscribeRefusesANonASCIIOrigin(t *testing.T) {
	store := &listUnsubscribeTestStore{fakeStore: &fakeStore{}, config: ListUnsubscribe{Enabled: true}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, Runtime: &RuntimeInfo{Features: RuntimeFeatures{Tracking: true}, Tracking: &RuntimeTracking{Origin: "https://träck.example.com", LinkEventLimit: 5}}}
	path := "/api/v1/apps/" + strings.Repeat("a", 32) + "/list-unsubscribe"
	w := send(h, "PUT", path, `{"enabled":true}`, "valid")
	if w.Code != 409 || store.calls != 0 || !strings.Contains(w.Body.String(), "not ASCII") {
		t.Fatal("non-ASCII origin accepted", w.Code, w.Body.String())
	}
	if w = send(h, "PUT", path, `{"enabled":false}`, "valid"); w.Code != 200 {
		t.Fatal("disable refused", w.Code)
	}
}

func TestListUnsubscribeNeedsTrackingToEnable(t *testing.T) {
	for name, runtime := range map[string]*RuntimeInfo{"no runtime info": nil, "tracking off": {Features: RuntimeFeatures{Submission: true}}} {
		t.Run(name, func(t *testing.T) {
			store := &listUnsubscribeTestStore{fakeStore: &fakeStore{}, config: ListUnsubscribe{Enabled: true}}
			h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, Runtime: runtime}
			path := "/api/v1/apps/" + strings.Repeat("a", 32) + "/list-unsubscribe"
			if w := send(h, "PUT", path, `{"enabled":true}`, "valid"); w.Code != 409 || store.calls != 0 {
				t.Fatal("enabling without tracking", w.Code)
			}
			// Turning it off is always allowed.
			if w := send(h, "PUT", path, `{"enabled":false}`, "valid"); w.Code != 200 || store.config.Enabled {
				t.Fatal("disabling refused", w.Code)
			}
		})
	}
}
