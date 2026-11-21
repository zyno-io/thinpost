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

// TestMigration046 applies migrations through 45 over verified and pending
// sender domains, then migrates: verified domains stay verified (and have no
// key), the _thinpost challenge column is gone, and the key table enforces its
// rules.
func TestMigration046(t *testing.T) {
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
	schema := "migration46_" + hex.EncodeToString(random[:])
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
	if migrations[45].version != 46 {
		t.Fatal("this test targets migration 46")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:45] {
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
		`INSERT INTO apps(id,name) VALUES ('` + app + `','A')`,
		`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ('` + app + `','verified.example',true,now())`,
		`INSERT INTO sender_domains(app_id,domain,active,verification_hash) VALUES ('` + app + `','pending.example',false,decode(repeat('ab',32),'hex'))`,
	} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var active bool
	var verified sql.NullTime
	if err = db.QueryRowContext(ctx, `SELECT active,verified_at FROM sender_domains WHERE domain='verified.example'`).Scan(&active, &verified); err != nil || !active || !verified.Valid {
		t.Fatal("a verified domain lost its verification", err)
	}
	if err = db.QueryRowContext(ctx, `SELECT active FROM sender_domains WHERE domain='pending.example'`).Scan(&active); err != nil || active {
		t.Fatal("a pending domain changed", err)
	}
	var columns, keys int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='sender_domains' AND column_name='verification_hash'`, schema).Scan(&columns); err != nil || columns != 0 {
		t.Fatal("the _thinpost challenge column remains", columns, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM dkim_keys`).Scan(&keys); err != nil || keys != 0 {
		t.Fatal("existing domains must start without keys", keys, err)
	}
	// Keys can be written for either domain, and the new rules hold.
	insert := func(id, domain, selector, state, sealed string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO dkim_keys(id,app_id,domain,selector,public_key,sealed_private_key,state,activated_at,retired_at,keep_record_until)
			VALUES ($1,$2,$3,$4,decode(repeat('ab',270),'hex'),`+sealed+`,$5,CASE WHEN $5<>'pending' THEN now() END,CASE WHEN $5='retired' THEN now() END,CASE WHEN $5='retired' THEN now() END)`, id, app, domain, selector, state)
		return err
	}
	const sealed = `decode(repeat('cd',100),'hex')`
	if err = insert(strings.Repeat("1", 32), "verified.example", "thinpost1", "active", sealed); err != nil {
		t.Fatal(err)
	}
	if err = insert(strings.Repeat("2", 32), "verified.example", "thinpost2", "pending", sealed); err != nil {
		t.Fatal(err)
	}
	if err = insert(strings.Repeat("3", 32), "verified.example", "thinpost3", "retired", "NULL"); err != nil {
		t.Fatal("a shredded retired key", err)
	}
	for name, err := range map[string]error{
		"second active":                insert(strings.Repeat("4", 32), "verified.example", "thinpost4", "active", sealed),
		"second pending":               insert(strings.Repeat("5", 32), "verified.example", "thinpost5", "pending", sealed),
		"selector reused by a row":     insert(strings.Repeat("6", 32), "verified.example", "thinpost3", "retired", "NULL"),
		"retired but not shredded":     insert(strings.Repeat("7", 32), "verified.example", "thinpost6", "retired", sealed),
		"active but shredded":          insert(strings.Repeat("8", 32), "pending.example", "thinpost1", "active", "NULL"),
		"bad selector":                 insert(strings.Repeat("9", 32), "pending.example", "Bad_Selector", "pending", sealed),
		"domain the app does not have": insert(strings.Repeat("e", 32), "missing.example", "thinpost1", "pending", sealed),
	} {
		if err == nil {
			t.Error("accepted:", name)
		}
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil || version != len(migrations) {
		t.Fatal("schema version", version, err)
	}
}
