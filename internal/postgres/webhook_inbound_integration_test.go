package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/inbound"
	"github.com/zyno-io/thinpost/internal/webhook"
)

// outboundDelivery queues the app's outbound event for its first outbound
// endpoint on the event lane, as the fan-out does.
func (f *inboundFixture) outboundDelivery(app string) string {
	f.t.Helper()
	event := f.acceptedEvent(app)
	f.fanout(event, app)
	return event
}

func TestInboundLaneClaimsAreSeparateFromTheEventLane(t *testing.T) {
	f := newInboundFixture(t)
	outboundEndpoint := f.create(f.app, "https://hooks.example.com/out")
	outboundEvent := f.outboundDelivery(f.app)
	rec := f.record(f.app)
	rec.Recipients = []string{"Reply+Ticket@mail.example.com", "other@mail.example.com"}
	if got := f.commit(rec); got != inbound.CommitAccepted {
		t.Fatalf("commit %v", got)
	}
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE lane='inbound' AND state='queued'`) != 2 || f.count(`SELECT count(*) FROM webhook_deliveries WHERE lane='event'`) != 1 {
		t.Fatal("lanes")
	}

	// The outbound claim sees only the event lane, the inbound claim only its own.
	out, err := f.repo.ClaimWebhook(f.ctx, "a0000000000000000000000000000001", 45*time.Second)
	if err != nil || out.Event.ID != outboundEvent || out.SubscriptionID != outboundEndpoint.ID || out.Inbound != nil {
		t.Fatal("outbound claim", out.Event.ID, err)
	}
	if _, err = f.repo.ClaimWebhook(f.ctx, "a0000000000000000000000000000002", 45*time.Second); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal("the outbound claim took an inbound row", err)
	}
	first, err := f.repo.ClaimWebhookInbound(f.ctx, "b0000000000000000000000000000001", webhook.InboundClaimTTL)
	if err != nil {
		t.Fatal(err)
	}
	d := first.Inbound
	if d == nil || d.InboundID != rec.ID || d.ObjectKey != rec.ObjectKey || d.Size != 100 || d.Expired || d.MailFrom != "alice@example.org" || d.ClientIP != "192.0.2.7" || d.Helo != "mx.example.org" ||
		d.TLSVersion != "TLS 1.3" || d.Receiver != "smtp.mx.example" || d.SPF != "pass" || d.DKIM != "fail" || d.DMARC != "fail" || d.MessageID != "" || len(d.Recipients) != 2 || d.Recipients[1] != "other@mail.example.com" {
		t.Fatalf("claimed data %+v", d)
	}
	if first.Event.Outcome != "inbound_received" || first.Format != "thinpost_inbound" || first.Event.MessageID != "" || first.Attempt != 1 || (d.Recipient != "Reply+Ticket@mail.example.com" && d.Recipient != "other@mail.example.com") {
		t.Fatal("claimed event", first.Event, first.Format, first.Attempt, d.Recipient)
	}
	var ttl time.Duration
	var seconds float64
	if err = f.owner.QueryRowContext(f.ctx, `SELECT extract(epoch from claim_until-clock_timestamp()) FROM webhook_deliveries WHERE claim_token=$1`, first.Token).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	if ttl = time.Duration(seconds * float64(time.Second)); ttl < 170*time.Second || ttl > 181*time.Second {
		t.Fatalf("inbound claim TTL %v, want 3 minutes", ttl)
	}
	second, err := f.repo.ClaimWebhookInbound(f.ctx, "b0000000000000000000000000000002", webhook.InboundClaimTTL)
	if err != nil || second.Event.ID == first.Event.ID || second.Inbound.Recipient == d.Recipient {
		t.Fatal("the second recipient was not claimed separately", err)
	}
	if _, err = f.repo.ClaimWebhookInbound(f.ctx, "b0000000000000000000000000000003", webhook.InboundClaimTTL); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal("nothing else is due", err)
	}

	// Results use the shared commit: a 204 delivers, a 500 retries on the same schedule.
	if err = f.repo.CommitWebhook(f.ctx, first, 204); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.CommitWebhook(f.ctx, second, 500); err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE claim_token IS NULL AND lane='inbound' AND state='delivered' AND event_id=$1`, first.Event.ID) != 1 ||
		f.count(`SELECT count(*) FROM webhook_deliveries WHERE lane='inbound' AND state='queued' AND attempts=1 AND next_attempt_at>clock_timestamp()+interval '50 seconds' AND event_id=$1`, second.Event.ID) != 1 {
		t.Fatal("results")
	}
}

