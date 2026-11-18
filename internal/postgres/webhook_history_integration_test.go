package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/admin"
)

func TestWebhookHistoryPostgresIsolationAndReplay(t *testing.T) {
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
	schema := "webhook_history_" + hex.EncodeToString(random[:])
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
	for _, item := range migrations[:16] {
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
	if err = CheckSchema(ctx, owner); err == nil {
		t.Fatal("mail runtime accepted schema 16")
	}
	const generation = "00000000000000000000000000000aaa"
	const endpoint = "https://hooks.example.com/events"
	id := func(n int) string { return fmt.Sprintf("%032x", n) }
	app, other, msg, otherMsg, event, otherEvent, sub, sub2, otherSub := id(1), id(2), id(3), id(4), id(5), id(6), id(7), id(8), id(9)
	statements := []struct {
		query string
		args  []any
	}{
		{`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, []any{generation}},
		{`INSERT INTO apps(id,name) VALUES ($1,'One'),($2,'Other')`, []any{app, other}},
		{`INSERT INTO messages(id,app_id,object_key,require_tls) VALUES ($1,$2,'first',true),($3,$4,'second',true)`, []any{msg, app, otherMsg, other}},
		{`INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ref) VALUES ($1,$4,$6,'private-ref'),($2,$4,$6,'private-ref'),($3,$5,$6,'private-ref')`, []any{sub, sub2, otherSub, app, other, endpoint}},
		{`INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code,occurred_at) VALUES ($1,$2,$3,'preparation_failed',0,'2026-01-01'),($4,$5,$6,'preparation_failed',0,'2026-01-01')`, []any{event, app, msg, otherEvent, other, otherMsg}},
		{`INSERT INTO webhook_deliveries(event_id,subscription_id,app_id,state,attempts) VALUES ($1,$2,$4,'failed',12),($1,$3,$4,'failed',12),($5,$6,$7,'failed',12)`, []any{event, sub, sub2, app, otherEvent, otherSub, other}},
		{`INSERT INTO webhook_attempts(id,event_id,subscription_id,state,http_status,started_at,finished_at) SELECT md5('attempt'||n),$1,$2,'failed',500,now()+n*interval '1 second',now()+n*interval '1 second' FROM generate_series(1,51) n`, []any{event, sub}},
	}
	for _, statement := range statements {
		if _, err = owner.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = Migrate(ctx, owner); err == nil {
		t.Fatal("migration accepted an open gate")
	}
	if _, err = owner.ExecContext(ctx, `UPDATE runtime_gate SET mode='closed' WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, owner); err != nil {
		t.Fatal("migration 17", err)
	}
	if err = CheckSchema(ctx, owner); err != nil {
		t.Fatal("schema 17", err)
	}
	// Migration 29 retires catalog-era endpoints; revive the fixtures as live ones.
	if _, err = owner.ExecContext(ctx, `UPDATE webhook_subscriptions SET active=true,deleted_at=NULL,secret_ciphertext='\x01'::bytea`); err != nil {
		t.Fatal(err)
	}
	var indexes int
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname IN ('webhook_events_app_time_idx','webhook_attempts_delivery_time_idx')`).Scan(&indexes); err != nil || indexes != 2 {
		t.Fatal("migration indexes", indexes, err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE runtime_gate SET mode='open' WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	runtimeDB, cleanup := restrictedRuntimeForTest(t, ctx, owner, u.String())
	defer cleanup()
	repo := &Repository{DB: runtimeDB, Generation: generation}
	first, err := repo.ListWebhookDeliveries(ctx, "operator", app, "", 1)
	if err != nil || len(first.Deliveries) != 1 || first.NextCursor == "" {
		t.Fatal("first page", first, err)
	}
	second, err := repo.ListWebhookDeliveries(ctx, "operator", app, first.NextCursor, 1)
	if err != nil || len(second.Deliveries) != 1 || second.NextCursor != "" || first.Deliveries[0].SubscriptionID == second.Deliveries[0].SubscriptionID {
		t.Fatal("pagination lost or duplicated delivery", second, err)
	}
	if _, err = repo.ListWebhookDeliveries(ctx, "operator", app, "invalid", 1); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("invalid cursor accepted", err)
	}
	if _, err = repo.ListWebhookAttempts(ctx, "operator", other, event, sub); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("cross-app attempts accessible", err)
	}
	attempts, err := repo.ListWebhookAttempts(ctx, "operator", app, event, sub)
	if err != nil || len(attempts.Attempts) != 50 || !attempts.Truncated || !attempts.Attempts[0].StartedAt.After(attempts.Attempts[49].StartedAt) {
		t.Fatal("attempt history", len(attempts.Attempts), err)
	}
	data, err := json.Marshal(attempts)
	if err != nil || strings.Contains(string(data), `"id"`) || strings.Contains(string(data), "private-ref") {
		t.Fatal("attempt claim token or reference exposed", err)
	}
	if err = repo.ReplayWebhookDelivery(ctx, "operator", other, event, otherSub); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("cross-app replay accepted", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE webhook_subscriptions SET active=false,deleted_at=now(),secret_ciphertext=NULL WHERE id=$1`, sub); err != nil {
		t.Fatal(err)
	}
	if err = repo.ReplayWebhookDelivery(ctx, "operator", app, event, sub); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("deleted endpoint accepted", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE webhook_subscriptions SET active=true,deleted_at=NULL,secret_ciphertext='\x01'::bytea WHERE id=$1`, sub); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE webhook_subscriptions SET active=false WHERE id=$1`, sub); err != nil {
		t.Fatal(err)
	}
	if err = repo.ReplayWebhookDelivery(ctx, "operator", app, event, sub); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("inactive subscription accepted", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE webhook_subscriptions SET active=true WHERE id=$1`, sub); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE apps SET active=false WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	if err = repo.ReplayWebhookDelivery(ctx, "operator", app, event, sub); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("inactive app accepted", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE apps SET active=true WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `CREATE FUNCTION fail_replay_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='webhook.delivery.replay' THEN RAISE EXCEPTION 'test audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_replay_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_replay_audit()`); err != nil {
		t.Fatal(err)
	}
	if err = repo.ReplayWebhookDelivery(ctx, "operator", app, event, sub); err == nil {
		t.Fatal("replay succeeded despite failed audit")
	}
	var state string
	var count int
	if err = owner.QueryRowContext(ctx, `SELECT state,attempts FROM webhook_deliveries WHERE event_id=$1 AND subscription_id=$2`, event, sub).Scan(&state, &count); err != nil || state != "failed" || count != 12 {
		t.Fatal("failed audit did not roll back replay", state, count, err)
	}
	if _, err = owner.ExecContext(ctx, `DROP TRIGGER fail_replay_audit ON audit_events; DROP FUNCTION fail_replay_audit()`); err != nil {
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
			results <- repo.ReplayWebhookDelivery(ctx, "operator", app, event, sub)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var success, conflict int
	for result := range results {
		if result == nil {
			success++
		} else if errors.Is(result, admin.ErrConflict) {
			conflict++
		} else {
			t.Fatal("unexpected replay error", result)
		}
	}
	if success != 1 || conflict != workers-1 {
		t.Fatal("concurrent replay", success, conflict)
	}
	var retained, audits int
	if err = owner.QueryRowContext(ctx, `SELECT state,attempts,(SELECT count(*) FROM webhook_attempts WHERE event_id=$1 AND subscription_id=$2),(SELECT count(*) FROM audit_events WHERE action='webhook.delivery.replay') FROM webhook_deliveries WHERE event_id=$1 AND subscription_id=$2`, event, sub).Scan(&state, &count, &retained, &audits); err != nil || state != "queued" || count != 0 || retained != 51 || audits != 1 {
		t.Fatal("replay lost history or audit", state, count, retained, audits, err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE runtime_gate SET mode='closed' WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if err = repo.ReplayWebhookDelivery(ctx, "operator", app, event, sub2); err == nil {
		t.Fatal("replay accepted while gate is closed")
	}
}
