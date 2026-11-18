package admin

import (
	"context"
	"strings"
	"testing"
)

type pendingQuotaTestStore struct {
	*fakeStore
	quota PendingQuota
}

func (s *pendingQuotaTestStore) GetPendingQuota(_ context.Context, app string) (PendingQuota, error) {
	s.app = app
	return s.quota, nil
}
func (s *pendingQuotaTestStore) SetPendingQuota(_ context.Context, actor, app string, limit int) (PendingQuota, error) {
	s.actor, s.app = actor, app
	s.quota.Limit = limit
	return s.quota, nil
}

func TestPendingQuotaRequiresAdminAndValidLimit(t *testing.T) {
	store := &pendingQuotaTestStore{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
	path := "/api/v1/apps/" + strings.Repeat("a", 32) + "/pending-quota"
	if w := send(h, "PUT", path, `{"limit":10}`, "bad"); w.Code != 401 {
		t.Fatal("unauthenticated update accepted")
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "PUT", path, `{"limit":10}`, "valid"); w.Code != 403 {
		t.Fatal("non-admin update accepted")
	}
	h.Authenticator = fakeAuth{admin: true}
	for _, body := range []string{`{}`, `{"limit":-1}`, `{"limit":100001}`, `{"limit":1,"extra":true}`} {
		if w := send(h, "PUT", path, body, "valid"); w.Code != 400 {
			t.Fatal("invalid quota accepted", w.Code)
		}
	}
	if w := send(h, "PUT", path, `{"limit":10}`, "valid"); w.Code != 200 || store.actor != "operator-1" || store.quota.Limit != 10 {
		t.Fatal("quota update failed")
	}
	if w := send(h, "GET", path, "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"limit":10`) {
		t.Fatal("quota read failed")
	}
}