func TestInboundLaneSweepsExhaustedAttemptsOnlyInItsOwnLane(t *testing.T) {
	f := newInboundFixture(t)
	f.create(f.app, "https://hooks.example.com/out")
	outboundEvent := f.outboundDelivery(f.app)
	rec := f.record(f.app)
	if got := f.commit(rec); got != inbound.CommitAccepted {
		t.Fatal("commit", got)
	}
	// Use up every attempt of both deliveries, with the claims expired.
	for i, token := range []string{"c0000000000000000000000000000001", "c0000000000000000000000000000002"} {
		lane := []string{"event", "inbound"}[i]
		f.exec(`INSERT INTO webhook_attempts(id,event_id,subscription_id) SELECT $1,event_id,subscription_id FROM webhook_deliveries WHERE lane=$2`, token, lane)
		f.exec(`UPDATE webhook_deliveries SET state='in_flight',attempts=12,claim_token=$1,claim_until=clock_timestamp()-interval '1 second' WHERE lane=$2`, token, lane)
	}
	if _, err := f.repo.ClaimWebhookInbound(f.ctx, "d0000000000000000000000000000001", webhook.InboundClaimTTL); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal("an exhausted delivery was claimed again", err)
	}
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE lane='inbound' AND state='failed' AND claim_token IS NULL AND attempts=12`) != 1 ||
		f.count(`SELECT count(*) FROM webhook_attempts WHERE id='c0000000000000000000000000000002' AND state='unknown'`) != 1 {
		t.Fatal("the inbound sweep did not fail the exhausted delivery")
	}
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE lane='event' AND state='in_flight'`) != 1 {
		t.Fatal("the inbound sweep touched the event lane")
	}
	// ...and the outbound sweep is still limited to the event lane.
	f.exec(`UPDATE webhook_deliveries SET state='in_flight',attempts=12,claim_token='c0000000000000000000000000000002',claim_until=clock_timestamp()-interval '1 second' WHERE lane='inbound'`)
	f.exec(`UPDATE webhook_attempts SET state='in_flight' WHERE id='c0000000000000000000000000000002'`)
	if _, err := f.repo.ClaimWebhook(f.ctx, "d0000000000000000000000000000002", 45*time.Second); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE lane='event' AND state='failed' AND event_id=$1`, outboundEvent) != 1 || f.count(`SELECT count(*) FROM webhook_deliveries WHERE lane='inbound' AND state='in_flight'`) != 1 {
		t.Fatal("the outbound sweep crossed lanes")
	}
}

func TestInboundDeferralDoesNotUseAnAttempt(t *testing.T) {
	f := newInboundFixture(t)
	if got := f.commit(f.record(f.app)); got != inbound.CommitAccepted {
		t.Fatal("commit", got)
	}
	claim, err := f.repo.ClaimWebhookInbound(f.ctx, "f0000000000000000000000000000001", webhook.InboundClaimTTL)
	if err != nil || claim.Attempt != 1 {
		t.Fatal(err)
	}
	if err = f.repo.DeferWebhook(f.ctx, claim, 0); err == nil {
		t.Fatal("a zero delay was accepted")
	}
	if err = f.repo.DeferWebhook(f.ctx, claim, webhook.BudgetDeferral); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.DeferWebhook(f.ctx, claim, webhook.BudgetDeferral); !errors.Is(err, webhook.ErrLostClaim) {
		t.Fatal("a deferred claim is gone", err)
	}
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE state='queued' AND attempts=0 AND claim_token IS NULL AND next_attempt_at>clock_timestamp()+interval '10 seconds' AND next_attempt_at<clock_timestamp()+interval '20 seconds'`) != 1 ||
		f.count(`SELECT count(*) FROM webhook_attempts WHERE id=$1 AND state='deferred' AND http_status IS NULL AND finished_at IS NOT NULL`, claim.Token) != 1 {
		t.Fatal("deferral not recorded")
	}
	if _, err = f.repo.ClaimWebhookInbound(f.ctx, "f0000000000000000000000000000002", webhook.InboundClaimTTL); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal("a deferred delivery is not due yet", err)
	}
	attempts, err := f.repo.ListWebhookAttempts(f.ctx, "op", f.app, claim.Event.ID, claim.SubscriptionID)
	if err != nil || len(attempts.Attempts) != 1 || attempts.Attempts[0].State != "deferred" {
		t.Fatal("attempts", attempts, err)
	}
	f.exec(`UPDATE webhook_deliveries SET next_attempt_at=clock_timestamp()`)
	again, err := f.repo.ClaimWebhookInbound(f.ctx, "f0000000000000000000000000000003", webhook.InboundClaimTTL)
	if err != nil || again.Attempt != 1 {
		t.Fatal("the attempt counter must not have advanced", err, again.Attempt)
	}
}

