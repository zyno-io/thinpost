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

// TestMigration044 applies migrations through 43 over existing messages, then
// migrates: existing rows read NULL in the new columns, the messages
// constraints are not validated against old rows (no table scan) but bind new
// writes, and the schema version is 44.
func TestMigration044(t *testing.T) {
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
	schema := "migration44_" + hex.EncodeToString(random[:])
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
	if migrations[43].version != 44 {
		t.Fatal("this test targets migration 44")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:43] {
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
	var rows, tagged int
	if err = db.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE smtp_credential_id IS NOT NULL OR original_from_address IS NOT NULL) FROM messages`).Scan(&rows, &tagged); err != nil || rows != 3 || tagged != 0 {
		t.Fatal("existing rows changed", rows, tagged, err)
	}
	var unvalidated int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint WHERE conname IN ('messages_smtp_credential_fk','messages_original_from','messages_subject_size','messages_smtp_not_console') AND NOT convalidated AND connamespace=$1::regnamespace`, schema).Scan(&unvalidated); err != nil || unvalidated != 4 {
		t.Fatal("constraints must be added without scanning", unvalidated, err)
	}
	cred := strings.Repeat("c", 32)
	if _, err = db.ExecContext(ctx, `INSERT INTO smtp_credentials(id,app_id,name,digest) VALUES ('`+cred+`','app','Printer',decode(repeat('ab',32),'hex'))`); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"credential of another app":         `INSERT INTO apps(id,name) VALUES ('other','o'); INSERT INTO messages(id,app_id,object_key,enhanced_protection,smtp_credential_id) VALUES ('bad1','other','k4',false,'` + cred + `')`,
		"original From without credential":  `INSERT INTO messages(id,app_id,object_key,enhanced_protection,original_from_address) VALUES ('bad2','app','k5',false,'o@example.net')`,
		"credential on a console test":      `INSERT INTO messages(id,app_id,object_key,enhanced_protection,console_test,smtp_credential_id) VALUES ('bad3','app','k6',false,true,'` + cred + `')`,
		"original From added to an old row": `UPDATE messages SET original_from_address='o@example.net' WHERE id='queued'`,
	} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil || version != 44 {
		t.Fatal("schema version", version, err)
	}
}
