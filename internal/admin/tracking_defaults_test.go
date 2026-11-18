package admin

import (
	"context"
	"strings"
	"testing"
)

// trackingDefaultsTestStore enforces canEnable like the real store.
type trackingDefaultsTestStore struct {
	*fakeStore
	config TrackingDefaults
	calls  int
}

func (s *trackingDefaultsTestStore) GetTrackingDefaults(_ context.Context, app string) (TrackingDefaults, error) {
	s.app = app
	return s.config, nil
}
func (s *trackingDefaultsTestStore) SetTrackingDefaults(_ context.Context, actor, app string, v TrackingDefaults, canEnable bool) (TrackingDefaults, error) {
	if !canEnable && (v.Opens && !s.config.Opens || v.Clicks && !s.config.Clicks) {
		return TrackingDefaults{}, ErrConflict
	}
	s.actor, s.app, s.config = actor, app, v
	s.calls++
	return v, nil
}

func TestTrackingDefaultsRoute(t *testing.T) {
	store := &trackingDefaultsTestStore{fakeStore: &fakeStore{}}
	runtime := &RuntimeInfo{Features: RuntimeFeatures{Tracking: true}, Tracking: &RuntimeTracking{Origin: "https://t.example", LinkEventLimit: 5}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, Runtime: runtime}
	path := "/api/v1/apps/" + strings.Repeat("a", 32) + "/tracking-defaults"
	if w := send(h, "PUT", path, `{"opens":true,"clicks":true}`, "bad"); w.Code != 401 {
		t.Fatal("unauthenticated update accepted", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "PUT", path, `{"opens":true,"clicks":true}`, "valid"); w.Code != 403 || store.calls != 0 {
		t.Fatal("non-admin update accepted", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	if w := send(h, "GET", path, "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"opens":false`) || !strings.Contains(w.Body.String(), `"clicks":false`) {
		t.Fatal("default is not off", w.Code, w.Body.String())
	}
	for _, body := range []string{``, `{}`, `{"opens":true}`, `{"clicks":true}`, `{"opens":null,"clicks":true}`, `{"opens":"yes","clicks":true}`, `{"opens":true,"clicks":true,"extra":1}`} {
		if w := send(h, "PUT", path, body, "valid"); w.Code != 400 || store.calls != 0 {
			t.Fatal("invalid body accepted", body, w.Code)
		}
	}
	if w := send(h, "PUT", path, `{"opens":false,"clicks":true}`, "valid"); w.Code != 200 || store.actor != "operator-1" || store.config != (TrackingDefaults{Clicks: true}) || !strings.Contains(w.Body.String(), `"clicks":true`) {
		t.Fatal("update failed", w.Code, w.Body.String())
	}
	if w := send(h, "GET", path, "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"clicks":true`) {
		t.Fatal("read failed")
	}
	if w := send(h, "DELETE", path, "", "valid"); w.Code != 405 {
		t.Fatal("DELETE", w.Code)
	}
}

func TestTrackingDefaultsNeedTrackingToEnable(t *testing.T) {
	for name, runtime := range map[string]*RuntimeInfo{
		"no runtime info":  nil,
		"tracking off":     {Features: RuntimeFeatures{Submission: true}},
		"no tracking info": {Features: RuntimeFeatures{Tracking: true}},
	} {
		t.Run(name, func(t *testing.T) {
			store := &trackingDefaultsTestStore{fakeStore: &fakeStore{}, config: TrackingDefaults{Clicks: true}}
			h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, Runtime: runtime}
			path := "/api/v1/apps/" + strings.Repeat("a", 32) + "/tracking-defaults"
			if w := send(h, "PUT", path, `{"opens":true,"clicks":true}`, "valid"); w.Code != 409 || store.calls != 0 {
				t.Fatal("enabling without tracking", w.Code)
			}
			// Turning one off, and resending an unchanged on, are allowed.
			if w := send(h, "PUT", path, `{"opens":false,"clicks":true}`, "valid"); w.Code != 200 {
				t.Fatal("unchanged value refused", w.Code)
			}
			if w := send(h, "PUT", path, `{"opens":false,"clicks":false}`, "valid"); w.Code != 200 || store.config != (TrackingDefaults{}) {
				t.Fatal("disable refused", w.Code)
			}
		})
	}
}

func TestTrackingDefaultsUnavailableWithoutStore(t *testing.T) {
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}}
	path := "/api/v1/apps/" + strings.Repeat("a", 32) + "/tracking-defaults"
	if w := send(h, "GET", path, "", "valid"); w.Code != 503 {
		t.Fatal("expected 503", w.Code)
	}
}