func TestInboundFailureHistoryAndReplay(t *testing.T) {
	f := newInboundFixture(t)
	rec := f.record(f.app)
	if got := f.commit(rec); got != inbound.CommitAccepted {
		t.Fatal("commit", got)
	}
	claim, err := f.repo.ClaimWebhookInbound(f.ctx, "e0000000000000000000000000000001", webhook.InboundClaimTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.repo.FailWebhook(f.ctx, claim, "bogus"); err == nil {
		t.Fatal("an unknown failure class was accepted")
	}
	if err = f.repo.FailWebhook(f.ctx, claim, webhook.ClassContentUnavailable); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.FailWebhook(f.ctx, claim, webhook.ClassContentUnavailable); !errors.Is(err, webhook.ErrLostClaim) {
		t.Fatal("a failed delivery's claim must be gone", err)
	}
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE state='failed' AND claim_token IS NULL AND event_id=$1`, claim.Event.ID) != 1 ||
		f.count(`SELECT count(*) FROM webhook_attempts WHERE id=$1 AND state='content_unavailable' AND http_status IS NULL AND finished_at IS NOT NULL`, claim.Token) != 1 {
		t.Fatal("failure not recorded with its class")
	}
	if _, err = f.repo.ClaimWebhookInbound(f.ctx, "e0000000000000000000000000000002", webhook.InboundClaimTTL); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal("a permanently failed delivery was retried", err)
	}

	// History lists the inbound event and its attempts.
	page, err := f.repo.ListWebhookDeliveries(f.ctx, "op", f.app, "", 50)
	if err != nil || len(page.Deliveries) != 1 || page.Deliveries[0].InboundID != rec.ID || page.Deliveries[0].MessageID != "" || page.Deliveries[0].State != "failed" || page.Deliveries[0].Outcome != "inbound_received" {
		t.Fatal("history", page, err)
	}
	attempts, err := f.repo.ListWebhookAttempts(f.ctx, "op", f.app, claim.Event.ID, claim.SubscriptionID)
	if err != nil || len(attempts.Attempts) != 1 || attempts.Attempts[0].State != "content_unavailable" || attempts.Attempts[0].HTTPStatus != nil {
		t.Fatal("attempts", attempts, err)
	}
	if _, err = f.repo.ListWebhookAttempts(f.ctx, "op", f.other, claim.Event.ID, claim.SubscriptionID); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("another app saw the attempts", err)
	}

	// Replay: refused once the content has expired, allowed before.
	f.exec(`UPDATE inbound_messages SET content_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, rec.ID)
	if err = f.repo.ReplayWebhookDelivery(f.ctx, "op", f.app, claim.Event.ID, claim.SubscriptionID); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("replay of expired content must be refused", err)
	}
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE state='failed'`) != 1 || f.count(`SELECT count(*) FROM audit_events WHERE action='webhook.delivery.replay'`) != 0 {
		t.Fatal("a refused replay changed state")
	}
	expired, err := f.repo.ClaimWebhookInbound(f.ctx, "e0000000000000000000000000000003", webhook.InboundClaimTTL)
	if !errors.Is(err, webhook.ErrNoWork) {
		t.Fatalf("%+v %v", expired, err)
	}
	f.exec(`UPDATE inbound_messages SET content_expires_at=clock_timestamp()+interval '1 day' WHERE id=$1`, rec.ID)
	if err = f.repo.ReplayWebhookDelivery(f.ctx, "op", f.app, claim.Event.ID, claim.SubscriptionID); err != nil {
		t.Fatal(err)
	}
	replayed, err := f.repo.ClaimWebhookInbound(f.ctx, "e0000000000000000000000000000004", webhook.InboundClaimTTL)
	if err != nil || replayed.Event.ID != claim.Event.ID || replayed.Attempt != 1 || replayed.Inbound.Expired {
		t.Fatal("replayed claim", err)
	}
	// A claim whose content expired in the meantime is flagged by the database
	// clock, so the dispatcher never reads it.
	f.exec(`UPDATE inbound_messages SET content_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, rec.ID)
	f.exec(`UPDATE webhook_deliveries SET state='queued',claim_token=NULL,claim_until=NULL,next_attempt_at=clock_timestamp()`)
	f.exec(`UPDATE webhook_attempts SET state='unknown' WHERE state='in_flight'`)
	late, err := f.repo.ClaimWebhookInbound(f.ctx, "e0000000000000000000000000000005", webhook.InboundClaimTTL)
	if err != nil || !late.Inbound.Expired {
		t.Fatal("expiry not reported on claim", err)
	}
}

