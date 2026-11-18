package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

type trackingQuotaFake struct {
	*fakeStore
	quota TrackingQuota
}

func (s *trackingQuotaFake) GetTrackingQuota(_ context.Context, app string) (TrackingQuota, error) {
	s.calls++
	s.app = app
	return s.quota, nil
}

func (s *trackingQuotaFake) SetTrackingQuota(_ context.Context, actor, app string, limit int) (TrackingQuota, error) {
	s.calls++
	s.actor, s.app, s.quota.DailyLimit = actor, app, limit
	return s.quota, nil
}

func TestTrackingQuotaRequiresAdminAndValidBound(t *testing.T) {
	store := &trackingQuotaFake{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
	app := strings.Repeat("a", 32)
	path := "/api/v1/apps/" + app + "/tracking-quota"
	if got := send(h, http.MethodGet, path, "", ""); got.Code != http.StatusUnauthorized || store.calls != 0 {
		t.Fatal("unauthenticated quota read")
	}
	h.Authenticator = fakeAuth{admin: false}
	if got := send(h, http.MethodPut, path, `{"daily_limit":1}`, "valid"); got.Code != http.StatusForbidden || store.calls != 0 {
		t.Fatal("non-admin quota update")
	}
	h.Authenticator = fakeAuth{admin: true}
	for _, body := range []string{`{}`, `{"daily_limit":-1}`, `{"daily_limit":1000001}`, `{"daily_limit":3,"extra":1}`} {
		if got := send(h, http.MethodPut, path, body, "valid"); got.Code != http.StatusBadRequest || store.calls != 0 {
			t.Fatal("invalid quota accepted", body, got.Code)
		}
	}
	if got := send(h, http.MethodPut, path, `{"daily_limit":500}`, "valid"); got.Code != http.StatusOK || store.actor != "operator-1" || store.app != app || store.quota.DailyLimit != 500 {
		t.Fatal("quota update failed", got.Code)
	}
	if got := send(h, http.MethodGet, path, "", "valid"); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"daily_limit":500`) || got.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("quota read failed", got.Code)
	}
	if got := send(&Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}}, http.MethodGet, path, "", "valid"); got.Code != http.StatusServiceUnavailable {
		t.Fatal("missing quota store did not fail closed")
	}
}
