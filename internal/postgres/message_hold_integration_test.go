package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/controlapp"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/httpapi"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/submission"
)

// holdFixture adds held-message helpers to the webhook fixture: an app with a
// verified sender domain, an active key and a native and a Resend endpoint.
type holdFixture struct {
	*webhookFixture
	app, key, native, resend string
}

func newHoldFixture(t *testing.T) *holdFixture {
	f := &holdFixture{webhookFixture: newWebhookFixture(t, "hold_")}
	f.repo.Objects = objectVerifierFunc(func(context.Context, string) error { return nil })
	f.app = f.app_("hold")
	f.key = f.keyFor(f.app)
	f.native = f.createFormat(f.app, "thinpost", "https://hooks.example.com/native", nil, true, nil).ID
	f.resend = f.createFormat(f.app, "resend", "https://hooks.example.com/resend", nil, true, nil).ID
	return f
}

func (f *holdFixture) app_(name string) string {
	app := f.webhookFixture.app(name)
	f.exec(`UPDATE apps SET pending_message_limit=1000,recipient_daily_limit=1000 WHERE id=$1`, app)
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, app)
	return app
}

func (f *holdFixture) keyFor(app string) string {
	key := f.id()
	digest := sha256.Sum256([]byte(key))
	f.exec(`INSERT INTO api_keys(id,digest,app_id) VALUES ($1,$2,$3)`, key, digest[:], app)
	return key
}

// submit commits one single-recipient message, held or not, as acceptance would.
func (f *holdFixture) submit(app, key string, hold bool) (string, error) {
	f.t.Helper()
	record := withMetadata(acceptance.Record{AppID: app, KeyID: key, ObjectKey: fixtureObjectKey("submissions", f.id()), Adapter: "resend", From: "sender@example.com", RecipientCount: 1, Hold: hold})
	receipt, err := f.repo.Commit(f.ctx, record)
	return receipt.ID, err
}

func (f *holdFixture) mustSubmit(hold bool) string {
	f.t.Helper()
	id, err := f.submit(f.app, f.key, hold)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *holdFixture) state(message string) (state string, pending bool) {
	f.t.Helper()
	if err := f.owner.QueryRowContext(f.ctx, `SELECT state,pending FROM messages WHERE id=$1`, message).Scan(&state, &pending); err != nil {
		f.t.Fatal(err)
	}
	return state, pending
}

// events counts the message's events with the outcome, and how many endpoint
// deliveries were queued for them per format.
func (f *holdFixture) events(message, outcome string) (events, native, resend int) {
	f.t.Helper()
	if err := f.owner.QueryRowContext(f.ctx, `SELECT count(DISTINCT e.id),
		count(w.event_id) FILTER (WHERE w.subscription_id=$3),count(w.event_id) FILTER (WHERE w.subscription_id=$4)
		FROM webhook_events e LEFT JOIN webhook_deliveries w ON w.event_id=e.id
		WHERE e.message_id=$1 AND e.outcome=$2 AND e.delivery_id IS NULL AND e.smtp_code=0`, message, outcome, f.native, f.resend).Scan(&events, &native, &resend); err != nil {
		f.t.Fatal(err)
	}
	return events, native, resend
}

