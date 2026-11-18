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
	"github.com/zyno-io/thinpost/internal/tracking"
)

func TestTrackedDeliveryHoldsAndRechecksBeforePermit(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	schema := "tracking_send_" + hex.EncodeToString(random[:])
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
	if _, err = owner.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:22] {
		body, checksum, readErr := migrationBody(item)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, err = owner.ExecContext(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
		if _, err = owner.ExecContext(ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, item.version, checksum); err != nil {
			t.Fatal(err)
		}
	}
	const generation = "00000000000000000000000000000aaa"
	id := func(n int) string { return fmt.Sprintf("%032x", n) }
	app, subscription := id(1), id(2)
	if _, err = owner.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'fixture')`, app); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ref) VALUES ($2,$1,'https://example.net/events','v1')`, app, subscription); err != nil {
		t.Fatal(err)
	}
	legacyKey, err := tracking.NewMappingKey(id(24), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,require_tls,state) VALUES ($1,$2,'source/legacy',false,'prepared')`, id(14), app); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,retry_after) VALUES ($1,$2,0,'final/legacy',clock_timestamp()-interval '150 minutes')`, id(24), id(14)); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO tracking_objects(delivery_id,object_key,expires_at,event_limit) VALUES ($1,$2,clock_timestamp()+interval '1 hour',10)`, id(24), legacyKey); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, owner); err != nil {
		t.Fatal("send-window migration", err)
	}
	// Migration 29 retires catalog-era endpoints; revive the fixture as a live one.
	if _, err = owner.ExecContext(ctx, `UPDATE webhook_subscriptions SET active=true,deleted_at=NULL,secret_ciphertext='\x01'::bytea`); err != nil {
		t.Fatal(err)
	}
	var legacyValidity sql.NullInt64
	if err = owner.QueryRowContext(ctx, `SELECT post_send_validity_seconds FROM tracking_objects WHERE delivery_id=$1`, id(24)).Scan(&legacyValidity); err != nil || legacyValidity.Valid {
		t.Fatal("migration assigned a guessed legacy lifetime", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 4; n++ {
		messageID, deliveryID := id(10+n), id(20+n)
		recipientIndex := 0
		if n == 1 {
			messageID = id(10)
			recipientIndex = 1
		}
		key, keyErr := tracking.NewMappingKey(deliveryID, time.Now())
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		if n != 1 {
			if _, err = owner.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,$3,false,'prepared')`, messageID, app, "source/"+messageID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = owner.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,retry_after) VALUES ($1,$2,$5,$3,clock_timestamp()-$4*interval '1 hour')`, deliveryID, messageID, "final/"+deliveryID, 4-n, recipientIndex); err != nil {
			t.Fatal(err)
		}
		if _, err = owner.ExecContext(ctx, `INSERT INTO tracking_objects(delivery_id,object_key,expires_at,event_limit,revoked_at,post_send_validity_seconds) VALUES ($1,$2,clock_timestamp()+$3*interval '1 minute',10,CASE WHEN $4 THEN clock_timestamp() ELSE NULL END,$5)`, deliveryID, key, []int{1441, 60, 300, 300}[n], n == 1, []int{86400, 60, 60, 60}[n]); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []struct {
		delivery any
		code     int
	}{{id(20), 250}, {nil, 0}} {
		if _, err = owner.ExecContext(ctx, `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,outcome,smtp_code) VALUES ($1,$2,$3,$4,'tracking_abandoned',$5)`, id(90), app, id(10), invalid.delivery, invalid.code); err == nil {
			t.Fatal("invalid tracking abandonment event was accepted")
		}
	}
	runtime, cleanup := restrictedRuntimeForTest(t, ctx, owner, u.String())
	defer cleanup()
	repo := &Repository{DB: runtime, Generation: generation}
	lease, err := repo.AcquireControllerLease(ctx, "worker", "controller", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `CREATE FUNCTION fail_tracking_hold() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.outcome='tracking_unavailable' THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_tracking_hold BEFORE INSERT ON webhook_events FOR EACH ROW EXECUTE FUNCTION fail_tracking_hold()`); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ClaimDelivery(ctx, lease, id(100), delivery.EdgeCapabilities{}); err == nil {
		t.Fatal("hold committed without its webhook event")
	}
	var state string
	var count int
	if err = owner.QueryRowContext(ctx, `SELECT state,(SELECT count(*) FROM delivery_attempts WHERE id=$2) FROM deliveries WHERE id=$1`, id(20), id(100)).Scan(&state, &count); err != nil || state != "queued" || count != 0 {
		t.Fatal("failed hold did not roll back", state, count, err)
	}
	if _, err = owner.ExecContext(ctx, `DROP TRIGGER fail_tracking_hold ON webhook_events; DROP FUNCTION fail_tracking_hold()`); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if _, err = repo.ClaimDelivery(ctx, lease, id(101+n), delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrNoWork) {
			t.Fatal("unusable tracking was sent", err)
		}
		var pending, authorized bool
		var outcome string
		if err = owner.QueryRowContext(ctx, `SELECT d.state,m.pending,a.start_authorized,e.outcome,(SELECT count(*) FROM webhook_deliveries WHERE event_id=e.id AND subscription_id=$3) FROM deliveries d JOIN messages m ON m.id=d.message_id JOIN delivery_attempts a ON a.delivery_id=d.id JOIN webhook_events e ON e.attempt_id=a.id WHERE d.id=$1 AND a.id=$2`, id(20+n), id(101+n), subscription).Scan(&state, &pending, &authorized, &outcome, &count); err != nil || state != "tracking_held" || !pending || authorized || outcome != delivery.OutcomeTrackingUnavailable || count != 1 {
			t.Fatal("invalid tracking hold", state, pending, authorized, outcome, count, err)
		}
	}
	if _, err = repo.ClaimDelivery(ctx, lease, id(109), delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrNoWork) {
		t.Fatal("legacy mapping without a post-send lifetime was sent", err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE id=$1`, id(24)).Scan(&state); err != nil || state != "tracking_held" {
		t.Fatal("legacy tracking mapping was not held", state, err)
	}
	if err = repo.AbandonTrackingHold(ctx, "operator", id(999), id(10), id(20)); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("cross-app abandonment was accepted", err)
	}
	if err = repo.AbandonTrackingHold(ctx, "operator", app, id(12), id(22)); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("unheld delivery was abandoned", err)
	}
	// A valid hold does not prove that an earlier uncertain or terminal SMTP
	// attempt was resolved. Inconsistent or incomplete history stays held.
	if _, err = owner.ExecContext(ctx, `CREATE FUNCTION fail_tracking_abandon() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.outcome='tracking_abandoned' THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_tracking_abandon BEFORE INSERT ON webhook_events FOR EACH ROW EXECUTE FUNCTION fail_tracking_abandon()`); err != nil {
		t.Fatal(err)
	}
	if err = repo.AbandonTrackingHold(ctx, "operator", app, id(10), id(20)); err == nil {
		t.Fatal("abandonment succeeded without its event")
	}
	var pending bool
	if err = owner.QueryRowContext(ctx, `SELECT d.state,m.pending,(SELECT count(*) FROM audit_events WHERE action='delivery.tracking.abandon' AND resource_id=d.id) FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE d.id=$1`, id(20)).Scan(&state, &pending, &count); err != nil || state != "tracking_held" || !pending || count != 0 {
		t.Fatal("failed abandonment did not roll back", state, pending, count, err)
	}
	if _, err = owner.ExecContext(ctx, `DROP TRIGGER fail_tracking_abandon ON webhook_events; DROP FUNCTION fail_tracking_abandon()`); err != nil {
		t.Fatal(err)
	}
	// Inactive apps can still dispose of held work without resuming delivery.
	if _, err = owner.ExecContext(ctx, `UPDATE apps SET active=false WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- repo.AbandonTrackingHold(ctx, "operator", app, id(10), id(20)) }()
	}
	wg.Wait()
	close(results)
	var succeeded, conflicted int
	for result := range results {
		if result == nil {
			succeeded++
		} else if errors.Is(result, admin.ErrConflict) {
			conflicted++
		} else {
			t.Fatal("concurrent abandonment", result)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatal("abandonment did not serialize", succeeded, conflicted)
	}
	var eventID string
	if err = owner.QueryRowContext(ctx, `SELECT d.state,m.pending,e.id,(SELECT count(*) FROM audit_events WHERE actor_id='operator' AND action='delivery.tracking.abandon' AND resource_id=d.id),(SELECT count(*) FROM webhook_deliveries WHERE event_id=e.id AND subscription_id=$2) FROM deliveries d JOIN messages m ON m.id=d.message_id JOIN webhook_events e ON e.delivery_id=d.id AND e.outcome='tracking_abandoned' WHERE d.id=$1`, id(20), subscription).Scan(&state, &pending, &eventID, &count, &succeeded); err != nil || state != "failed" || !pending || count != 1 || succeeded != 1 {
		t.Fatal("abandonment did not persist audit, event and sibling pending state", state, pending, count, succeeded, err)
	}
	if err = repo.AbandonTrackingHold(ctx, "operator", app, id(10), id(21)); err != nil {
		t.Fatal("could not abandon sibling hold", err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT pending FROM messages WHERE id=$1`, id(10)).Scan(&pending); err != nil || pending {
		t.Fatal("terminal siblings did not release pending capacity", pending, err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE apps SET active=true WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE webhook_deliveries SET next_attempt_at=clock_timestamp()-interval '1 hour' WHERE event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	hook, err := repo.ClaimWebhook(ctx, id(108), time.Minute)
	if err != nil || hook.Event.ID != eventID || hook.Event.Outcome != "tracking_abandoned" || hook.Event.DeliveryID != id(20) || hook.Event.AttemptID != "" || hook.Event.SMTPCode != 0 {
		t.Fatal("tracking abandonment event could not be dispatched", err)
	}
	if err = repo.CommitWebhook(ctx, hook, 204); err != nil {
		t.Fatal("tracking abandonment webhook result", err)
	}
	claim, err := repo.ClaimDelivery(ctx, lease, id(103), delivery.EdgeCapabilities{})
	if err != nil || claim.DeliveryID != id(22) {
		t.Fatal("valid mapping not claimed", err)
	}
	if claim.TrackingSendCutoff.IsZero() {
		t.Fatal("tracked claim omitted its expiry")
	}
	if err = repo.AbandonTrackingHold(ctx, "operator", app, id(12), id(22)); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("in-flight attempt was abandoned", err)
	}
	for _, expiry := range []int64{0, claim.TrackingSendCutoff.UnixMilli() + 1} {
		if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, claim.DeliveryID, id(104), id(105), [32]byte{1}, expiry); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("mismatched tracking expiry received a permit", err)
		}
	}
	if _, err = owner.ExecContext(ctx, `UPDATE tracking_objects SET expires_at=clock_timestamp()+interval '1 minute' WHERE delivery_id=$1`, claim.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, claim.DeliveryID, id(104), id(105), [32]byte{1}, 0); !errors.Is(err, delivery.ErrTrackingUnavailable) {
		t.Fatal("expired mapping received permit", err)
	}
	var authorized bool
	if err = owner.QueryRowContext(ctx, `SELECT start_authorized FROM delivery_attempts WHERE id=$1`, claim.AttemptID).Scan(&authorized); err != nil || authorized {
		t.Fatal("expired mapping persisted a permit", err)
	}
	claim, err = repo.ClaimDelivery(ctx, lease, id(106), delivery.EdgeCapabilities{})
	if err != nil || claim.DeliveryID != id(23) {
		t.Fatal("second valid mapping not claimed", err)
	}
	if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, claim.DeliveryID, id(104), id(105), [32]byte{1}, claim.TrackingSendCutoff.UnixMilli()); err != nil {
		t.Fatal("valid mapping denied", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE tracking_objects SET revoked_at=clock_timestamp() WHERE delivery_id=$1`, claim.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, claim.DeliveryID, id(104), id(105), [32]byte{1}, 0); !errors.Is(err, delivery.ErrTrackingUnavailable) {
		t.Fatal("revoked mapping received a repeated permit", err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT start_authorized FROM delivery_attempts WHERE id=$1`, claim.AttemptID).Scan(&authorized); err != nil || !authorized {
		t.Fatal("revocation incorrectly erased an issued permit", err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'source/untracked',false,'prepared')`, id(30), app); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'final/untracked')`, id(31), id(30)); err != nil {
		t.Fatal(err)
	}
	untracked, err := repo.ClaimDelivery(ctx, lease, id(107), delivery.EdgeCapabilities{})
	if err != nil || !untracked.TrackingSendCutoff.IsZero() {
		t.Fatal("untracked claim included an expiry", err)
	}
	if err = repo.AuthorizeDeliveryStart(ctx, lease, untracked.AttemptID, untracked.DeliveryID, id(104), id(105), [32]byte{1}, time.Now().Add(time.Hour).UnixMilli()); !errors.Is(err, delivery.ErrResultConflict) {
		t.Fatal("untracked message accepted a fabricated expiry", err)
	}
	if err = repo.AuthorizeDeliveryStart(ctx, lease, untracked.AttemptID, untracked.DeliveryID, id(104), id(105), [32]byte{1}, 0); err != nil {
		t.Fatal("untracked message was denied", err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'source/corrupt',false,'prepared')`, id(40), app); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ($1,$2,0,'final/corrupt','tracking_held')`, id(41), id(40)); err != nil {
		t.Fatal(err)
	}
	if err = repo.AbandonTrackingHold(ctx, "operator", app, id(40), id(41)); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("tracking hold without pre-SMTP evidence was abandoned", err)
	}
}
