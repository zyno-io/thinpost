package postgres

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/unsubforward"
	"github.com/zyno-io/thinpost/internal/webhook"
)

type forwardRow struct {
	state, host, reason, failure string
	attempts                     int
	status                       *int
	finished                     bool
}

func (f *webhookFixture) forward(delivery string) forwardRow {
	f.t.Helper()
	var r forwardRow
	var host, reason, failure *string
	var finished *time.Time
	if err := f.owner.QueryRowContext(f.ctx, `SELECT state,target_host,reason,last_failure,attempts,last_status,finished_at FROM unsubscribe_forwards WHERE delivery_id=$1`, delivery).
		Scan(&r.state, &host, &reason, &failure, &r.attempts, &r.status, &finished); err != nil {
		f.t.Fatal(err)
	}
	if host != nil {
		r.host = *host
	}
	if reason != nil {
		r.reason = *reason
	}
	if failure != nil {
		r.failure = *failure
	}
	r.finished = finished != nil
	return r
}

func (f *webhookFixture) arm(delivery string) {
	f.exec(`INSERT INTO unsubscribe_forwards(delivery_id,state,target_host) VALUES ($1,'armed','unsub.example.com')`, delivery)
}

func (f *webhookFixture) makeDue(delivery string) {
	f.exec(`UPDATE unsubscribe_forwards SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE delivery_id=$1 AND state='queued'`, delivery)
}

