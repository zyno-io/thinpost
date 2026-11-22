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

// TestMigration049 applies migrations through 48 over existing apps, then
// migrates: existing apps stay live (deleted_at is NULL), and the CHECK makes
// a deleted app inactive.
func TestMigration049(t *testing.T) {
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
	schema := "migration49_" + hex.EncodeToString(random[:])
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
	if migrations[48].version != 49 {
		t.Fatal("this test targets migration 49")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:48] {
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
	live, paused := strings.Repeat("a", 32), strings.Repeat("b", 32)
	for _, statement := range []string{
		`INSERT INTO apps(id,name) VALUES ('` + live + `','Live')`,
		`INSERT INTO apps(id,name,active) VALUES ('` + paused + `','Paused',false)`,
	} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var deleted int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM apps WHERE deleted_at IS NOT NULL`).Scan(&deleted); err != nil || deleted != 0 {
		t.Fatal("existing apps are not deleted", deleted, err)
	}
	var active bool
	if err = db.QueryRowContext(ctx, `SELECT active FROM apps WHERE id=$1`, live).Scan(&active); err != nil || !active {
		t.Fatal("a live app stays active", active, err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE apps SET deleted_at=now() WHERE id=$1`, live); err == nil {
		t.Error("a deleted app may be active")
	}
	if _, err = db.ExecContext(ctx, `UPDATE apps SET deleted_at=now() WHERE id=$1`, paused); err != nil {
		t.Fatal("a paused app may be deleted", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE apps SET active=true WHERE id=$1`, paused); err == nil {
		t.Error("a deleted app may be resumed")
	}
	// Names stay non-unique.
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'Live')`, strings.Repeat("c", 32)); err != nil {
		t.Fatal("app names are not unique", err)
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil || version != len(migrations) {
		t.Fatal("schema version", version, err)
	}
}
