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

// TestMigration053 applies migrations through 52 over existing sender domains,
// then migrates: existing domains stay as they were (removed_at is NULL), and
// the CHECK makes a removed domain inactive.
func TestMigration053(t *testing.T) {
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
	schema := "migration53_" + hex.EncodeToString(random[:])
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
	if migrations[52].version != 53 {
		t.Fatal("this test targets migration 53")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:52] {
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
	app := strings.Repeat("a", 32)
	for _, statement := range []string{
		`INSERT INTO apps(id,name) VALUES ('` + app + `','App')`,
		`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ('` + app + `','live.example',true,now())`,
		`INSERT INTO sender_domains(app_id,domain,active) VALUES ('` + app + `','pending.example',false)`,
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
	var removed int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM sender_domains WHERE removed_at IS NOT NULL`).Scan(&removed); err != nil || removed != 0 {
		t.Fatal("existing domains are not removed", removed, err)
	}
	var active bool
	if err = db.QueryRowContext(ctx, `SELECT active FROM sender_domains WHERE domain='live.example'`).Scan(&active); err != nil || !active {
		t.Fatal("a live domain stays active", active, err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE sender_domains SET removed_at=now() WHERE domain='live.example'`); err == nil {
		t.Error("a removed domain may be active")
	}
	if _, err = db.ExecContext(ctx, `UPDATE sender_domains SET active=false,removed_at=now() WHERE domain='live.example'`); err != nil {
		t.Fatal("an inactive domain may be removed", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE sender_domains SET active=true WHERE domain='live.example'`); err == nil {
		t.Error("a removed domain may be reactivated")
	}
	if _, err = db.ExecContext(ctx, `UPDATE sender_domains SET removed_at=NULL,verified_at=NULL WHERE domain='live.example'`); err != nil {
		t.Fatal("a removed domain may be revived", err)
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil || version != len(migrations) {
		t.Fatal("schema version", version, err)
	}
}
