package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/submission"
)

type fakeSubmitter struct {
	receipt submission.Receipt
	err     error
	actor   string
	appID   string
	body    []byte
	calls   int
}

func (f *fakeSubmitter) SubmitTestMessage(_ context.Context, actor, appID string, body []byte) (submission.Receipt, error) {
	f.calls++
	f.actor, f.appID, f.body = actor, appID, body
	return f.receipt, f.err
}

func TestTestMessageRoute(t *testing.T) {
	app, id := strings.Repeat("a", 32), strings.Repeat("c", 32)
	path := "/api/v1/apps/" + app + "/messages/test"
	accepted := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	submitter := &fakeSubmitter{receipt: submission.Receipt{ID: id, AcceptedAt: accepted}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, TestMessages: submitter}
	const body = `{"from":"a@example.com","to":["b@example.net"],"subject":"s","text":"t"}`

	got := send(h, "POST", path, body, "valid")
	if got.Code != http.StatusCreated || got.Header().Get("Location") != "/api/v1/apps/"+app+"/messages/"+id {
		t.Fatalf("status %d: %s", got.Code, got.Body.String())
	}
	var response SubmittedMessage
	if err := json.Unmarshal(got.Body.Bytes(), &response); err != nil || response.ID != id || !response.AcceptedAt.Equal(accepted) {
		t.Fatalf("response: %s", got.Body.String())
	}
	if submitter.actor != "operator-1" || submitter.appID != app || string(submitter.body) != body {
		t.Fatalf("submitter got actor=%q app=%q", submitter.actor, submitter.appID)
	}
}

func TestTestMessageRouteRejections(t *testing.T) {
	app := strings.Repeat("a", 32)
	path := "/api/v1/apps/" + app + "/messages/test"
	submitter := &fakeSubmitter{receipt: submission.Receipt{ID: strings.Repeat("c", 32), AcceptedAt: time.Now()}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, TestMessages: submitter}
	if got := send(h, "GET", path, "", "valid"); got.Code != 405 || got.Header().Get("Allow") != "POST" {
		t.Fatal("GET accepted", got.Code)
	}
	if got := send(h, "POST", path+"?x=1", `{}`, "valid"); got.Code != 400 {
		t.Fatal("query string accepted", got.Code)
	}
	if got := send(h, "POST", path, strings.Repeat(" ", 64<<10+1), "valid"); got.Code != 400 {
		t.Fatal("oversized body accepted", got.Code)
	}
	if got := send(h, "POST", path, `{}`, ""); got.Code != 401 {
		t.Fatal("anonymous request accepted", got.Code)
	}
	nonAdmin := &Handler{Authenticator: fakeAuth{admin: false}, Store: &fakeStore{}, TestMessages: submitter}
	if got := send(nonAdmin, "POST", path, `{}`, "valid"); got.Code != 403 {
		t.Fatal("non-admin submitted", got.Code)
	}
	if submitter.calls != 0 {
		t.Fatal("a rejected request reached the submitter")
	}
	// The body limit is inclusive.
	if got := send(h, "POST", path, strings.Repeat(" ", 64<<10), "valid"); got.Code != http.StatusCreated || submitter.calls != 1 {
		t.Fatal("body at the limit refused", got.Code)
	}
	none := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}}
	if got := send(none, "POST", path, `{}`, "valid"); got.Code != 503 {
		t.Fatal("missing submitter not 503", got.Code)
	}
	submitter.receipt = submission.Receipt{}
	if got := send(h, "POST", path, `{}`, "valid"); got.Code != 503 {
		t.Fatal("empty receipt not 503", got.Code)
	}
}

func TestSubmissionFailureTable(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{submission.ErrInvalidRequest, 400, "Bad Request"},
		{submission.ErrInvalidKey, 401, "Unauthorized"},
		{ErrNotFound, 404, "Not Found"},
		{submission.ErrAppPaused, 409, "app_paused"},
		{submission.ErrIdempotencyConflict, 409, "idempotency_conflict"},
		{submission.ErrInvalidMessage, 422, "message_rejected"},
		{submission.ErrQuotaExceeded, 429, "recipient_quota_exceeded"},
		{submission.ErrRateLimited, 429, "submission_rate_exceeded"},
		{submission.ErrBacklogExceeded, 429, "pending_limit_exceeded"},
		{submission.ErrUnavailable, 503, "Service Unavailable"},
		{errors.New("database"), 503, "Service Unavailable"},
	}
	app := strings.Repeat("a", 32)
	for _, tc := range tests {
		status, code := SubmissionFailure(tc.err)
		if status != tc.status || code != tc.code {
			t.Fatalf("%v: %d %s", tc.err, status, code)
		}
		h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, TestMessages: &fakeSubmitter{err: tc.err}}
		got := send(h, "POST", "/api/v1/apps/"+app+"/messages/test", `{}`, "valid")
		var body map[string]string
		if got.Code != tc.status || json.Unmarshal(got.Body.Bytes(), &body) != nil || body["error"] != tc.code || len(body) != 1 {
			t.Fatalf("%v: %d %s", tc.err, got.Code, got.Body.String())
		}
	}
}
