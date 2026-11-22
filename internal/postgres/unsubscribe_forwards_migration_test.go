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

// TestMigration051 applies migrations through 50 over an existing delivery, then
// migrates: the delivery is untouched, unsubscribe_forwards is created empty, and
// its constraints keep each state consistent (a link only while it is needed, a
// reason only for not_forwarded, a lease only in flight).
func TestMigration051(t *testing.T) {
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
	schema := "migration51_" + hex.EncodeToString(random[:])
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
	if migrations[50].version != 51 {
		t.Fatal("this test targets migration 51")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:50] {
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
	var rows, deliveries int
	if err = db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM unsubscribe_forwards),(SELECT count(*) FROM deliveries)`).Scan(&rows, &deliveries); err != nil || rows != 0 || deliveries != 2 {
		t.Fatal("the table is not empty or deliveries changed", rows, deliveries, err)
	}
	valid := []string{
		`INSERT INTO unsubscribe_forwards(delivery_id,state,target_host) VALUES ('` + delivery + `','armed','unsub.example.com')`,
		`INSERT INTO unsubscribe_forwards(delivery_id,state,reason) VALUES ('` + second + `','not_forwarded','mailto_only')`,
	}
	for _, statement := range valid {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO unsubscribe_forwards(delivery_id,state,reason) VALUES ('`+delivery+`','not_forwarded','mailto_only')`); err == nil {
		t.Error("two rows for one delivery")
	}
	// Each statement below breaks one rule on a row that would otherwise be valid.
	for name, set := range map[string]string{
		"armed without a host":          `target_host=NULL`,
		"queued without a time":         `state='queued',next_attempt_at=NULL`,
		"in flight without a lease":     `state='in_flight'`,
		"a lease while armed":           `claim_token='` + strings.Repeat("e", 32) + `',claim_until=now()`,
		"a reason on a link":            `reason='mailto_only'`,
		"unknown state":                 `state='sent'`,
		"unknown failure":               `last_failure='boom'`,
		"attempts past the limit":       `attempts=25`,
		"delivered without finished_at": `state='delivered'`,
	} {
		if _, err = db.ExecContext(ctx, `UPDATE unsubscribe_forwards SET `+set+` WHERE delivery_id='`+delivery+`'`); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for _, step := range []struct{ name, set string }{
		{"queued", `state='queued',next_attempt_at=now()`},
		{"in flight", `state='in_flight',attempts=1,claim_token='` + strings.Repeat("e", 32) + `',claim_until=now()+interval '1 minute'`},
		{"delivered", `state='delivered',claim_token=NULL,claim_until=NULL,finished_at=now()`},
	} {
		if _, err = db.ExecContext(ctx, `UPDATE unsubscribe_forwards SET `+step.set+` WHERE delivery_id='`+delivery+`'`); err != nil {
			t.Errorf("%s was refused: %v", step.name, err)
		}
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil || version != len(migrations) {
		t.Fatal("schema version", version, err)
	}
}
