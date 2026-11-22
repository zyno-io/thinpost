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

// TestMigration052 applies migrations through 51 over existing deliveries, then
// migrates: they read as format 1 (a full object) and are otherwise untouched,
// the new constraint is NOT VALID (nothing was scanned) yet enforced for new and
// changed rows, and a new delivery defaults to format 1.
func TestMigration052(t *testing.T) {
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
	schema := "migration52_" + hex.EncodeToString(random[:])
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
	if migrations[51].version != 52 {
		t.Fatal("this test targets migration 52")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:51] {
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
	app, message, delivery, second := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32), strings.Repeat("d", 32)
	for _, statement := range []string{
		`INSERT INTO apps(id,name) VALUES ('` + app + `','Old')`,
		`INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev) VALUES ('` + message + `','` + app + `','k1',false,'s',2,'sender.example','example.sender.')`,
		`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ('` + delivery + `','` + message + `',0,'f1')`,
		`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ('` + second + `','` + message + `',1,'f2')`,
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
	var formats, deliveries int
	if err = db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM deliveries WHERE prepared_format=1),(SELECT count(*) FROM deliveries)`).Scan(&formats, &deliveries); err != nil || formats != 2 || deliveries != 2 {
		t.Fatal("existing deliveries are not format 1", formats, deliveries, err)
	}
	var validated bool
	if err = db.QueryRowContext(ctx, `SELECT convalidated FROM pg_constraint WHERE conname='deliveries_prepared_format' AND conrelid='deliveries'::regclass`).Scan(&validated); err != nil || validated {
		t.Fatal("the constraint must be NOT VALID", validated, err)
	}
	third, fourth := strings.Repeat("e", 32), strings.Repeat("f", 32)
	if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ('`+third+`','`+message+`',2,'f3')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,prepared_format) VALUES ('`+fourth+`','`+message+`',3,'f4',2)`); err != nil {
		t.Fatal("format 2 refused:", err)
	}
	var def int
	if err = db.QueryRowContext(ctx, `SELECT prepared_format FROM deliveries WHERE id=$1`, third).Scan(&def); err != nil || def != 1 {
		t.Fatal("default format", def, err)
	}
	for _, format := range []int{0, 3, -1} {
		if _, err = db.ExecContext(ctx, `UPDATE deliveries SET prepared_format=$1 WHERE id=$2`, format, third); err == nil {
			t.Errorf("format %d accepted", format)
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,prepared_format) VALUES ($1,$2,4,'f5',NULL)`, strings.Repeat("9", 32), message); err == nil {
		t.Error("NULL format accepted")
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil || version != len(migrations) {
		t.Fatal("schema version", version, err)
	}
}
