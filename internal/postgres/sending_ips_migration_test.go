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

// TestMigration045 applies migrations through 44 over existing sending IPs,
// app pins and an edge report, then migrates: every row keeps its id (so every
// pin survives), the name and bounce-domain columns are gone, the report keeps
// its addresses, and rows can be written without the dropped columns.
func TestMigration045(t *testing.T) {
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
	schema := "migration45_" + hex.EncodeToString(random[:])
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
	if migrations[44].version != 45 {
		t.Fatal("this test targets migration 45")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:44] {
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
	rowOne, rowTwo := strings.Repeat("1", 32), strings.Repeat("2", 32)
	for _, statement := range []string{
		`INSERT INTO sending_ips(id,name,address,helo_name,bounce_domain) VALUES
			('` + rowOne + `','main','45.0.1.22','smtp-main.example.org','example.org'),
			('` + rowTwo + `','hosting','45.0.0.10','smtp.hosting.example.org','hosting.example.org')`,
		`INSERT INTO apps(id,name,sending_ip_id) VALUES ('pinned','Pinned','` + rowTwo + `'),('plain','Plain',NULL)`,
		`INSERT INTO edge_source_reports(worker_id,epoch,addresses,bounce_domains,reported_at)
			VALUES ('w1',3,'{45.0.1.22,45.0.0.10}'::inet[],'{example.org}',clock_timestamp())`,
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
	var columns int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND
		((table_name='sending_ips' AND column_name IN ('name','bounce_domain')) OR (table_name='edge_source_reports' AND column_name='bounce_domains'))`, schema).Scan(&columns); err != nil || columns != 0 {
		t.Fatal("dropped columns still exist", columns, err)
	}
	var rows int
	var pinned string
	if err = db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM sending_ips WHERE (id,host(address),helo_name) IN (($1,'45.0.1.22','smtp-main.example.org'),($2,'45.0.0.10','smtp.hosting.example.org'))),
		(SELECT sending_ip_id FROM apps WHERE id='pinned')`, rowOne, rowTwo).Scan(&rows, &pinned); err != nil || rows != 2 || pinned != rowTwo {
		t.Fatal("rows or pins changed", rows, pinned, err)
	}
	var unpinned sql.NullString
	var addresses string
	if err = db.QueryRowContext(ctx, `SELECT (SELECT sending_ip_id FROM apps WHERE id='plain'),(SELECT addresses::text FROM edge_source_reports WHERE worker_id='w1')`).Scan(&unpinned, &addresses); err != nil || unpinned.Valid || addresses != "{45.0.1.22,45.0.0.10}" {
		t.Fatal("report or unpinned app changed", unpinned, addresses, err)
	}
	// The pin still protects the row, and rows and reports need no dropped column.
	if _, err = db.ExecContext(ctx, `DELETE FROM sending_ips WHERE id=$1`, rowTwo); err == nil {
		t.Fatal("a pinned row was deleted")
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO sending_ips(id,address,helo_name) VALUES ($1,'45.0.0.11','smtp.third.example.net')`, strings.Repeat("3", 32)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO edge_source_reports(worker_id,epoch,addresses,reported_at) VALUES ('w2',1,NULL,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil || version != len(migrations) {
		t.Fatal("schema version", version, err)
	}
}
