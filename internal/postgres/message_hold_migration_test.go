package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"
)

// TestMigration042 applies migrations through 41 over existing messages and
// events of every kind, then migrates: nothing existing changes, the new
// CHECKs are not validated against old rows (no table scan) but bind new
// writes, and a held message and its message-level events can be written.
func TestMigration042(t *testing.T) {
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
	schema := "migration42_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if migrations[41].version != 42 {
		t.Fatal("this test targets migration 42")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:41] {
		body, checksum, err := migrationBody(item)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, item.version, checksum); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('app','test');
		INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('queued','app','k1',false,'queued'),('prepared','app','k2',false,'prepared'),('failed','app','k3',false,'preparation_failed');
		INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ('d','prepared',0,'f');
		INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch) VALUES ('a','d','w',1);
		INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ('e1','app','prepared','d','a','accepted',250);
		INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ('e2','app','failed','preparation_failed',0)`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var changed, events int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE hold_expires_at IS NOT NULL OR hold_resolved_at IS NOT NULL),
		(SELECT count(*) FROM webhook_events) FROM messages`).Scan(&changed, &events); err != nil || changed != 0 || events != 2 {
		t.Fatal("existing rows changed", changed, events, err)
	}
	var unvalidated int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint WHERE conname IN ('messages_hold_resolved_needs_hold','messages_hold_states_need_hold','messages_hold_open_unresolved','webhook_events_exactly_one_source') AND NOT convalidated AND connamespace=$1::regnamespace`, schema).Scan(&unvalidated); err != nil || unvalidated != 4 {
		t.Fatal("constraints must be added without scanning", unvalidated, err)
	}
	for name, statement := range map[string]string{
		"on_hold without expiry":    `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('bad1','app','k4',false,'on_hold')`,
		"cancelled without hold":    `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('bad2','app','k5',false,'cancelled')`,
		"resolved without hold":     `UPDATE messages SET hold_resolved_at=now() WHERE id='queued'`,
		"resolved but still held":   `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state,hold_expires_at,hold_resolved_at) VALUES ('bad3','app','k6',false,'on_hold',now(),now())`,
		"hold outcome on attempt":   `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ('bad4','app','prepared','d','a','released',0)`,
		"hold outcome with a code":  `INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ('bad5','app','queued','held',250)`,
		"unknown message-level one": `INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ('bad6','app','queued','accepted',0)`,
	} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state,hold_expires_at) VALUES ('held','app','k7',false,'on_hold',now()+interval '7 days');
		INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ('h1','app','held','held',0),('h2','app','held','released',0),('h3','app','held','cancelled',0),('h4','app','held','hold_expired',0);
		UPDATE messages SET state='cancelled',pending=false,hold_resolved_at=now() WHERE id='held'`); err != nil {
		t.Fatal("held message and its events", err)
	}
	var indexed bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname=$1 AND indexname='messages_hold_expiry_idx')`, schema).Scan(&indexed); err != nil || !indexed {
		t.Fatal("expiry index missing", err)
	}
}
