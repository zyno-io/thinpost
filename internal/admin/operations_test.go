package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type operationsTestStore struct {
	*fakeStore
	cursor string
	limit  int
	update RetryPolicyUpdate
	err    error
}

func (s *operationsTestStore) Operations(context.Context) (Operations, error) {
	s.calls++
	return Operations{RetryPolicy: RetryPolicy{MaxAttempts: 128, Revision: 1}}, s.err
}
func (s *operationsTestStore) Attention(_ context.Context, cursor string, limit int) (AttentionPage, error) {
	s.calls++
	s.cursor, s.limit = cursor, limit
	return AttentionPage{}, s.err
}
func (s *operationsTestStore) SetRetryPolicy(_ context.Context, actor string, update RetryPolicyUpdate) (RetryPolicy, error) {
	s.calls++
	s.actor, s.update = actor, update
	return RetryPolicy{MaxAttempts: update.MaxAttempts, Revision: update.ExpectedRevision + 1}, s.err
}

func TestRuntimeRoute(t *testing.T) {
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, Runtime: &RuntimeInfo{
		Features: RuntimeFeatures{Submission: true, Tracking: true}, Tracking: &RuntimeTracking{Origin: "https://t.example", LinkEventLimit: 10}}}
	if w := send(h, "GET", "/api/v1/runtime", "", ""); w.Code != 401 {
		t.Fatal("unauthenticated", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "GET", "/api/v1/runtime", "", "valid"); w.Code != 403 {
		t.Fatal("non-admin", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	w := send(h, "GET", "/api/v1/runtime", "", "valid")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"features":{"submission":true,"tracking":true,"smtp_submission":false}`) || !strings.Contains(w.Body.String(), `"link_event_limit":10`) {
		t.Fatal("runtime", w.Code, w.Body.String())
	}
	if w = send(h, "POST", "/api/v1/runtime", "", "valid"); w.Code != 405 {
		t.Fatal("method", w.Code)
	}
	h.Runtime = &RuntimeInfo{}
	if w = send(h, "GET", "/api/v1/runtime", "", "valid"); strings.Contains(w.Body.String(), `"tracking":{`) {
		t.Fatal("disabled tracking reports a deployment", w.Body.String())
	}
	h.Runtime = nil
	if w = send(h, "GET", "/api/v1/runtime", "", "valid"); w.Code != 503 {
		t.Fatal("unconfigured runtime", w.Code)
	}
}

func TestOperationsRoutes(t *testing.T) {
	store := &operationsTestStore{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, Operations: store}
	for _, path := range []string{"/api/v1/operations", "/api/v1/operations/attention"} {
		if w := send(h, "GET", path, "", ""); w.Code != 401 || store.calls != 0 {
			t.Fatal("unauthenticated", path)
		}
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "GET", "/api/v1/operations", "", "valid"); w.Code != 403 || store.calls != 0 {
		t.Fatal("non-admin")
	}
	h.Authenticator = fakeAuth{admin: true}
	if w := send(h, "GET", "/api/v1/operations", "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"workers":[]`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("snapshot", w.Code, w.Body.String())
	}
	if w := send(h, "GET", "/api/v1/operations?x=1", "", "valid"); w.Code != 503 {
		t.Fatal("snapshot accepted a query", w.Code)
	}
	if w := send(h, "GET", "/api/v1/operations/attention?limit=10&cursor=abc", "", "valid"); w.Code != 200 || store.limit != 10 || store.cursor != "abc" || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatal("attention", w.Code, w.Body.String())
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=1&limit=2", "?q=a@example.com", "?cursor=" + strings.Repeat("x", 513), "?limit=x"} {
		if w := send(h, "GET", "/api/v1/operations/attention"+q, "", "valid"); w.Code != 400 {
			t.Fatal("accepted attention query", q, w.Code)
		}
	}
	store.err = errors.New("db down")
	if w := send(h, "GET", "/api/v1/operations", "", "valid"); w.Code != 503 || strings.Contains(w.Body.String(), "db down") {
		t.Fatal("store error leaked", w.Code, w.Body.String())
	}
	if w := send(&Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}}, "GET", "/api/v1/operations", "", "valid"); w.Code != 503 {
		t.Fatal("missing store did not fail closed", w.Code)
	}
}

func TestRetryPolicyRoute(t *testing.T) {
	store := &operationsTestStore{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, Operations: store}
	const ok = `{"max_attempts":20,"initial_delay_ms":60000,"max_delay_ms":3600000,"max_age_ms":86400000,"expected_revision":3}`
	if w := send(h, "PUT", "/api/v1/retry-policy", ok, ""); w.Code != 401 || store.calls != 0 {
		t.Fatal("unauthenticated")
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "PUT", "/api/v1/retry-policy", ok, "valid"); w.Code != 403 || store.calls != 0 {
		t.Fatal("non-admin")
	}
	h.Authenticator = fakeAuth{admin: true}
	for _, body := range []string{
		`{}`,
		`{"max_attempts":0,"initial_delay_ms":60000,"max_delay_ms":3600000,"max_age_ms":86400000,"expected_revision":3}`,
		`{"max_attempts":1001,"initial_delay_ms":60000,"max_delay_ms":3600000,"max_age_ms":86400000,"expected_revision":3}`,
		`{"max_attempts":20,"initial_delay_ms":59999,"max_delay_ms":3600000,"max_age_ms":86400000,"expected_revision":3}`,
		`{"max_attempts":20,"initial_delay_ms":86400001,"max_delay_ms":86400001,"max_age_ms":86400001,"expected_revision":3}`,
		`{"max_attempts":20,"initial_delay_ms":120000,"max_delay_ms":60000,"max_age_ms":86400000,"expected_revision":3}`,
		`{"max_attempts":20,"initial_delay_ms":60000,"max_delay_ms":3600000,"max_age_ms":3599999,"expected_revision":3}`,
		`{"max_attempts":20,"initial_delay_ms":60000,"max_delay_ms":3600000,"max_age_ms":2592000001,"expected_revision":3}`,
		`{"max_attempts":20,"initial_delay_ms":60000,"max_delay_ms":3600000,"max_age_ms":86400000,"expected_revision":0}`,
		`{"max_attempts":20,"initial_delay_ms":60000,"max_delay_ms":3600000,"max_age_ms":86400000}`,
		`{"max_attempts":20,"initial_delay_ms":60000,"max_delay_ms":3600000,"max_age_ms":86400000,"expected_revision":3,"extra":1}`,
	} {
		if w := send(h, "PUT", "/api/v1/retry-policy", body, "valid"); w.Code != 400 || store.calls != 0 {
			t.Fatal("invalid policy accepted", body, w.Code)
		}
	}
	if w := send(h, "GET", "/api/v1/retry-policy", "", "valid"); w.Code != 405 {
		t.Fatal("method", w.Code)
	}
	if w := send(h, "PUT", "/api/v1/retry-policy", ok, "valid"); w.Code != 200 || store.actor != "operator-1" || store.update.ExpectedRevision != 3 || store.update.MaxAttempts != 20 || !strings.Contains(w.Body.String(), `"revision":4`) {
		t.Fatal("update", w.Code, w.Body.String())
	}
	store.err = ErrConflict
	if w := send(h, "PUT", "/api/v1/retry-policy", ok, "valid"); w.Code != 409 {
		t.Fatal("stale revision", w.Code)
	}
}
