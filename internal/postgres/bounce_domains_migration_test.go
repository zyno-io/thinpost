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

// TestMigration047 applies migrations through 46 over an existing delivery
// attempt, then migrates: the attempt keeps a NULL return path, and the new
// table and column enforce their rules.
func TestMigration047(t *testing.T) {
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
	schema := "migration47_" + hex.EncodeToString(random[:])
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
	if migrations[46].version != 47 {
		t.Fatal("this test targets migration 47")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:46] {
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
	app, message, deliveryID, attempt := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32), strings.Repeat("d", 32)
	for _, statement := range []string{
		`INSERT INTO apps(id,name) VALUES ('` + app + `','A')`,
		`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ('` + app + `','example.com',true,now())`,
		`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('` + message + `','` + app + `','source','false','prepared')`,
		`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ('` + deliveryID + `','` + message + `',0,'signed')`,
		`INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized) VALUES ('` + attempt + `','` + deliveryID + `','worker',1,false)`,
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
	var returnPath sql.NullString
	if err = db.QueryRowContext(ctx, `SELECT return_path_domain FROM delivery_attempts WHERE id=$1`, attempt).Scan(&returnPath); err != nil || returnPath.Valid {
		t.Fatal("an existing attempt must keep a NULL return path", returnPath, err)
	}
	insertAttempt := func(id, domain string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized,return_path_domain) VALUES ($1,$2,'worker',2,false,$3)`, id, deliveryID, domain)
		return err
	}
	if err = insertAttempt(strings.Repeat("e", 32), "bounces.example.com"); err != nil {
		t.Fatal(err)
	}
	if err = insertAttempt(strings.Repeat("f", 32), "ab"); err == nil {
		t.Error("accepted a return path domain under the size limit")
	}
	if err = insertAttempt(strings.Repeat("1", 32), strings.Repeat("a", 220)); err == nil {
		t.Error("accepted a return path domain over the size limit")
	}

	insert := func(id, sender, domain, state, extra string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO bounce_domains(id,app_id,sender_domain,domain,state`+extra+`) VALUES ($1,$2,$3,$4,$5`+strings.Repeat(",now()", strings.Count(extra, ","))+`)`, id, app, sender, domain, state)
		return err
	}
	if err = insert(strings.Repeat("2", 32), "example.com", "bounces.example.com", "pending", ""); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"the sender domain itself":   insert(strings.Repeat("3", 32), "example.com", "example.com", "pending", ""),
		"a lookalike suffix":         insert(strings.Repeat("4", 32), "example.com", "b.not"+"example.com", "pending", ""),
		"uppercase":                  insert(strings.Repeat("5", 32), "example.com", "B.example.com", "pending", ""),
		"a second domain per sender": insert(strings.Repeat("6", 32), "example.com", "other.example.com", "pending", ""),
		"an unknown sender domain":   insert(strings.Repeat("7", 32), "nope.example", "b.nope.example", "pending", ""),
		"active without a target":    insert(strings.Repeat("8", 32), "example.com", "x.example.com", "active", ""),
		"an unknown state":           insert(strings.Repeat("9", 32), "example.com", "y.example.com", "verifying", ""),
		"a result without a check time": func() error {
			_, err := db.ExecContext(ctx, `UPDATE bounce_domains SET last_result='ok'`)
			return err
		}(),
	} {
		if err == nil {
			t.Error("accepted:", name)
		}
	}
	// The same name may be held by another app.
	other := strings.Repeat("0", 32)
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'B')`, other); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, other); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO bounce_domains(id,app_id,sender_domain,domain) VALUES ($1,$2,'example.com','bounces.example.com')`, strings.Repeat("1", 31)+"0", other); err != nil {
		t.Fatal("two apps may share a bounce domain", err)
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil || version != len(migrations) {
		t.Fatal("schema version", version, err)
	}
}
