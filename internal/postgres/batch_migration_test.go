package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestResendBatchMigrationPreservesSingleMarkers(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx := context.Background()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "batch_migration_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
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
	defer db.Close()
	if _, err = db.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:15] {
		body, checksum, err := migrationBody(migration)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, migration.version, checksum); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('app','App');
		INSERT INTO messages(id,app_id,object_key,require_tls) VALUES ('message','app','source',true);
		INSERT INTO idempotency_keys(app_id,adapter,key,request_hash,message_id,expires_at) VALUES ('app','resend','single',decode(repeat('ab',32),'hex'),'message',now()+interval '24 hours')`); err != nil {
		t.Fatal(err)
	}
	if err = CheckSchema(ctx, db); err == nil {
		t.Fatal("mail runtime accepted incomplete schema 15")
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal("schema 15-to-16 migration failed", err)
	}
	if err = CheckSchema(ctx, db); err != nil {
		t.Fatal("schema 16 verification failed", err)
	}
	var isSingle, hashPreserved bool
	if err = db.QueryRowContext(ctx, `SELECT batch_receipt_ids IS NULL,request_hash=decode(repeat('ab',32),'hex') FROM idempotency_keys WHERE app_id='app' AND adapter='resend' AND key='single' AND message_id='message'`).Scan(&isSingle, &hashPreserved); err != nil || !isSingle || !hashPreserved {
		t.Fatal("migration changed an existing single-message retry marker", err)
	}
}
