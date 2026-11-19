package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/submission"
)

// fakeHolds records hold actions and answers with err. It enforces app
// ownership the way the repository does.
type fakeHolds struct {
	calls               []string
	app, key, message   string
	err                 error
	releaseErr, cancErr error
}

func (f *fakeHolds) act(action, app, key, msg string, err error) error {
	f.calls = append(f.calls, action)
	f.app, f.key, f.message = app, key, msg
	if ownership[msg] != app {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return f.err
}
func (f *fakeHolds) ReleaseHeldMessage(_ context.Context, app, key, msg string) error {
	return f.act("release", app, key, msg, f.releaseErr)
}
func (f *fakeHolds) CancelHeldMessage(_ context.Context, app, key, msg string) error {
	return f.act("cancel", app, key, msg, f.cancErr)
}

// heldReads reports a released hold so the response body can be checked.
type heldReads struct{ scopedReads }

func (s *heldReads) GetMessage(_ context.Context, actor, app, msg string) (MessageDetail, error) {
	at := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	detail := MessageDetail{Message: MessageSummary{ID: msg, State: "queued", Status: "queued"}, Hold: &MessageHold{ExpiresAt: at.Add(MaxHoldForTest), ResolvedAt: &at, Resolution: HoldReleased}}
	return detail, s.own(actor, app, msg)
}

const MaxHoldForTest = 7 * 24 * time.Hour

func newAppHold() (*AppReadHandler, *fakeHolds, *heldReads) {
	reads := &heldReads{}
	holds := &fakeHolds{}
	return &AppReadHandler{Service: &appKeys{}, Store: reads, ContentReader: reads, Holds: holds}, holds, reads
}

func TestAppHoldActionsAreScopedPostAndReturnTheMessage(t *testing.T) {
	for _, action := range []string{"release", "cancel"} {
		h, holds, reads := newAppHold()
		path := "/app/v1/messages/" + msgA + "/" + action
		got := appGet(h, "POST", path, "keyA")
		if got.Code != 200 || len(holds.calls) != 1 || holds.calls[0] != action || holds.app != appA || holds.key != "k1" || holds.message != msgA {
			t.Fatalf("%s: %d %+v", action, got.Code, holds)
		}
		if reads.actor != "key:k1" || reads.app != appA {
			t.Fatal(action, "detail read not scoped to the key", reads.actor, reads.app)
		}
		body := got.Body.String()
		if !strings.Contains(body, `"deliveries":[]`) || !strings.Contains(body, `"hold":{"expires_at":"2026-11-25T10:00:00Z","resolved_at":"2026-11-18T10:00:00Z","resolution":"released"}`) {
			t.Fatal(action, "body", body)
		}
		if got.Header().Get("Cache-Control") != "no-store" || got.Header().Get("Content-Type") != "application/json" {
			t.Fatal(action, "headers")
		}
		// A provider rendering of the ID works as for reads.
		if got = appGet(h, "POST", "/app/v1/messages/01234567-89ab-cdef-0123-456789abcdef/"+action, "keyA"); got.Code != 200 || holds.message != msgA {
			t.Fatal(action, "UUID rendering", got.Code)
		}
		// Another app's message is indistinguishable from an absent one.
		holds.calls = nil
		if got = appGet(h, "POST", "/app/v1/messages/"+msgB+"/"+action, "keyA"); got.Code != 404 || holds.app != appA || strings.Contains(got.Body.String(), msgB) {
			t.Fatal(action, "cross-app", got.Code)
		}
		// Only POST is served, and no credential is checked before the method.
		for _, method := range []string{"GET", "PUT", "DELETE", "PATCH"} {
			holds.calls = nil
			if got = appGet(h, method, path, "keyA"); got.Code != 405 || got.Header().Get("Allow") != "POST" || len(holds.calls) != 0 {
				t.Fatal(action, "method", method, got.Code)
			}
		}
		// Reads stay GET only.
		if got = appGet(h, "POST", "/app/v1/messages/"+msgA, "keyA"); got.Code != 405 || got.Header().Get("Allow") != "GET" {
			t.Fatal("read accepted POST", got.Code)
		}
		holds.calls = nil
		for _, bad := range []string{path + "?x=1", path + "/", path + "/now", "/app/v1/messages/" + msgA + "/Release"} {
			if got = appGet(h, "POST", bad, "keyA"); got.Code != 400 && got.Code != 404 || len(holds.calls) != 0 {
				t.Fatal(action, "path accepted", bad, got.Code)
			}
		}
		for _, key := range []string{"", "nope", "revoked"} {
			if got = appGet(h, "POST", path, key); got.Code != 401 || len(holds.calls) != 0 {
				t.Fatal(action, "key accepted", key, got.Code)
			}
		}
	}
}

func TestAppHoldConflictsAndFailures(t *testing.T) {
	h, holds, _ := newAppHold()
	path := "/app/v1/messages/" + msgA + "/release"
	for _, reason := range []HoldConflict{HoldNotHeld, HoldWasReleased, HoldWasCancelled, HoldHasExpired, HoldInRecovery} {
		holds.err = reason
		got := appGet(h, "POST", path, "keyA")
		if got.Code != 409 || got.Body.String() != `{"error":"`+string(reason)+`"}`+"\n" {
			t.Fatal("conflict", reason, got.Code, got.Body.String())
		}
		if !errors.Is(reason, ErrConflict) {
			t.Fatal("HoldConflict must match ErrConflict")
		}
	}
	for err, code := range map[error]int{
		submission.ErrInvalidKey: http.StatusUnauthorized,
		ErrNotFound:              http.StatusNotFound,
		errors.New("database"):   http.StatusServiceUnavailable,
	} {
		holds.err = err
		if got := appGet(h, "POST", path, "keyA"); got.Code != code {
			t.Fatal(err, got.Code)
		}
	}
	holds.err = nil
	h.Holds = nil
	if got := appGet(h, "POST", path, "keyA"); got.Code != 503 {
		t.Fatal("missing hold store must fail closed", got.Code)
	}
}
