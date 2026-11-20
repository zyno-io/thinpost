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

// TestMigration043 applies migrations through 42 over existing messages, then
// migrates: existing rows read false in all three new columns, the CHECK is
// not validated against old rows (no table scan) but binds new writes, and the
// schema version is at least 43.
func TestMigration043(t *testing.T) {
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
	schema := "migration43_" + hex.EncodeToString(random[:])
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
	if migrations[42].version != 43 {
		t.Fatal("this test targets migration 43")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:42] {
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
		INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('queued','app','k1',false,'queued'),('prepared','app','k2',false,'prepared'),('failed','app','k3',false,'preparation_failed')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var rows, flagged int
	if err = db.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE console_test OR limits_override OR recipient_policy_override) FROM messages`).Scan(&rows, &flagged); err != nil || rows != 3 || flagged != 0 {
		t.Fatal("existing rows changed", rows, flagged, err)
	}
	var unvalidated int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint WHERE conname='messages_overrides_console' AND NOT convalidated AND connamespace=$1::regnamespace`, schema).Scan(&unvalidated); err != nil || unvalidated != 1 {
		t.Fatal("constraint must be added without scanning", unvalidated, err)
	}
	for name, statement := range map[string]string{
		"limits override without console":    `INSERT INTO messages(id,app_id,object_key,enhanced_protection,limits_override) VALUES ('bad1','app','k4',false,true)`,
		"recipient override without console": `INSERT INTO messages(id,app_id,object_key,enhanced_protection,recipient_policy_override) VALUES ('bad2','app','k5',false,true)`,
		"override added to an old row":       `UPDATE messages SET limits_override=true WHERE id='queued'`,
	} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,console_test,limits_override,recipient_policy_override) VALUES ('ok1','app','k6',false,true,true,true);
		INSERT INTO messages(id,app_id,object_key,enhanced_protection,console_test) VALUES ('ok2','app','k7',false,true)`); err != nil {
		t.Fatal("console test rows", err)
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil || version < 43 {
		t.Fatal("schema version", version, err)
	}
}
