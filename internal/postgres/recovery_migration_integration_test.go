package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestRecoveryHoldHistoryMigration(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	schema := "recovery_migration_" + hex.EncodeToString(random[:])
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
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Recreate the exact schema-24 prefix in this disposable fixture.
	if _, err = db.ExecContext(ctx, `DROP TABLE inbound_daily, inbound_recipients, inbound_messages, inbound_domains CASCADE; ALTER TABLE tracking_objects DROP CONSTRAINT tracking_indefinite_no_send_window, DROP CONSTRAINT tracking_indefinite_no_retirement, ALTER COLUMN expires_at SET NOT NULL; DROP TABLE delivery_retry_policy_events; DROP TABLE delivery_retry_policy; DROP INDEX recovery_hold_history_idx; DROP TABLE message_recipients; DROP INDEX deliveries_id_prefix_idx, messages_app_id_prefix_idx; ALTER TABLE messages DROP CONSTRAINT messages_id_app_key, DROP COLUMN subject, DROP COLUMN recipient_count, DROP COLUMN sender_domain, DROP COLUMN sender_domain_rev; ALTER TABLE messages RENAME COLUMN enhanced_protection TO require_tls; DELETE FROM thinpost_schema_migrations WHERE version>=25;
		INSERT INTO apps(id,name) VALUES ('app','app');
		INSERT INTO messages(id,app_id,object_key,require_tls,state) VALUES ('message','app','source/fixture',true,'queued')`); err != nil {
		t.Fatal(err)
	}
	if err = CheckRecoveryBootstrapSchema(ctx, db); err != nil || CheckSchema(ctx, db) == nil {
		t.Fatal("schema 24 bootstrap compatibility", err)
	}
	const gen = "11111111111111111111111111111111"
	const run = "22222222222222222222222222222222"
	if err = RecoveryMigrateRestoredSchema(ctx, db, gen, run, "operator", 1); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("migration accepted a closed, uninitialized gate", err)
	}
	_, checksum24, err := migrationBody(migrations[23])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM thinpost_schema_migrations WHERE version>=17`); err != nil {
		t.Fatal(err)
	}
	if err = CheckRecoveryBootstrapSchema(ctx, db); err == nil {
		t.Fatal("accepted an older schema")
	}
	for _, item := range migrations[16:24] {
		_, checksum, bodyErr := migrationBody(item)
		if bodyErr != nil {
			t.Fatal(bodyErr)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, item.version, checksum); err != nil {
			t.Fatal(err)
		}
	}
	if err = RecoveryBegin(ctx, db, 1, gen, run, "operator"); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err == nil {
		t.Fatal("ordinary migration accepted recovery mode")
	}
	if err = RecoveryMigrateRestoredSchema(ctx, db, gen, run, "operator", 1); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("migration accepted a stale revision", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE thinpost_schema_migrations SET checksum='corrupt' WHERE version=24`); err != nil {
		t.Fatal(err)
	}
	if err = RecoveryMigrateRestoredSchema(ctx, db, gen, run, "operator", 2); err == nil {
		t.Fatal("migration accepted checksum drift")
	}
	if _, err = db.ExecContext(ctx, `UPDATE thinpost_schema_migrations SET checksum=$1 WHERE version=24`, checksum24); err != nil {
		t.Fatal(err)
	}
	const workers = 4
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- RecoveryMigrateRestoredSchema(ctx, db, gen, run, "operator", 2)
		}()
	}
	wg.Wait()
	close(results)
	for result := range results {
		if result != nil {
			t.Fatal("identical migration retry failed", result)
		}
	}
	if count, countErr := appliedCount(ctx, db); countErr != nil || count != 28 || CheckRecoveryTargetSchema(ctx, db) != nil {
		t.Fatal("recovery migration did not stop at its reviewed schema", count, countErr)
	}
	status, err := RecoveryInspect(ctx, db)
	if err != nil || status.Mode != "recovery" || status.Revision != 3 || status.QuarantineComplete {
		t.Fatal("migration changed recovery state unexpectedly", status, err)
	}
	var events int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM recovery_events WHERE action='migrate_schema_24_to_28' AND recovery_id=$1 AND actor='operator' AND revision=3`, run).Scan(&events); err != nil || events != 1 {
		t.Fatal("migration audit receipt missing or duplicated", events, err)
	}
	if err = RecoveryMigrateRestoredSchema(ctx, db, gen, run, "other-operator", 2); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("different actor reused migration receipt", err)
	}
	if err = RecoveryMigrateRestoredSchema(ctx, db, gen, run, "operator", 3); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("migration accepted a different revision", err)
	}
	if complete, qerr := RecoveryQuarantineBatch(ctx, db, gen, run, "operator"); qerr != nil || complete {
		t.Fatal("restored pending message was not quarantined", complete, qerr)
	}
	if complete, qerr := RecoveryQuarantineBatch(ctx, db, gen, run, "operator"); qerr != nil || !complete {
		t.Fatal("quarantine did not complete", complete, qerr)
	}
	history, err := RecoveryHoldHistoryPage(ctx, db, gen, run, 4, RecoveryCursor{Kind: "message", EntityID: "message"}, "", 100)
	if err != nil || len(history.Entries) != 1 || history.Entries[0].OriginalState != "queued" {
		t.Fatal("history unavailable after migration", history, err)
	}
}

func TestRecoveryMigrateRestoredSchemas(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	for version := 17; version <= 27; version++ {
		t.Run(fmt.Sprintf("schema_%d", version), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
			schema := "recovery_restore_" + hex.EncodeToString(random[:])
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
			for _, item := range migrations[:version] {
				body, checksum, bodyErr := migrationBody(item)
				if bodyErr != nil {
					t.Fatal(bodyErr)
				}
				if _, err = db.ExecContext(ctx, string(body)); err != nil {
					t.Fatal(err)
				}
				if _, err = db.ExecContext(ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, item.version, checksum); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('app','app');
				INSERT INTO messages(id,app_id,object_key,require_tls,state) VALUES ('queued-message','app','source/queued',true,'queued'),('message','app','source/fixture',true,'prepared');
				INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,current_attempt_id) VALUES ('delivery','message',0,'final/fixture','in_flight','attempt');
				INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized) VALUES ('attempt','delivery','worker',1,true);
				INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ref) VALUES ('subscription','app','https://example.test','key');
				INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ('event','app','message','delivery','attempt','uncertain',0);
				INSERT INTO webhook_deliveries(event_id,subscription_id,app_id,state) VALUES ('event','subscription','app','queued')`); err != nil {
				t.Fatal(err)
			}
			const gen = "33333333333333333333333333333333"
			const run = "44444444444444444444444444444444"
			if err = CheckRecoveryBootstrapSchema(ctx, db); err != nil {
				t.Fatal("restored schema rejected", err)
			}
			if err = RecoveryBegin(ctx, db, 1, gen, run, "operator"); err != nil {
				t.Fatal(err)
			}
			if version == 17 {
				// A historical collision must abort every migration in the
				// transaction, including DDL and the gate/audit changes.
				if _, err = db.ExecContext(ctx, `UPDATE webhook_events SET outcome='tracking_abandoned' WHERE id='event'`); err != nil {
					t.Fatal(err)
				}
				if err = RecoveryMigrateRestoredSchema(ctx, db, gen, run, "operator", 2); err == nil || err.Error() != "recovery migration 22 failed" {
					t.Fatal("historical outcome collision was accepted or exposed diagnostics", err)
				}
				var count int
				var trackingExists bool
				var revision int64
				if err = db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM thinpost_schema_migrations),to_regclass('tracking_objects') IS NOT NULL,(SELECT revision FROM runtime_gate WHERE singleton)`).Scan(&count, &trackingExists, &revision); err != nil || count != 17 || trackingExists || revision != 2 {
					t.Fatal("failed migration was not fully rolled back", count, trackingExists, revision, err)
				}
				if _, err = db.ExecContext(ctx, `UPDATE webhook_events SET outcome='uncertain' WHERE id='event'`); err != nil {
					t.Fatal(err)
				}
			}
			if err = RecoveryMigrateRestoredSchema(ctx, db, gen, run, "operator", 2); err != nil {
				t.Fatal("restored schema migration failed", err)
			}
			if count, countErr := appliedCount(ctx, db); countErr != nil || count != 28 || CheckRecoveryTargetSchema(ctx, db) != nil {
				t.Fatal("recovery migration did not stop at its reviewed schema", count, countErr)
			}
			var mode, action, outcome string
			var revision int64
			var rate, trackingLimit int
			if err = db.QueryRowContext(ctx, `SELECT g.mode,g.revision,e.action,w.outcome,a.submission_rate_per_minute,a.tracking_daily_limit FROM runtime_gate g,recovery_events e,webhook_events w,apps a WHERE g.singleton AND e.recovery_id=$1 AND e.action LIKE 'migrate_schema_%' AND w.id='event' AND a.id='app'`, run).Scan(&mode, &revision, &action, &outcome, &rate, &trackingLimit); err != nil || mode != "recovery" || revision != 3 || action != fmt.Sprintf("migrate_schema_%d_to_28", version) || outcome != "uncertain" || rate != 0 || trackingLimit != 0 {
				t.Fatal("migration changed restored evidence or opened admission", mode, revision, action, outcome, rate, trackingLimit, err)
			}
			if complete, qerr := RecoveryQuarantineBatch(ctx, db, gen, run, "operator"); qerr != nil || complete {
				t.Fatal("restored work was not quarantined", complete, qerr)
			}
			if complete, qerr := RecoveryQuarantineBatch(ctx, db, gen, run, "operator"); qerr != nil || !complete {
				t.Fatal("restored work quarantine did not complete", complete, qerr)
			}
			report, reportErr := RecoveryReportPage(ctx, db, gen, run, 4, RecoveryCursor{}, 100)
			if reportErr != nil || len(report.Holds) != 3 {
				t.Fatal("restored work missing from recovery report", report, reportErr)
			}
		})
	}
}