func TestInboundEndpointsAreCreatedThroughTheAdminStore(t *testing.T) {
	f := newWebhookFixture(t, "inbound_endpoints_")
	app, other := f.app("one"), f.app("two")
	input := admin.WebhookInput{Format: "thinpost_inbound", Endpoint: "https://hooks.example.com/in", Description: "replies"}
	created, err := f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), input, []byte("sealed"), &admin.SealedWebhookAuth{Ciphertext: []byte("auth"), Summary: admin.WebhookAuthSummary{Username: "u"}})
	if err != nil || created.Format != "thinpost_inbound" || !created.Active || created.SecretCreatedAt == nil || created.Auth == nil || created.Auth.Username != "u" || len(created.Events) != 0 {
		t.Fatal("create", created, err)
	}
	if _, err = f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), admin.WebhookInput{Format: "thinpost_inbound", Endpoint: "https://hooks.example.com/in", Events: []string{"accepted"}}, []byte("sealed"), nil); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("an outbound event filter on an inbound endpoint", err)
	}
	if _, err = f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), admin.WebhookInput{Format: "thinpost", Endpoint: "https://hooks.example.com/in", Events: []string{"inbound_received"}}, []byte("sealed"), nil); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("an inbound event filter on an outbound endpoint", err)
	}
	if _, err = f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), input, nil, nil); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("an unsigned inbound endpoint", err)
	}
	filtered, err := f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), admin.WebhookInput{Format: "thinpost_inbound", Endpoint: "https://hooks.example.com/in2", Events: []string{"inbound_received"}}, []byte("sealed"), nil)
	if err != nil || len(filtered.Events) != 1 {
		t.Fatal(err)
	}
	// Endpoints of every format share the ten-endpoint cap.
	for i := 0; i < 8; i++ {
		if _, err = f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), input, []byte("sealed"), nil); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err = f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), input, []byte("sealed"), nil); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("the eleventh endpoint", err)
	}
	if _, err = f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), admin.WebhookInput{Endpoint: "https://hooks.example.com/x"}, []byte("sealed"), nil); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("the cap counts inbound endpoints", err)
	}
	if _, err = f.repo.InsertWebhook(f.ctx, "operator", other, f.id(), input, []byte("sealed"), nil); err != nil {
		t.Fatal("the cap is per app", err)
	}
	// An enabled inbound endpoint is what lets the app receive.
	f.exec(`INSERT INTO inbound_domains(id,app_id,domain,state,verified_at) VALUES ($1,$2,'mail.example.com','active',clock_timestamp())`, f.id(), other)
	routes, err := f.repo.InboundRoutes(f.ctx)
	if err != nil || len(routes.Routes) != 1 || !routes.Routes[0].Accept {
		t.Fatal("an app with an inbound endpoint must accept", routes, err)
	}
	list, err := f.repo.ListWebhooks(f.ctx, other)
	if err != nil || len(list) != 1 {
		t.Fatal(err)
	}
	off := false
	if _, err = f.repo.UpdateWebhook(f.ctx, "operator", other, list[0].ID, admin.WebhookPatch{Active: &off}, nil); err != nil {
		t.Fatal(err)
	}
	if routes, err = f.repo.InboundRoutes(f.ctx); err != nil || routes.Routes[0].Accept {
		t.Fatal("an app with no enabled inbound endpoint must answer 451", routes, err)
	}
	if _, err = f.repo.ReplaceWebhookSecret(f.ctx, "operator", app, created.ID, "thinpost_inbound", []byte("new")); err != nil {
		t.Fatal("secret rotation", err)
	}
	if name, err := f.repo.WebhookFormat(f.ctx, app, created.ID); err != nil || name != "thinpost_inbound" {
		t.Fatal(name, err)
	}
}
