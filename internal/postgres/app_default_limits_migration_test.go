package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMigration050 applies migrations through 49 over an existing app, then
// migrates: the existing app keeps its zero limits (no backfill), and a new app
// gets the working defaults with a full submission bucket.
func TestMigration050(t *testing.T) {
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
	schema := "migration50_" + hex.EncodeToString(random[:])
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
	if migrations[49].version != 50 {
		t.Fatal("this test targets migration 49")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:49] {
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
	old := strings.Repeat("a", 32)
	if _, err = tx.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('`+old+`','Old')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	const columns = `submission_rate_per_minute,submission_burst,submission_tokens,recipient_daily_limit,pending_message_limit,tracking_daily_limit`
	var got [6]int64
	scan := func(id string) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT `+columns+` FROM apps WHERE id=$1`, id).Scan(&got[0], &got[1], &got[2], &got[3], &got[4], &got[5]); err != nil {
			t.Fatal(err)
		}
	}
	scan(old)
	if got != [6]int64{} {
		t.Error("an existing app keeps its zero limits", got)
	}
	fresh := strings.Repeat("b", 32)
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'New')`, fresh); err != nil {
		t.Fatal(err)
	}
	scan(fresh)
	if got != [6]int64{60, 100, 100 * 60_000_000, 10000, 1000, 100000} {
		t.Error("a new app gets the working defaults and a full bucket", got)
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil || version != len(migrations) {
		t.Fatal("schema version", version, err)
	}
}
