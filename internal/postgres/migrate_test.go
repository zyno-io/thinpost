package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/delivery"
)

func TestMigrationsSerializeAndDetectDrift(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL database")
	}
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "migration_test_" + hex.EncodeToString(random[:])
	if _, err = root.Exec("CREATE SCHEMA " + name); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.Exec("DROP SCHEMA " + name + " CASCADE") }()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", name)
	u.RawQuery = query.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if CheckSchema(ctx, db) == nil {
		t.Fatal("missing migration accepted")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- Migrate(ctx, db) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent migration: %v", err)
		}
	}
	if err = CheckSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	var original string
	if err = db.QueryRow(`SELECT checksum FROM thinpost_schema_migrations WHERE version=1`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE thinpost_schema_migrations SET checksum='changed' WHERE version=1`); err != nil {
		t.Fatal(err)
	}
	if CheckSchema(ctx, db) == nil || Migrate(ctx, db) == nil {
		t.Fatal("checksum drift accepted")
	}
	if _, err = db.Exec(`UPDATE thinpost_schema_migrations SET checksum=$1 WHERE version=1`, original); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,'future')`, len(migrations)+1); err != nil {
		t.Fatal(err)
	}
	if CheckSchema(ctx, db) == nil || Migrate(ctx, db) == nil {
		t.Fatal("newer schema accepted")
	}
}