func TestUnsubscribeForwardPostgres(t *testing.T) {
	f := newWebhookFixture(t, "unsub_forward_")
	ctx := f.ctx
	app := f.app("forward")
	f.create(app, "https://native.example.com/")
	var lines []string
	var linesMu sync.Mutex
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) {
		linesMu.Lock()
		defer linesMu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}))
	forwardLines := func() []string {
		linesMu.Lock()
		defer linesMu.Unlock()
		var out []string
		for _, line := range lines {
			if strings.HasPrefix(line, "unsubscribe_forward ") {
				out = append(out, line)
			}
		}
		return out
	}

	t.Run("runtime grants", func(t *testing.T) {
		for _, statement := range []string{`DELETE FROM unsubscribe_forwards`, `TRUNCATE unsubscribe_forwards`} {
			if _, err := f.repo.DB.ExecContext(ctx, statement); err == nil {
				t.Fatalf("runtime permitted %q", statement)
			}
		}
		if _, err := f.repo.DB.ExecContext(ctx, `SELECT * FROM unsubscribe_forwards`); err != nil {
			t.Fatal(err)
		}
	})

	message, deliveries, _ := f.message(app, false, "subject")
	f.arm(deliveries[0])
	f.exec(`INSERT INTO unsubscribe_forwards(delivery_id,state,reason) VALUES ($1,'not_forwarded','mailto_only')`, deliveries[1])

	t.Run("nothing shows before the recipient unsubscribes", func(t *testing.T) {
		detail, err := f.repo.GetMessage(ctx, "operator", app, message)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range detail.Deliveries {
			if d.UnsubscribeForward != nil {
				t.Fatal("a forward showed before an unsubscribe", d.UnsubscribeForward)
			}
		}
	})

	t.Run("an unsubscribe queues the forward once, even when clicked many times", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- f.repo.RecordUnsubscribe(ctx, deliveries[0]) }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if r := f.forward(deliveries[0]); r.state != "queued" || r.attempts != 0 {
			t.Fatalf("%+v", r)
		}
		if got := forwardLines(); len(got) != 1 || !strings.HasSuffix(got[0], " state=queued") || !strings.Contains(got[0], deliveries[0]) {
			t.Fatal("oplog", got)
		}
		// The unsubscribe itself is recorded as always.
		if reason, _, _ := f.suppression(app, "first@example.net"); reason != "unsubscribe" || f.count(`SELECT count(*) FROM webhook_events WHERE delivery_id=$1 AND outcome='unsubscribed'`, deliveries[0]) != 1 {
			t.Fatal("unsubscribe not recorded")
		}
		// A later click after the forward finished does not queue another.
		if err := f.repo.RecordUnsubscribe(ctx, deliveries[0]); err != nil || f.forward(deliveries[0]).attempts != 0 || len(forwardLines()) != 1 {
			t.Fatal("a repeat changed the forward", err)
		}
	})

	t.Run("a not-forwardable original is shown as such after the unsubscribe", func(t *testing.T) {
		if err := f.repo.RecordUnsubscribe(ctx, deliveries[1]); err != nil {
			t.Fatal(err)
		}
		if r := f.forward(deliveries[1]); r.state != "not_forwarded" || r.reason != "mailto_only" {
			t.Fatalf("%+v", r)
		}
		detail, err := f.repo.GetMessage(ctx, "operator", app, message)
		if err != nil {
			t.Fatal(err)
		}
		got := detail.Deliveries[1].UnsubscribeForward
		if got == nil || got.State != "not_forwarded" || got.Reason != "mailto_only" || got.Host != "" {
			t.Fatalf("%+v", got)
		}
		queued := detail.Deliveries[0].UnsubscribeForward
		if queued == nil || queued.State != "queued" || queued.Host != "unsub.example.com" {
			t.Fatalf("%+v", queued)
		}
		encoded, _ := json.Marshal(detail)
		if strings.Contains(string(encoded), "token") || strings.Contains(string(encoded), "https://"+"unsub") {
			t.Fatal("the sender's link reached the admin API")
		}
	})

	t.Run("a worker leases it, a 5xx is retried with backoff, then it is delivered and the link is cleared", func(t *testing.T) {
		claim, err := f.repo.ClaimForward(ctx, f.id(), 45*time.Second)
		if err != nil || claim.DeliveryID != deliveries[0] || claim.Host != "unsub.example.com" || claim.EnhancedProtection || claim.SourceKey != "k"+message || claim.Attempt != 1 {
			t.Fatalf("%+v %v", claim, err)
		}
		if _, err = f.repo.ClaimForward(ctx, f.id(), 45*time.Second); !errors.Is(err, webhook.ErrNoWork) {
			t.Fatal("a leased forward was claimed twice", err)
		}
		if err = f.repo.CommitForward(ctx, unsubforward.Claim{DeliveryID: claim.DeliveryID, Token: f.id()}, unsubforward.Outcome{Done: true, Status: 200}); !errors.Is(err, webhook.ErrLostClaim) {
			t.Fatal("a result without the lease was accepted", err)
		}
		if err = f.repo.CommitForward(ctx, claim, unsubforward.Classify(webhook.Result{StatusCode: 503}, nil)); err != nil {
			t.Fatal(err)
		}
		r := f.forward(deliveries[0])
		if r.state != "queued" || r.attempts != 1 || r.failure != "http_status" || r.status == nil || *r.status != 503 || r.finished {
			t.Fatalf("%+v", r)
		}
		if _, err = f.repo.ClaimForward(ctx, f.id(), 45*time.Second); !errors.Is(err, webhook.ErrNoWork) {
			t.Fatal("a retry was claimed before its backoff", err)
		}
		detail, _ := f.repo.GetMessage(ctx, "operator", app, message)
		if got := detail.Deliveries[0].UnsubscribeForward; got == nil || got.State != "queued" || got.Reason != "http_status" || got.Attempts != 1 {
			t.Fatalf("%+v", got)
		}
		f.makeDue(deliveries[0])
		claim, err = f.repo.ClaimForward(ctx, f.id(), 45*time.Second)
		if err != nil || claim.Attempt != 2 {
			t.Fatalf("%+v %v", claim, err)
		}
		if err = f.repo.CommitForward(ctx, claim, unsubforward.Classify(webhook.Result{StatusCode: 200, Success: true}, nil)); err != nil {
			t.Fatal(err)
		}
		r = f.forward(deliveries[0])
		if r.state != "delivered" || r.host != "unsub.example.com" || !r.finished || r.failure != "" || r.attempts != 2 {
			t.Fatalf("%+v", r)
		}
		if got := forwardLines(); len(got) != 2 || !strings.HasSuffix(got[1], " state=delivered") {
			t.Fatal("oplog", got)
		}
		for _, line := range forwardLines() {
			if strings.Contains(line, "unsub.example.com") || strings.Contains(line, "secret") || strings.Contains(line, "@") {
				t.Fatal("the oplog line carries the link or an address:", line)
			}
		}
		detail, _ = f.repo.GetMessage(ctx, "operator", app, message)
		if got := detail.Deliveries[0].UnsubscribeForward; got == nil || got.State != "delivered" || got.Reason != "" || got.FinishedAt == nil {
			t.Fatalf("%+v", got)
		}
	})

	// fresh arms a forward on a new message and records the recipient's unsubscribe,
	// returning the message and delivery IDs.
	fresh := func(t *testing.T) (string, string) {
		t.Helper()
		m, d, _ := f.message(app, false, "subject")
		f.arm(d[0])
		if err := f.repo.RecordUnsubscribe(ctx, d[0]); err != nil {
			t.Fatal(err)
		}
		return m, d[0]
	}

	t.Run("attempts are bounded: the forward fails after the last retry, and the unsubscribe stands", func(t *testing.T) {
		m, delivery := fresh(t)
		for attempt := 1; attempt <= unsubforward.MaxAttempts; attempt++ {
			f.makeDue(delivery)
			claim, err := f.repo.ClaimForward(ctx, f.id(), 45*time.Second)
			if err != nil || claim.Attempt != attempt {
				t.Fatal(attempt, claim, err)
			}
			if err = f.repo.CommitForward(ctx, claim, unsubforward.Classify(webhook.Result{Failure: webhook.FailureTimeout}, nil)); err != nil {
				t.Fatal(err)
			}
		}
		r := f.forward(delivery)
		if r.state != "failed" || r.failure != "timeout" || !r.finished || r.attempts != unsubforward.MaxAttempts {
			t.Fatalf("%+v", r)
		}
		f.makeDue(delivery)
		if _, err := f.repo.ClaimForward(ctx, f.id(), 45*time.Second); !errors.Is(err, webhook.ErrNoWork) {
			t.Fatal("a failed forward was claimed", err)
		}
		detail, _ := f.repo.GetMessage(ctx, "operator", app, m)
		if got := detail.Deliveries[0].UnsubscribeForward; got == nil || got.State != "failed" || got.Reason != "timeout" || got.Host != "unsub.example.com" {
			t.Fatalf("%+v", got)
		}
		// The recipient is suppressed and the event was queued all the same.
		if f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1 AND kind='unsubscribe'`, delivery) != 1 || f.count(`SELECT count(*) FROM webhook_events WHERE delivery_id=$1 AND outcome='unsubscribed'`, delivery) != 1 {
			t.Fatal("the unsubscribe was not kept")
		}
	})

	t.Run("a blocked destination and a refusal are final at once", func(t *testing.T) {
		for name, result := range map[string]webhook.Result{
			"blocked":  {Failure: webhook.FailureBlocked},
			"refused":  {StatusCode: 404},
			"redirect": {StatusCode: 302},
		} {
			_, delivery := fresh(t)
			claim, err := f.repo.ClaimForward(ctx, f.id(), 45*time.Second)
			if err != nil || claim.DeliveryID != delivery {
				t.Fatal(name, claim, err)
			}
			if err = f.repo.CommitForward(ctx, claim, unsubforward.Classify(result, nil)); err != nil {
				t.Fatal(err)
			}
			if r := f.forward(delivery); r.state != "failed" || r.attempts != 1 {
				t.Fatalf("%s %+v", name, r)
			}
		}
	})

	t.Run("a crashed worker's lease is retaken; with every attempt used it fails", func(t *testing.T) {
		_, delivery := fresh(t)
		claim, err := f.repo.ClaimForward(ctx, f.id(), 45*time.Second)
		if err != nil || claim.DeliveryID != delivery {
			t.Fatal(claim, err)
		}
		f.exec(`UPDATE unsubscribe_forwards SET claim_until=clock_timestamp()-interval '1 second' WHERE delivery_id=$1`, delivery)
		if err = f.repo.CommitForward(ctx, claim, unsubforward.Outcome{Done: true}); !errors.Is(err, webhook.ErrLostClaim) {
			t.Fatal("an expired lease committed", err)
		}
		again, err := f.repo.ClaimForward(ctx, f.id(), 45*time.Second)
		if err != nil || again.DeliveryID != delivery || again.Attempt != 2 {
			t.Fatal(again, err)
		}
		f.exec(`UPDATE unsubscribe_forwards SET attempts=$2,claim_until=clock_timestamp()-interval '1 second' WHERE delivery_id=$1`, delivery, unsubforward.MaxAttempts)
		if _, err = f.repo.ClaimForward(ctx, f.id(), 45*time.Second); !errors.Is(err, webhook.ErrNoWork) {
			t.Fatal(err)
		}
		if r := f.forward(delivery); r.state != "failed" || r.failure != "unknown" {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("an unsubscribe from a delivery with no forward record works as before", func(t *testing.T) {
		_, d, _ := f.message(app, false, "subject")
		if err := f.repo.RecordUnsubscribe(ctx, d[0]); err != nil {
			t.Fatal(err)
		}
		if f.count(`SELECT count(*) FROM unsubscribe_forwards WHERE delivery_id=$1`, d[0]) != 0 {
			t.Fatal("a forward appeared from nowhere")
		}
	})
}

// A forward that is not exactly what unsubforward produces never fails the
// preparation: checkedForward records it as unusable and the mail goes on.
func TestCheckedForwardNeverFailsAPreparation(t *testing.T) {
	for name, forward := range map[string]*preparation.Forward{
		"empty":              {},
		"host and reason":    {Host: "unsub.example.com", Reason: unsubforward.ReasonUnusable},
		"ip literal":         {Host: "10.0.0.1"},
		"not canonical":      {Host: "Unsub.Example.com"},
		"unknown reason":     {Reason: "because"},
		"host with a reason": {Host: "unsub.example.com", Reason: unsubforward.ReasonMailtoOnly},
	} {
		if got := checkedForward(forward); got.Host != "" || got.Reason != unsubforward.ReasonUnusable {
			t.Errorf("%s: %+v", name, got)
		}
	}
	for name, forward := range map[string]*preparation.Forward{
		"host":   {Host: "unsub.example.com"},
		"short":  {Host: "a.b"},
		"reason": {Reason: unsubforward.ReasonNoOneClick},
	} {
		if got := checkedForward(forward); got != forward {
			t.Errorf("%s changed: %+v", name, got)
		}
	}
}

// Every value Decide can produce is accepted by the table: its host and reason
// satisfy the CHECKs, so a preparation commit cannot fail on a forward.
func TestEveryDecideOutputFitsTheTable(t *testing.T) {
	f := newWebhookFixture(t, "unsub_decide_")
	app := f.app("decide")
	_, deliveries, _ := f.message(app, false, "s")
	long := strings.Repeat("a", 63)
	cases := []unsubforward.Original{
		{ListFields: 1, Value: "<https://" + "a.b" + ">", PostFields: 1, PostValue: unsubforward.OneClickPost},
		{ListFields: 1, Value: "<https://" + long + "." + long + "." + long + "." + strings.Repeat("a", 57) + ".com/u>", PostFields: 1, PostValue: unsubforward.OneClickPost},
		{ListFields: 1, Value: "<https://Unsub.Example.COM:443/u?x=1>", PostFields: 1, PostValue: unsubforward.OneClickPost},
		{ListFields: 1, Value: "<https://unsub.example.com/u>"},
		{ListFields: 1, Value: "<mailto:u@example.com>"},
		{ListFields: 1, Value: "junk"},
		{ListFields: 1, Value: "<https://10.0.0.1/u>", PostFields: 1, PostValue: unsubforward.OneClickPost},
		{ListFields: 2},
		{PostFields: 1, PostValue: unsubforward.OneClickPost},
	}
	for i, original := range cases {
		target, reason := unsubforward.Decide(original)
		if i == 1 && (reason != "" || len(target.Host) != 253) {
			t.Fatalf("the longest host was not accepted by Decide: %d bytes, reason %q", len(target.Host), reason)
		}
		forward := &preparation.Forward{Host: target.Host, Reason: reason}
		if !validForward(forward) {
			t.Fatalf("case %d: Decide produced %+v which the commit would downgrade", i, forward)
		}
		delivery := deliveries[0]
		f.exec(`DELETE FROM unsubscribe_forwards WHERE delivery_id=$1`, delivery)
		tx, err := f.owner.BeginTx(f.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = insertForward(f.ctx, tx, delivery, forward); err != nil {
			t.Fatalf("case %d: %+v rejected by the table: %v", i, forward, err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

// Deleting an app ends its forwards (armed, queued and in flight) as failed with
// the reason app_deleted, counts them in the audit, stops a leased attempt from
// committing, and keeps a later unsubscribe from queueing one.
func TestDeletedAppEndsItsForwards(t *testing.T) {
	f := newWebhookFixture(t, "unsub_delete_")
	ctx := f.ctx
	app, other := f.app("gone"), f.app("kept")
	_, d1, _ := f.message(app, false, "s")
	_, d2, _ := f.message(app, false, "s")
	_, keptDeliveries, _ := f.message(other, false, "s")
	for _, id := range []string{d1[0], d1[1], d2[0], keptDeliveries[0]} {
		f.arm(id)
	}
	// d1[0]: in flight. d2[0]: queued. d1[1]: armed. keptDeliveries[0]: queued, another app.
	if err := f.repo.RecordUnsubscribe(ctx, d1[0]); err != nil {
		t.Fatal(err)
	}
	leased, err := f.repo.ClaimForward(ctx, f.id(), 45*time.Second)
	if err != nil || leased.DeliveryID != d1[0] {
		t.Fatal(leased, err)
	}
	for _, id := range []string{d2[0], keptDeliveries[0]} {
		if err = f.repo.RecordUnsubscribe(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.repo.DeleteApp(ctx, "operator", app); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{d1[0], d1[1], d2[0]} {
		if r := f.forward(id); r.state != "failed" || r.failure != "app_deleted" || !r.finished {
			t.Fatalf("%s %+v", id, r)
		}
	}
	if r := f.forward(keptDeliveries[0]); r.state != "queued" {
		t.Fatalf("another app's forward changed: %+v", r)
	}
	if err = f.repo.CommitForward(ctx, leased, unsubforward.Outcome{Done: true}); !errors.Is(err, webhook.ErrLostClaim) {
		t.Fatal("a leased attempt committed after the app was deleted", err)
	}
	if r := f.forward(d1[0]); r.state != "failed" {
		t.Fatalf("%+v", r)
	}
	var ended int
	if err = f.owner.QueryRowContext(ctx, `SELECT (details->>'unsubscribe_forwards_ended')::int FROM audit_events WHERE app_id=$1 AND action='app.deleted'`, app).Scan(&ended); err != nil || ended != 3 {
		t.Fatal("audit count", ended, err)
	}
	// Only the other app's forward is claimable.
	claim, err := f.repo.ClaimForward(ctx, f.id(), 45*time.Second)
	if err != nil || claim.DeliveryID != keptDeliveries[0] {
		t.Fatal(claim, err)
	}
	// A forward armed for a deleted app (a delivery that slipped past) is never queued.
	_, late, _ := f.message(app, false, "s")
	f.arm(late[0])
	if err = f.repo.RecordUnsubscribe(ctx, late[0]); err != nil {
		t.Fatal(err)
	}
	if r := f.forward(late[0]); r.state != "armed" {
		t.Fatalf("a deleted app's unsubscribe queued a forward: %+v", r)
	}
	// And a queued row of a deleted app is not claimed even if one was left behind.
	f.exec(`UPDATE unsubscribe_forwards SET state='queued',next_attempt_at=now() WHERE delivery_id=$1`, late[0])
	f.exec(`UPDATE unsubscribe_forwards SET state='delivered',finished_at=now(),claim_token=NULL,claim_until=NULL WHERE delivery_id=$1`, keptDeliveries[0])
	if _, err = f.repo.ClaimForward(ctx, f.id(), 45*time.Second); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal("a deleted app's forward was claimed", err)
	}
}
