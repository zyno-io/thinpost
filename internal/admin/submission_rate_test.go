package admin

import (
	"context"
	"strings"
	"testing"
)

type submissionRateTestStore struct {
	*fakeStore
	config SubmissionRate
}

func (s *submissionRateTestStore) GetSubmissionRate(_ context.Context, app string) (SubmissionRate, error) {
	s.app = app
	return s.config, nil
}
func (s *submissionRateTestStore) SetSubmissionRate(_ context.Context, actor, app string, rate, burst int) (SubmissionRate, error) {
	s.actor, s.app = actor, app
	s.config = SubmissionRate{PerMinute: rate, Burst: burst}
	return s.config, nil
}

func TestSubmissionRateRequiresAdminAndValidPair(t *testing.T) {
	store := &submissionRateTestStore{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
	path := "/api/v1/apps/" + strings.Repeat("a", 32) + "/submission-rate"
	if w := send(h, "PUT", path, `{"per_minute":10,"burst":5}`, "bad"); w.Code != 401 {
		t.Fatal("unauthenticated update accepted")
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "PUT", path, `{"per_minute":10,"burst":5}`, "valid"); w.Code != 403 {
		t.Fatal("non-admin update accepted")
	}
	h.Authenticator = fakeAuth{admin: true}
	for _, body := range []string{`{}`, `{"per_minute":0,"burst":1}`, `{"per_minute":1,"burst":0}`, `{"per_minute":60001,"burst":1}`, `{"per_minute":1,"burst":10001}`, `{"per_minute":1,"burst":1,"extra":1}`} {
		if w := send(h, "PUT", path, body, "valid"); w.Code != 400 {
			t.Fatal("invalid configuration accepted", body, w.Code)
		}
	}
	if w := send(h, "PUT", path, `{"per_minute":10,"burst":5}`, "valid"); w.Code != 200 || store.actor != "operator-1" || store.config != (SubmissionRate{PerMinute: 10, Burst: 5}) {
		t.Fatal("rate update failed")
	}
	if w := send(h, "GET", path, "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"per_minute":10`) {
		t.Fatal("rate read failed")
	}
	if w := send(h, "PUT", path, `{"per_minute":0,"burst":0}`, "valid"); w.Code != 200 {
		t.Fatal("disable rejected")
	}
}