func TestJournalIdentityMigrationPreservesLegacyAttempts(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL database")
	}
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "journal_migration_" + hex.EncodeToString(random[:])
	if _, err = root.Exec("CREATE SCHEMA " + name); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.Exec("DROP SCHEMA " + name + " CASCADE") }()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", name)
	u.RawQuery = query.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:9] {
		body, checksum, err := migrationBody(item)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(body)); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, item.version, checksum); err != nil {
			t.Fatal(err)
		}
	}
	_, err = db.Exec(`INSERT INTO apps(id,name) VALUES ('app','test');
		INSERT INTO messages(id,app_id,object_key,require_tls) VALUES ('msg','app','source',false);
		INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,current_attempt_id) VALUES ('delivery','msg',0,'final','in_flight','attempt');
		INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized,start_descriptor_hash,start_session_id) VALUES ('attempt','delivery','worker',1,true,decode(repeat('ab',32),'hex'),'old-session')`)
	if err != nil {
		t.Fatal(err)
	}
	legacyVersion := migrations[9]
	body, checksum, err := migrationBody(legacyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, legacyVersion.version, checksum); err != nil {
		t.Fatal(err)
	}
	if err = CheckSchema(context.Background(), db); err == nil {
		t.Fatal("mail runtime accepted the prior schema")
	}
	if _, err = db.Exec(`INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,state,outcome,smtp_code) VALUES ('historical-attempt','delivery','worker',1,'completed','rejected',450);
		INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ('historical-event','app','msg','delivery','historical-attempt','rejected',450)`); err != nil {
		t.Fatal(err)
	}
	legacyVersion = migrations[10]
	body, checksum, err = migrationBody(legacyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, legacyVersion.version, checksum); err != nil {
		t.Fatal(err)
	}
	if err = CheckSchema(context.Background(), db); err == nil {
		t.Fatal("mail runtime accepted the prior schema")
	}
	if _, err = db.Exec(`INSERT INTO bounce_receipts(id,delivery_id,object_key,fingerprint) VALUES ('old-bounce','delivery','bounces/old',decode(repeat('ab',32),'hex'));
		INSERT INTO webhook_events(id,app_id,message_id,delivery_id,bounce_receipt_id,outcome,smtp_code) VALUES ('old-bounce-event','app','msg','delivery','old-bounce','bounce_received_unverified',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO messages(id,app_id,object_key,require_tls,state,prepare_token,prepare_until) VALUES ('old-preparation','app','old/source',false,'preparing','old-token',now()+interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	legacyVersion = migrations[11]
	body, checksum, err = migrationBody(legacyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, legacyVersion.version, checksum); err != nil {
		t.Fatal(err)
	}
	if err = CheckSchema(context.Background(), db); err == nil {
		t.Fatal("mail runtime accepted the prior schema")
	}
	legacyVersion = migrations[12]
	body, checksum, err = migrationBody(legacyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, legacyVersion.version, checksum); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO messages(id,app_id,object_key,require_tls,state) VALUES
		('terminal-msg','app','source/terminal',false,'prepared'),
		('uncertain-msg','app','source/uncertain',false,'prepared'),
		('empty-msg','app','source/empty',false,'prepared');
		INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES
		('terminal-delivery','terminal-msg',0,'final/terminal','accepted'),
		('uncertain-delivery','uncertain-msg',0,'final/uncertain','uncertain')`); err != nil {
		t.Fatal(err)
	}
	if err = CheckSchema(context.Background(), db); err == nil {
		t.Fatal("mail runtime accepted the prior schema")
	}
	legacyVersion = migrations[13]
	body, checksum, err = migrationBody(legacyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, legacyVersion.version, checksum); err != nil {
		t.Fatal(err)
	}
	if err = CheckSchema(context.Background(), db); err == nil {
		t.Fatal("mail runtime accepted schema 14")
	}
	if _, err = db.Exec(`UPDATE runtime_gate SET mode='open' WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(context.Background(), db); err == nil {
		t.Fatal("migrated while admission was open")
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM thinpost_schema_migrations`).Scan(&count); err != nil || count != 14 {
		t.Fatal("rejected migration changed schema history", count, err)
	}
	if _, err = db.Exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	writer, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Exec(`SELECT mode FROM runtime_gate WHERE singleton FOR SHARE`); err != nil {
		t.Fatal(err)
	}
	blockedCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	err = Migrate(blockedCtx, db)
	deadlineReached := errors.Is(blockedCtx.Err(), context.DeadlineExceeded)
	cancel()
	if err == nil || !deadlineReached {
		t.Fatal("migration did not wait for an admitted transaction", err)
	}
	if err = writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var pendingLimit int
	if err = db.QueryRow(`SELECT pending_message_limit FROM apps WHERE id='app'`).Scan(&pendingLimit); err != nil || pendingLimit != 0 {
		t.Fatal("existing app did not default to a closed pending limit", pendingLimit, err)
	}
	for _, item := range []struct {
		id      string
		pending bool
	}{{"terminal-msg", false}, {"uncertain-msg", true}, {"empty-msg", true}, {"old-preparation", true}} {
		var pending bool
		if err = db.QueryRow(`SELECT pending FROM messages WHERE id=$1`, item.id).Scan(&pending); err != nil || pending != item.pending {
			t.Fatal("pending migration misclassified historical work", item.id, pending, err)
		}
	}
	var historicalAttempt string
	var bounceID sql.NullString
	if err = db.QueryRow(`SELECT attempt_id,bounce_receipt_id FROM webhook_events WHERE id='historical-event'`).Scan(&historicalAttempt, &bounceID); err != nil || historicalAttempt != "historical-attempt" || bounceID.Valid {
		t.Fatal("bounce migration changed historical webhook event", err)
	}
	if err = db.QueryRow(`SELECT bounce_receipt_id FROM webhook_events WHERE id='old-bounce-event'`).Scan(&bounceID); err != nil || !bounceID.Valid || bounceID.String != "old-bounce" {
		t.Fatal("preparation migration changed an existing bounce event", err)
	}
	var priorAttempts int
	if err = db.QueryRow(`SELECT prepare_attempts FROM messages WHERE id='old-preparation'`).Scan(&priorAttempts); err != nil || priorAttempts != 1 {
		t.Fatal("active legacy preparation did not retain its consumed claim", priorAttempts, err)
	}
	var authorized bool
	var prior sql.NullString
	if err = db.QueryRow(`SELECT start_authorized,start_journal_id FROM delivery_attempts WHERE id='attempt'`).Scan(&authorized, &prior); err != nil || !authorized || prior.Valid {
		t.Fatal("migration changed legacy authorization or invented a journal identity", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	legacy := delivery.Result{AttemptID: "attempt", DeliveryID: "delivery", WorkerID: "worker", JournalID: "0123456789abcdef0123456789abcdef", Epoch: 1, Outcome: "accepted", SMTPCode: 250}
	if err = commitDeliveryResultTx(context.Background(), tx, legacy, false, delivery.DefaultRetryPolicy()); !errors.Is(err, delivery.ErrResultConflict) {
		_ = tx.Rollback()
		t.Fatal("legacy attempt accepted an invented journal identity", err)
	}
	_ = tx.Rollback()
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = commitDeliveryResultTx(context.Background(), tx, legacy, true, delivery.DefaultRetryPolicy()); err == nil {
		_ = tx.Rollback()
		t.Fatal("reconciliation accepted a successful SMTP outcome without journal evidence")
	}
	legacy.Outcome, legacy.SMTPCode, legacy.JournalID = "uncertain", 0, ""
	if err = commitDeliveryResultTx(context.Background(), tx, legacy, true, delivery.DefaultRetryPolicy()); err != nil {
		_ = tx.Rollback()
		t.Fatal("legacy attempt could not be held uncertain", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = CheckSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}
