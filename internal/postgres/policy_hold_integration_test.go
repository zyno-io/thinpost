package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/submission"
)

func TestRecipientPolicyHoldPostgres(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "policy_hold_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	owner, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err = Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	const generation = "00000000000000000000000000000aaa"
	id := func(n int) string { return fmt.Sprintf("%032x", n) }
	app, other, message, deliveryID := id(1), id(2), id(3), id(4)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, []any{generation}},
		{`INSERT INTO apps(id,name) VALUES ($1,'One'),($2,'Other')`, []any{app, other}},
		{`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'source',true,'prepared')`, []any{message, app}},
		{`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'signed')`, []any{deliveryID, message}},
	} {
		if _, err = owner.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	db, cleanup := restrictedRuntimeForTest(t, ctx, owner, u.String())
	defer cleanup()
	repo := &Repository{DB: db, Generation: generation}
	lease, err := repo.AcquireControllerLease(ctx, "worker", "controller", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := repo.ClaimDelivery(ctx, lease, id(100), claimCaps())
	if err != nil {
		t.Fatal(err)
	}
	result := delivery.Result{AttemptID: claim.AttemptID, DeliveryID: claim.DeliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch, Outcome: delivery.OutcomeRecipientPolicyDenied}
	if err = repo.CommitDeliveryResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	if err = repo.CommitDeliveryResult(ctx, result); err != nil {
		t.Fatal("idempotent hold", err)
	}
	var state, event string
	var pending bool
	var count int
	if err = owner.QueryRowContext(ctx, `SELECT d.state,m.pending,(SELECT count(*) FROM webhook_events WHERE attempt_id=$1), (SELECT outcome FROM webhook_events WHERE attempt_id=$1) FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE d.id=$2`, claim.AttemptID, deliveryID).Scan(&state, &pending, &count, &event); err != nil || state != "recipient_policy_held" || !pending || count != 1 || event != delivery.OutcomeRecipientPolicyDenied {
		t.Fatal("hold state, pending or event", state, pending, count, event, err)
	}
	if _, err = repo.ClaimDelivery(ctx, lease, id(101), claimCaps()); !errors.Is(err, delivery.ErrNoWork) {
		t.Fatal("held delivery claimed", err)
	}
	if err = repo.RetryPolicyHold(ctx, "operator", other, message, deliveryID); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("cross-app release", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE deliveries SET current_attempt_id=$2 WHERE id=$1`, deliveryID, id(777)); err != nil {
		t.Fatal(err)
	}
	if err = repo.RetryPolicyHold(ctx, "operator", app, message, deliveryID); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("inconsistent active attempt released", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE deliveries SET current_attempt_id=NULL WHERE id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE messages SET pending=false WHERE id=$1`, message); err != nil {
		t.Fatal(err)
	}
	if err = repo.RetryPolicyHold(ctx, "operator", app, message, deliveryID); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("non-pending message released", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE messages SET pending=true WHERE id=$1`, message); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE apps SET active=false WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	if err = repo.RetryPolicyHold(ctx, "operator", app, message, deliveryID); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("inactive app release", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE apps SET active=true WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `CREATE FUNCTION fail_hold_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='delivery.recipient_policy.retry' THEN RAISE EXCEPTION 'test audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_hold_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_hold_audit()`); err != nil {
		t.Fatal(err)
	}
	if err = repo.RetryPolicyHold(ctx, "operator", app, message, deliveryID); err == nil {
		t.Fatal("release succeeded without audit")
	}
	if err = owner.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE id=$1`, deliveryID).Scan(&state); err != nil || state != "recipient_policy_held" {
		t.Fatal("audit failure changed hold", state, err)
	}
	if _, err = owner.ExecContext(ctx, `DROP TRIGGER fail_hold_audit ON audit_events; DROP FUNCTION fail_hold_audit()`); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	results := make(chan error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- repo.RetryPolicyHold(ctx, "operator", app, message, deliveryID)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var success, conflicts int
	for e := range results {
		if e == nil {
			success++
		} else if errors.Is(e, admin.ErrConflict) {
			conflicts++
		} else {
			t.Fatal("release error", e)
		}
	}
	if success != 1 || conflicts != workers-1 {
		t.Fatal("concurrent release", success, conflicts)
	}
	// More than 12 operator releases and policy denials must not exhaust the
	// independent SMTP retry budget. The following ordinary rejection retries.
	for i := 0; i < 12; i++ {
		claim, err = repo.ClaimDelivery(ctx, lease, id(200+i), claimCaps())
		if err != nil {
			t.Fatal("claim after release", i, err)
		}
		result = delivery.Result{AttemptID: claim.AttemptID, DeliveryID: deliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch, Outcome: delivery.OutcomeRecipientPolicyDenied, JournalID: id(999)}
		if err = repo.CommitDeliveryResult(ctx, result); err != nil {
			t.Fatal("edge hold", i, err)
		}
		if err = repo.RetryPolicyHold(ctx, "operator", app, message, deliveryID); err != nil {
			t.Fatal("release", i, err)
		}
	}
	claim, err = repo.ClaimDelivery(ctx, lease, id(300), claimCaps())
	if err != nil {
		t.Fatal(err)
	}
	result = delivery.Result{AttemptID: claim.AttemptID, DeliveryID: deliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch, Outcome: "rejected"}
	if err = repo.CommitDeliveryResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE id=$1`, deliveryID).Scan(&state); err != nil || state != "queued" {
		t.Fatal("policy holds consumed SMTP retry budget", state, err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE deliveries SET retry_after=now()-interval '1 second' WHERE id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	claim, err = repo.ClaimDelivery(ctx, lease, id(301), claimCaps())
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, deliveryID, id(998), id(999), [32]byte{1}, 0); err != nil {
		t.Fatal(err)
	}
	result = delivery.Result{AttemptID: claim.AttemptID, DeliveryID: deliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch, JournalID: id(999), Outcome: delivery.OutcomeRecipientPolicyDenied}
	if err = repo.CommitDeliveryResult(ctx, result); !errors.Is(err, delivery.ErrResultConflict) {
		t.Fatal("policy denial accepted after SMTP authorization", err)
	}
	t.Run("app status changes are audited and atomic", func(t *testing.T) {
		if _, err := owner.ExecContext(ctx, `INSERT INTO api_keys(id,app_id,digest) VALUES ($1,$2,decode(repeat('ab',32),'hex'))`, id(399), other); err != nil {
			t.Fatal(err)
		}
		var keyDigest [32]byte
		for i := range keyDigest {
			keyDigest[i] = 0xab
		}
		const n = 8
		start := make(chan struct{})
		results := make(chan error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; results <- repo.SetAppActive(ctx, "operator", other, false) }()
		}
		close(start)
		wg.Wait()
		close(results)
		for e := range results {
			if e != nil {
				t.Fatal("concurrent pause", e)
			}
		}
		var active bool
		var changes int
		if err := owner.QueryRowContext(ctx, `SELECT active,(SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='app.deactivated') FROM apps WHERE id=$1`, other).Scan(&active, &changes); err != nil || active || changes != 1 {
			t.Fatal("pause audit", active, changes, err)
		}
		if _, err := repo.LookupKey(ctx, keyDigest); !errors.Is(err, submission.ErrInvalidKey) {
			t.Fatal("paused app key authenticated", err)
		}
		if _, err := owner.ExecContext(ctx, `CREATE FUNCTION fail_activation_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='app.activated' THEN RAISE EXCEPTION 'test audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_activation_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_activation_audit()`); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetAppActive(ctx, "operator", other, true); err == nil {
			t.Fatal("activation succeeded without audit")
		}
		if err := owner.QueryRowContext(ctx, `SELECT active FROM apps WHERE id=$1`, other).Scan(&active); err != nil || active {
			t.Fatal("failed audit did not roll back activation", active, err)
		}
		if _, err := owner.ExecContext(ctx, `DROP TRIGGER fail_activation_audit ON audit_events; DROP FUNCTION fail_activation_audit()`); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetAppActive(ctx, "operator", other, true); err != nil {
			t.Fatal("activation", err)
		}
		if principal, err := repo.LookupKey(ctx, keyDigest); err != nil || principal.ID != other {
			t.Fatal("resumed app key unavailable", err)
		}
		var keyActive bool
		if err := owner.QueryRowContext(ctx, `SELECT active FROM api_keys WHERE id=$1`, id(399)).Scan(&keyActive); err != nil || !keyActive {
			t.Fatal("app status changed an existing key", err)
		}
		if err := repo.SetAppActive(ctx, "operator", id(9999), false); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("missing app status", err)
		}
		if _, err := owner.ExecContext(ctx, `UPDATE runtime_gate SET mode='closed' WHERE singleton`); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetAppActive(ctx, "operator", other, false); !errors.Is(err, ErrRuntimeClosed) {
			t.Fatal("status changed with gate closed", err)
		}
		if err := owner.QueryRowContext(ctx, `SELECT active FROM apps WHERE id=$1`, other).Scan(&active); err != nil || !active {
			t.Fatal("closed gate changed app status", err)
		}
		if _, err := owner.ExecContext(ctx, `UPDATE runtime_gate SET mode='open' WHERE singleton`); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("inactive apps stop new preparation claims and SMTP permits", func(t *testing.T) {
		queued, prepared, inactiveDelivery := id(400), id(401), id(402)
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`UPDATE apps SET active=false WHERE id=$1`, []any{other}},
			{`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$3,'inactive-queued',true,'queued'),($2,$3,'inactive-prepared',true,'prepared')`, []any{queued, prepared, other}},
			{`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'inactive-signed')`, []any{inactiveDelivery, prepared}},
		} {
			if _, err := owner.ExecContext(ctx, statement.query, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := repo.ClaimPreparation(ctx, id(403), 30*time.Second); !errors.Is(err, preparation.ErrNoWork) {
			t.Fatal("inactive app preparation claimed", err)
		}
		if _, err := repo.ClaimDelivery(ctx, lease, id(404), claimCaps()); !errors.Is(err, delivery.ErrNoWork) {
			t.Fatal("inactive app delivery claimed", err)
		}
		if _, err := owner.ExecContext(ctx, `UPDATE apps SET active=true WHERE id=$1`, other); err != nil {
			t.Fatal(err)
		}
		prep, err := repo.ClaimPreparation(ctx, id(403), 30*time.Second)
		if err != nil || prep.MessageID != queued {
			t.Fatal("active app preparation unavailable", err)
		}
		c, err := repo.ClaimDelivery(ctx, lease, id(404), claimCaps())
		if err != nil || c.DeliveryID != inactiveDelivery {
			t.Fatal("active app delivery unavailable", err)
		}
		// Pause an uncommitted deactivation while the authorization races for
		// the same app row; the committed deactivation must win.
		tx, err := owner.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `UPDATE apps SET active=false WHERE id=$1`, other); err != nil {
			t.Fatal(err)
		}
		authorized := make(chan error, 1)
		go func() {
			authorized <- repo.AuthorizeDeliveryStart(ctx, lease, c.AttemptID, inactiveDelivery, id(405), id(406), [32]byte{1}, 0)
		}()
		select {
		case err = <-authorized:
			t.Fatal("authorization bypassed app lock", err)
		case <-time.After(100 * time.Millisecond):
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err = <-authorized; !errors.Is(err, delivery.ErrAppInactive) {
			t.Fatal("permit accepted after deactivation", err)
		}
		var permitted bool
		if err = owner.QueryRowContext(ctx, `SELECT start_authorized FROM delivery_attempts WHERE id=$1`, c.AttemptID).Scan(&permitted); err != nil || permitted {
			t.Fatal("inactive app permit persisted", err)
		}
		// The disconnected worker can replay its pre-permit rejection; it must
		// remain queued but unclaimable until the app is active again.
		cancelled := delivery.Result{AttemptID: c.AttemptID, DeliveryID: inactiveDelivery, WorkerID: lease.WorkerID, Epoch: lease.Epoch, JournalID: id(406), Outcome: "rejected"}
		if err = repo.CommitDeliveryResult(ctx, cancelled); err != nil {
			t.Fatal("pre-permit rejection was not recorded", err)
		}
		if _, err = owner.ExecContext(ctx, `UPDATE deliveries SET retry_after=now()-interval '1 second' WHERE id=$1`, inactiveDelivery); err != nil {
			t.Fatal(err)
		}
		if _, err = repo.ClaimDelivery(ctx, lease, id(407), claimCaps()); !errors.Is(err, delivery.ErrNoWork) {
			t.Fatal("inactive queued delivery retried", err)
		}
		if _, err = owner.ExecContext(ctx, `UPDATE apps SET active=true WHERE id=$1`, other); err != nil {
			t.Fatal(err)
		}
		c, err = repo.ClaimDelivery(ctx, lease, id(407), claimCaps())
		if err != nil {
			t.Fatal("reactivated delivery unavailable", err)
		}
		if err = repo.AuthorizeDeliveryStart(ctx, lease, c.AttemptID, inactiveDelivery, id(405), id(406), [32]byte{1}, 0); err != nil {
			t.Fatal("active app permit", err)
		}
		if _, err = owner.ExecContext(ctx, `UPDATE apps SET active=false WHERE id=$1`, other); err != nil {
			t.Fatal(err)
		}
		if err = repo.AuthorizeDeliveryStart(ctx, lease, c.AttemptID, inactiveDelivery, id(405), id(406), [32]byte{1}, 0); !errors.Is(err, delivery.ErrAppInactive) {
			t.Fatal("reissued permit after deactivation", err)
		}
		accepted := delivery.Result{AttemptID: c.AttemptID, DeliveryID: inactiveDelivery, WorkerID: lease.WorkerID, Epoch: lease.Epoch, JournalID: id(406), Outcome: "accepted", SMTPCode: 250}
		if err = repo.CommitDeliveryResult(ctx, accepted); err != nil {
			t.Fatal("previously authorized result was lost after deactivation", err)
		}
	})
}
