package admin

import (
	"context"
	"strings"
	"testing"
)

type quotaTestStore struct {
	*fakeStore
	quota RecipientQuota
}

func (s *quotaTestStore) GetRecipientQuota(_ context.Context, app string) (RecipientQuota, error) {
	s.app = app
	return s.quota, nil
}
func (s *quotaTestStore) SetRecipientQuota(_ context.Context, actor, app string, limit int) (RecipientQuota, error) {
	s.actor, s.app = actor, app
	s.quota.DailyLimit = limit
	return s.quota, nil
}

func TestRecipientQuotaRequiresAdminAndValidLimit(t *testing.T) {
	store := &quotaTestStore{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
	path := "/api/v1/apps/" + strings.Repeat("a", 32) + "/recipient-quota"
	if w := send(h, "PUT", path, `{"daily_limit":10}`, "bad"); w.Code != 401 {
		t.Fatal("unauthenticated update accepted")
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "PUT", path, `{"daily_limit":10}`, "valid"); w.Code != 403 {
		t.Fatal("non-admin update accepted")
	}
	h.Authenticator = fakeAuth{admin: true}
	for _, body := range []string{`{}`, `{"daily_limit":-1}`, `{"daily_limit":1000001}`, `{"daily_limit":1,"extra":true}`} {
		if w := send(h, "PUT", path, body, "valid"); w.Code != 400 {
			t.Fatal("invalid quota accepted", body, w.Code)
		}
	}
	if w := send(h, "PUT", path, `{"daily_limit":10}`, "valid"); w.Code != 200 || store.actor != "operator-1" || store.quota.DailyLimit != 10 {
		t.Fatal("quota update failed")
	}
	if w := send(h, "GET", path, "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"daily_limit":10`) {
		t.Fatal("quota read failed")
	}
}