func (f *holdFixture) audits(message, action string) int {
	f.t.Helper()
	var n int
	if err := f.owner.QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE resource_id=$1 AND action=$2 AND actor_id=$3`, message, action, "key:"+f.key).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestMessageHoldPostgres(t *testing.T) {
	f := newHoldFixture(t)
	ctx := f.ctx

	// Hold: stored, pending and announced, but never claimed for preparation,
	// so it can have no deliveries and nothing to dispatch.
	held := f.mustSubmit(true)
	var accepted, expires time.Time
	if err := f.owner.QueryRowContext(ctx, `SELECT accepted_at,hold_expires_at FROM messages WHERE id=$1 AND hold_resolved_at IS NULL`, held).Scan(&accepted, &expires); err != nil || !expires.Equal(accepted.Add(submission.MaxHold)) {
		t.Fatal("hold expiry not recorded", accepted, expires, err)
	}
	if state, pending := f.state(held); state != "on_hold" || !pending {
		t.Fatal("held message state", state, pending)
	}
	if events, native, resend := f.events(held, "held"); events != 1 || native != 1 || resend != 0 {
		t.Fatal("held event and fan-out", events, native, resend)
	}
	if _, err := f.repo.ClaimPreparation(ctx, f.id(), 5*time.Second); !errors.Is(err, preparation.ErrNoWork) {
		t.Fatal("held message claimed for preparation", err)
	}
	detail, err := f.repo.GetMessage(ctx, "operator", f.app, held)
	if err != nil || detail.Message.Status != "on_hold" || detail.Hold == nil || !detail.Hold.ExpiresAt.Equal(expires) || detail.Hold.Resolution != "" || detail.Hold.ResolvedAt != nil {
		t.Fatalf("held detail %+v %v", detail.Hold, err)
	}

	// Cross-app: another app's key sees no such message; a key of another
	// app cannot act for this one; a paused app or revoked key cannot act.
	other := f.app_("other")
	otherKey := f.keyFor(other)
	if err = f.repo.ReleaseHeldMessage(ctx, other, otherKey, held); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("cross-app release", err)
	}
	if err = f.repo.CancelHeldMessage(ctx, other, otherKey, held); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("cross-app cancel", err)
	}
	if err = f.repo.ReleaseHeldMessage(ctx, f.app, otherKey, held); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("another app's key accepted", err)
	}
	f.exec(`UPDATE apps SET active=false WHERE id=$1`, f.app)
	if err = f.repo.ReleaseHeldMessage(ctx, f.app, f.key, held); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("paused app released", err)
	}
	f.exec(`UPDATE apps SET active=true WHERE id=$1`, f.app)
	revoked := f.keyFor(f.app)
	f.exec(`UPDATE api_keys SET active=false WHERE id=$1`, revoked)
	if err = f.repo.ReleaseHeldMessage(ctx, f.app, revoked, held); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("revoked key released", err)
	}
	if state, _ := f.state(held); state != "on_hold" {
		t.Fatal("refused actions changed the message", state)
	}

	// Concurrent releases: every caller succeeds, exactly one transition,
	// event and audit record.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f.repo.ReleaseHeldMessage(ctx, f.app, f.key, held)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("concurrent release", err)
		}
	}
	if state, pending := f.state(held); state != "queued" || !pending {
		t.Fatal("released state", state, pending)
	}
	if events, native, _ := f.events(held, "released"); events != 1 || native != 1 || f.audits(held, "message.hold.released") != 1 {
		t.Fatal("release not recorded exactly once", events, native)
	}
	var resolved time.Time
	if err = f.owner.QueryRowContext(ctx, `SELECT hold_resolved_at FROM messages WHERE id=$1`, held).Scan(&resolved); err != nil {
		t.Fatal(err)
	}

	// Release: exactly one preparation claim, dated at the release, and one
	// delivery for the edge.
	claim, err := f.repo.ClaimPreparation(ctx, f.id(), 5*time.Second)
	if err != nil || claim.MessageID != held || !claim.Date.Equal(resolved) || !claim.AcceptedAt.Equal(accepted) {
		t.Fatal("released message not claimed once with its release date", claim.MessageID, claim.Date, resolved, err)
	}
	if _, err = f.repo.ClaimPreparation(ctx, f.id(), 5*time.Second); !errors.Is(err, preparation.ErrNoWork) {
		t.Fatal("released message claimed twice", err)
	}
	claim.SigningDomain = "example.com"
	deliveryID := f.id()
	if err = f.repo.CommitPreparation(ctx, claim, []preparation.Delivery{{ID: deliveryID, RecipientIndex: 0, ObjectKey: fixtureObjectKey("final", deliveryID)}}); err != nil {
		t.Fatal(err)
	}
	// A repeat release after preparation changes nothing.
	if err = f.repo.ReleaseHeldMessage(ctx, f.app, f.key, held); err != nil {
		t.Fatal("repeat release", err)
	}
	lease, err := f.repo.AcquireControllerLease(ctx, "worker", "controller", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := f.repo.ClaimDelivery(ctx, lease, f.id(), delivery.EdgeCapabilities{})
	if err != nil || sent.MessageID != held || sent.DeliveryID != deliveryID {
		t.Fatal("released message not dispatched", err)
	}
	if _, err = f.repo.ClaimDelivery(ctx, lease, f.id(), delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrNoWork) {
		t.Fatal("released message dispatched twice", err)
	}
	if events, _, _ := f.events(held, "released"); events != 1 || f.audits(held, "message.hold.released") != 1 {
		t.Fatal("repeat release recorded again", events)
	}
	if err = f.repo.CancelHeldMessage(ctx, f.app, f.key, held); !errors.Is(err, admin.HoldWasReleased) {
		t.Fatal("released message cancelled", err)
	}
	if detail, err = f.repo.GetMessage(ctx, "operator", f.app, held); err != nil || detail.Hold == nil || detail.Hold.Resolution != admin.HoldReleased || !detail.Hold.ResolvedAt.Equal(resolved) {
		t.Fatalf("released detail %+v %v", detail.Hold, err)
	}

	// Cancel: terminal, frees the pending slot, idempotent, blocks release.
	cancelled := f.mustSubmit(true)
	if err = f.repo.CancelHeldMessage(ctx, f.app, f.key, cancelled); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.CancelHeldMessage(ctx, f.app, f.key, cancelled); err != nil {
		t.Fatal("repeat cancel", err)
	}
	if err = f.repo.ReleaseHeldMessage(ctx, f.app, f.key, cancelled); !errors.Is(err, admin.HoldWasCancelled) || !errors.Is(err, admin.ErrConflict) {
		t.Fatal("release after cancel", err)
	}
	if state, pending := f.state(cancelled); state != "cancelled" || pending {
		t.Fatal("cancelled state", state, pending)
	}
	if events, native, resend := f.events(cancelled, "cancelled"); events != 1 || native != 1 || resend != 0 || f.audits(cancelled, "message.hold.cancelled") != 1 {
		t.Fatal("cancel not recorded exactly once", events, native, resend)
	}
	if detail, err = f.repo.GetMessage(ctx, "operator", f.app, cancelled); err != nil || detail.Message.Status != "cancelled" || detail.Hold.Resolution != admin.HoldCancelled {
		t.Fatalf("cancelled detail %+v %v", detail, err)
	}

	// Release racing cancel: exactly one wins; the other gets the conflict
	// that names the winner.
	for range 5 {
		raced := f.mustSubmit(true)
		var release, cancel error
		var race sync.WaitGroup
		race.Add(2)
		go func() { defer race.Done(); release = f.repo.ReleaseHeldMessage(ctx, f.app, f.key, raced) }()
		go func() { defer race.Done(); cancel = f.repo.CancelHeldMessage(ctx, f.app, f.key, raced) }()
		race.Wait()
		state, _ := f.state(raced)
		released, _, _ := f.events(raced, "released")
		gone, _, _ := f.events(raced, "cancelled")
		switch {
		case release == nil && errors.Is(cancel, admin.HoldWasReleased) && state == "queued" && released == 1 && gone == 0:
		case cancel == nil && errors.Is(release, admin.HoldWasCancelled) && state == "cancelled" && released == 0 && gone == 1:
		default:
			t.Fatal("release/cancel race", release, cancel, state, released, gone)
		}
	}

	// Expiry: a release after the deadline is refused and expires the message
	// even before the sweep; the sweep expires the rest.
	late := f.mustSubmit(true)
	swept := f.mustSubmit(true)
	f.exec(`UPDATE messages SET hold_expires_at=clock_timestamp()-interval '1 second' WHERE id IN ($1,$2)`, late, swept)
	if err = f.repo.ReleaseHeldMessage(ctx, f.app, f.key, late); !errors.Is(err, admin.HoldHasExpired) {
		t.Fatal("release after expiry", err)
	}
	if state, pending := f.state(late); state != "hold_expired" || pending {
		t.Fatal("late release did not expire", state, pending)
	}
	if n, err := f.repo.ExpireHeldMessages(ctx, 100); err != nil || n != 1 {
		t.Fatal("sweep", n, err)
	}
	if n, err := f.repo.ExpireHeldMessages(ctx, 100); err != nil || n != 0 {
		t.Fatal("repeat sweep", n, err)
	}
	for _, message := range []string{late, swept} {
		if state, pending := f.state(message); state != "hold_expired" || pending {
			t.Fatal("expired state", state, pending)
		}
		// Native and Resend (email.failed) endpoints both receive the expiry.
		if events, native, resend := f.events(message, "hold_expired"); events != 1 || native != 1 || resend != 1 {
			t.Fatal("expiry event", events, native, resend)
		}
		for _, act := range []func(context.Context, string, string, string) error{f.repo.ReleaseHeldMessage, f.repo.CancelHeldMessage} {
			if err = act(ctx, f.app, f.key, message); !errors.Is(err, admin.HoldHasExpired) {
				t.Fatal("action after expiry", err)
			}
		}
		if detail, err = f.repo.GetMessage(ctx, "operator", f.app, message); err != nil || detail.Message.Status != "expired" || detail.Hold.Resolution != admin.HoldExpired {
			t.Fatalf("expired detail %+v %v", detail, err)
		}
	}
	// Only the race winners that were released are prepared; a cancelled or
	// expired message never is.
	for {
		claimed, err := f.repo.ClaimPreparation(ctx, f.id(), 5*time.Second)
		if errors.Is(err, preparation.ErrNoWork) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if state, _ := f.state(claimed.MessageID); claimed.MessageID == cancelled || claimed.MessageID == late || claimed.MessageID == swept || state != "preparing" {
			t.Fatal("ended hold claimed", claimed.MessageID)
		}
		var released bool
		if err = f.owner.QueryRowContext(ctx, `SELECT hold_resolved_at IS NOT NULL FROM messages WHERE id=$1`, claimed.MessageID).Scan(&released); err != nil || !released {
			t.Fatal("unreleased message claimed", claimed.MessageID, err)
		}
	}

	// A message that was not submitted on hold cannot be released or cancelled.
	plain := f.mustSubmit(false)
	for _, act := range []func(context.Context, string, string, string) error{f.repo.ReleaseHeldMessage, f.repo.CancelHeldMessage} {
		if err = act(ctx, f.app, f.key, plain); !errors.Is(err, admin.HoldNotHeld) {
			t.Fatal("unheld message", err)
		}
	}
	// A held message quarantined by a recovery cannot be released.
	quarantined := f.mustSubmit(true)
	f.exec(`UPDATE messages SET state='recovery_hold' WHERE id=$1`, quarantined)
	if err = f.repo.ReleaseHeldMessage(ctx, f.app, f.key, quarantined); !errors.Is(err, admin.HoldInRecovery) {
		t.Fatal("quarantined release", err)
	}
	if _, err = f.repo.ExpireHeldMessages(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if state, _ := f.state(quarantined); state != "recovery_hold" {
		t.Fatal("sweep touched a quarantined message", state)
	}
}

func TestMessageHoldCountsTowardPendingLimit(t *testing.T) {
	f := newHoldFixture(t)
	f.exec(`UPDATE apps SET pending_message_limit=2 WHERE id=$1`, f.app)
	first := f.mustSubmit(true)
	f.mustSubmit(true)
	if _, err := f.submit(f.app, f.key, false); !errors.Is(err, submission.ErrBacklogExceeded) {
		t.Fatal("held messages not counted as pending", err)
	}
	if _, err := f.submit(f.app, f.key, true); !errors.Is(err, submission.ErrBacklogExceeded) {
		t.Fatal("held message over the pending limit", err)
	}
	ops, err := f.repo.Operations(f.ctx)
	if err != nil || ops.Messages.OnHold != 2 || ops.Messages.QueuedDue != 0 {
		t.Fatalf("operations %+v %v", ops.Messages, err)
	}
	// Release keeps the slot (the message is still to be sent); cancel frees it.
	if err = f.repo.ReleaseHeldMessage(f.ctx, f.app, f.key, first); err != nil {
		t.Fatal(err)
	}
	if _, err = f.submit(f.app, f.key, false); !errors.Is(err, submission.ErrBacklogExceeded) {
		t.Fatal("release freed a pending slot", err)
	}
	var second string
	if err = f.owner.QueryRowContext(f.ctx, `SELECT id FROM messages WHERE app_id=$1 AND state='on_hold'`, f.app).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.CancelHeldMessage(f.ctx, f.app, f.key, second); err != nil {
		t.Fatal(err)
	}
	if _, err = f.submit(f.app, f.key, false); err != nil {
		t.Fatal("cancel did not free the pending slot", err)
	}
}

func TestMessageHoldBatchAndRecoveryPostgres(t *testing.T) {
	f := newHoldFixture(t)
	hash := sha256.Sum256([]byte("held batch"))
	record := acceptance.BatchRecord{Record: acceptance.Record{AppID: f.app, KeyID: f.key, Adapter: "resend", IdempotencyKey: "held-batch", RequestHash: hash, Hold: true}, Items: []acceptance.BatchItem{
		withItemMetadata(acceptance.BatchItem{ObjectKey: fixtureObjectKey("submissions", f.id()), From: "sender@example.com", RecipientCount: 1}),
		withItemMetadata(acceptance.BatchItem{ObjectKey: fixtureObjectKey("submissions", f.id()), From: "sender@example.com", RecipientCount: 1}),
	}}
	receipt, err := f.repo.CommitBatch(f.ctx, record)
	if err != nil || len(receipt.IDs) != 2 {
		t.Fatal(err)
	}
	for _, id := range receipt.IDs {
		if state, pending := f.state(id); state != "on_hold" || !pending {
			t.Fatal("batch item not held", state)
		}
		if events, _, _ := f.events(id, "held"); events != 1 {
			t.Fatal("batch held event", events)
		}
	}
	if replay, err := f.repo.CommitBatch(f.ctx, record); err != nil || !replay.Replayed {
		t.Fatal("held batch replay", err)
	}
	if _, err = f.repo.ClaimPreparation(f.ctx, f.id(), 5*time.Second); !errors.Is(err, preparation.ErrNoWork) {
		t.Fatal("held batch claimed", err)
	}
	// A recovery quarantines held messages like queued ones: a restored
	// database cannot know whether a hold was released after the snapshot.
	tx, err := f.owner.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if pending, err := recoveryWorkPending(f.ctx, tx, "00000000000000000000000000000bbb"); err != nil || !pending {
		t.Fatal("held messages are not recovery work", pending, err)
	}
}

// A schema-41 controller's preparation claim selected exactly this predicate;
// it cannot see a held row, so an older dispatcher never prepares one.
func TestMessageHoldInvisibleToSchema41Claim(t *testing.T) {
	f := newHoldFixture(t)
	held := f.mustSubmit(true)
	var found int
	if err := f.owner.QueryRowContext(f.ctx, `SELECT count(*) FROM messages m JOIN apps app ON app.id=m.app_id
		WHERE app.active AND m.state='queued' AND m.prepare_next_at<=clock_timestamp() AND m.prepare_attempts<12 AND m.id=$1`, held).Scan(&found); err != nil || found != 0 {
		t.Fatal("schema-41 claim would see a held message", found, err)
	}
	// Its pending count (pending is true by default) includes the held row.
	var pending int
	if err := f.owner.QueryRowContext(f.ctx, `SELECT count(*) FROM messages WHERE app_id=$1 AND pending`, f.app).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("schema-41 pending count", pending, err)
	}
}

// TestMessageHoldHTTP drives the hold through the real routes: submit on hold
// with the Resend adapter, read it back, release it twice and cancel it, all
// with the app key.
func TestMessageHoldHTTP(t *testing.T) {
	f := newHoldFixture(t)
	objects := &batchMemoryObjects{}
	f.repo.Objects = objects
	f.exec(`UPDATE apps SET submission_rate_per_minute=60000,submission_burst=100,submission_tokens=6000000000 WHERE id=$1`, f.app)
	secret := "disposable-hold-http-key"
	digest := sha256.Sum256([]byte(secret))
	f.exec(`UPDATE api_keys SET digest=$1 WHERE id=$2`, digest[:], f.key)
	service, err := acceptance.New(f.repo, objects, bytes.Repeat([]byte{9}, 32), batchAllowedRecipients{})
	if err != nil {
		t.Fatal(err)
	}
	app := &admin.AppReadHandler{Service: service, Store: f.repo, Holds: f.repo}
	handler := controlapp.RouterWithRuntime(http.NotFoundHandler(), httpapi.New(service), nil, nil, controlapp.MailRuntime{App: app})
	call := func(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+secret)
		for name, value := range headers {
			r.Header.Set(name, value)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	sent := call("POST", "/emails", `{"from":"sender@example.com","to":"one@example.net","subject":"held","text":"body"}`, map[string]string{"X-Thinpost-Hold": "true", "Content-Type": "application/json"})
	if sent.Code != 200 || sent.Header().Get("X-Thinpost-Hold") != "held" {
		t.Fatal("held submission", sent.Code, sent.Body.String())
	}
	var receipt struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(sent.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	// The provider rendering of the ID is accepted by the app routes.
	read := call("GET", "/app/v1/messages/"+receipt.ID, "", nil)
	if read.Code != 200 || !strings.Contains(read.Body.String(), `"state":"on_hold"`) || !strings.Contains(read.Body.String(), `"hold":{"expires_at":`) {
		t.Fatal("held read", read.Code, read.Body.String())
	}
	for range 2 {
		released := call("POST", "/app/v1/messages/"+receipt.ID+"/release", "", nil)
		if released.Code != 200 || !strings.Contains(released.Body.String(), `"resolution":"released"`) || !strings.Contains(released.Body.String(), `"state":"queued"`) {
			t.Fatal("release", released.Code, released.Body.String())
		}
	}
	cancelled := call("POST", "/app/v1/messages/"+receipt.ID+"/cancel", "", nil)
	if cancelled.Code != 409 || cancelled.Body.String() != `{"error":"released"}`+"\n" {
		t.Fatal("cancel after release", cancelled.Code, cancelled.Body.String())
	}
	id, _ := providerid.Normalize(receipt.ID)
	if events, _, _ := f.events(id, "released"); events != 1 || f.audits(id, "message.hold.released") != 1 {
		t.Fatal("HTTP release not recorded once", events)
	}
	plain := call("POST", "/emails", `{"from":"sender@example.com","to":"one@example.net","subject":"plain","text":"body"}`, map[string]string{"Content-Type": "application/json"})
	if plain.Code != 200 || plain.Header().Get("X-Thinpost-Hold") != "" {
		t.Fatal("plain submission", plain.Code)
	}
	if err = json.Unmarshal(plain.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if got := call("POST", "/app/v1/messages/"+receipt.ID+"/release", "", nil); got.Code != 409 || got.Body.String() != `{"error":"not_held"}`+"\n" {
		t.Fatal("release of an unheld message", got.Code, got.Body.String())
	}
}
