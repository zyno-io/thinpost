package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMigration048 applies migrations through 47 over an existing app and
// sender domain, then migrates: the new table starts empty, enforces its
// rules, and leaves existing rows alone.
func TestMigration048(t *testing.T) {
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
	schema := "migration48_" + hex.EncodeToString(random[:])
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
	if migrations[47].version != 48 {
		t.Fatal("this test targets migration 48")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:47] {
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
	app, other := strings.Repeat("a", 32), strings.Repeat("b", 32)
	for _, statement := range []string{
		`INSERT INTO apps(id,name) VALUES ('` + app + `','A')`,
		`INSERT INTO apps(id,name) VALUES ('` + other + `','B')`,
		`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ('` + app + `','example.com',true,now())`,
		`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ('` + other + `','example.com',true,now())`,
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
	var count int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM tracking_domains`).Scan(&count); err != nil || count != 0 {
		t.Fatal("the table starts empty", count, err)
	}
	var verified bool
	if err = db.QueryRowContext(ctx, `SELECT active FROM sender_domains WHERE app_id=$1`, app).Scan(&verified); err != nil || !verified {
		t.Fatal("existing sender domains are untouched", verified, err)
	}

	n := 0
	insert := func(appID, sender, domain, state, extra, values string) error {
		n++
		id := fmt.Sprintf("%032x", n)
		_, err := db.ExecContext(ctx, `INSERT INTO tracking_domains(id,app_id,sender_domain,domain,state`+extra+`) VALUES ($1,$2,$3,$4,$5`+values+`)`, id, appID, sender, domain, state)
		return err
	}
	if err = insert(app, "example.com", "links.example.com", "pending", "", ""); err != nil {
		t.Fatal(err)
	}
	// A 253-byte name is the longest allowed.
	long := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 49) + ".big.example"
	if len(long) != 253 {
		t.Fatal("test name is", len(long))
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'big.example',true,now())`, app); err != nil {
		t.Fatal(err)
	}
	if err = insert(app, "big.example", long, "pending", "", ""); err != nil {
		t.Fatal("a name of 253 bytes is allowed:", err)
	}
	if err = insert(app, "big.example", "x"+long, "pending", "", ""); err == nil {
		t.Error("accepted a name over 253 bytes")
	}
	// The same name may be held by another app.
	if err = insert(other, "example.com", "links.example.com", "pending", "", ""); err != nil {
		t.Fatal("two apps may share a tracking domain", err)
	}
	for name, err := range map[string]error{
		"the sender domain itself":     insert(app, "example.com", "example.com", "pending", "", ""),
		"a lookalike suffix":           insert(app, "example.com", "links.not"+"example.com", "pending", "", ""),
		"uppercase":                    insert(app, "example.com", "L.example.com", "pending", "", ""),
		"a second live domain":         insert(app, "example.com", "other.example.com", "pending", "", ""),
		"the same name twice":          insert(app, "example.com", "links.example.com", "removed", ",removed_at", ",now()"),
		"an unknown sender domain":     insert(app, "nope.example", "l.nope.example", "pending", "", ""),
		"active without a pass":        insert(app, "example.com", "x.example.com", "active", "", ""),
		"an unknown state":             insert(app, "example.com", "y.example.com", "verifying", "", ""),
		"removed without a time":       insert(app, "example.com", "z.example.com", "removed", "", ""),
		"a removed time on a live row": insert(app, "example.com", "w.example.com", "pending", ",removed_at", ",now()"),
		"an unknown https result": func() error {
			_, err := db.ExecContext(ctx, `UPDATE tracking_domains SET https_result='host_changed',checked_at=now(),dns_result='ok'`)
			return err
		}(),
		"an unknown dns result": func() error {
			_, err := db.ExecContext(ctx, `UPDATE tracking_domains SET https_result='ok',checked_at=now(),dns_result='not_cname'`)
			return err
		}(),
		"a result without a check time": func() error {
			_, err := db.ExecContext(ctx, `UPDATE tracking_domains SET https_result='ok',dns_result='ok'`)
			return err
		}(),
	} {
		if err == nil {
			t.Error("accepted:", name)
		}
	}
	// not_found is a stored HTTPS result (checked in a transaction that is rolled back).
	if tx, err := db.BeginTx(ctx, nil); err != nil {
		t.Fatal(err)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE tracking_domains SET https_result='not_found',dns_result='missing',checked_at=now() WHERE domain='links.example.com' AND app_id=$1`, app)
		_ = tx.Rollback()
		if err != nil {
			t.Fatal("not_found rejected", err)
		}
	}
	// Several tombstones may share a sender domain, beside one live row.
	if err = insert(app, "example.com", "old1.example.com", "removed", ",removed_at", ",now()"); err != nil {
		t.Fatal("tombstones are not limited", err)
	}
	if err = insert(app, "example.com", "old2.example.com", "removed", ",removed_at", ",now()"); err != nil {
		t.Fatal("several tombstones per sender domain", err)
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil || version != len(migrations) {
		t.Fatal("schema version", version, err)
	}
}
