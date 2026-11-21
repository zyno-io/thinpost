package deployhook

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

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/postgres"
)

// fixture is a disposable schema reverted to migration 27 with an open gate.
type fixture struct {
	t    *testing.T
	ctx  context.Context
	db   *sql.DB
	role string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	if os.Getenv("THINPOST_TEST_DISPOSABLE_DB") != "1" {
		t.Fatal("deploy hook tests create roles and require THINPOST_TEST_DISPOSABLE_DB=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(random[:])
	schema := "deploy_hook_" + suffix
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
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
	db.SetMaxOpenConns(4)
	f := &fixture{t: t, ctx: ctx, db: db, role: "Hook_test_" + suffix}
	if err = postgres.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Revert migrations 46, 45, 44, 43, 42, 41, 40, 39, 38, 37, 36, 35, 34, 33, 32, 31, 30, 29 and 28 to leave the exact schema-27 shape.
	// Migration 44: the messages constraints and columns are removed first (messages_smtp_not_console
	// references console_test, and the credential FK precedes the DROP TABLE);
	// edge_source_reports.submission_ports is dropped with that table below.
	// Migration 43: dropping the console test columns also drops its CHECK.
	// Migration 42: dropping the hold columns also drops their CHECKs and index;
	// its webhook_events CHECK is dropped with inbound_id below.
	// Migration 35's constraint is rebuilt without its feedback branch first.
	// Migration 41: message_recipients.kind is dropped with the table below.
	// Migration 39: dropping webhook_events.inbound_id also drops the source CHECK and the unique index.
	if _, err = db.ExecContext(ctx, `DROP TABLE dkim_keys;
		ALTER TABLE sender_domains ADD COLUMN verification_hash bytea CHECK (verification_hash IS NULL OR octet_length(verification_hash) = 32);
		ALTER TABLE messages DROP CONSTRAINT messages_smtp_not_console, DROP CONSTRAINT messages_smtp_credential_fk, DROP CONSTRAINT messages_original_from,
		DROP CONSTRAINT messages_subject_size, DROP COLUMN original_from_address, DROP COLUMN smtp_credential_id,
		ADD CONSTRAINT messages_subject_size CHECK (subject IS NULL OR octet_length(subject) BETWEEN 1 AND 998);
		ALTER TABLE apps DROP CONSTRAINT apps_smtp_from_policy, DROP COLUMN smtp_from_mode, DROP COLUMN smtp_from_domain,
		DROP COLUMN smtp_from_address, DROP COLUMN smtp_from_only_unauthorized;
		DROP TABLE smtp_replay_secret, smtp_credentials;
		ALTER TABLE messages DROP COLUMN console_test, DROP COLUMN limits_override, DROP COLUMN recipient_policy_override;
		ALTER TABLE messages DROP COLUMN hold_expires_at, DROP COLUMN hold_resolved_at;
		ALTER TABLE apps DROP COLUMN sending_ip_id;
		ALTER TABLE delivery_attempts DROP COLUMN source_address;
		DROP TABLE edge_source_reports, sending_ips;
ALTER TABLE webhook_deliveries DROP COLUMN lane;
		ALTER TABLE webhook_events DROP CONSTRAINT webhook_events_inbound_source_fk, DROP COLUMN inbound_id, DROP COLUMN inbound_recipient_index, ALTER COLUMN message_id SET NOT NULL;
		DROP TABLE inbound_daily, inbound_recipients, inbound_messages, inbound_domains;
		ALTER TABLE apps DROP COLUMN inbound_daily_limit, DROP COLUMN inbound_pending_limit;
		ALTER TABLE webhook_events ADD CONSTRAINT webhook_events_exactly_one_source CHECK (true);
		ALTER TABLE messages DROP COLUMN shared_message_id;
		ALTER TABLE apps DROP COLUMN default_track_opens, DROP COLUMN default_track_clicks;
		DROP TABLE feedback_mailbox, feedback_reporters, inbound_feedback_daily;
		ALTER TABLE webhook_events DROP CONSTRAINT webhook_events_exactly_one_source, DROP CONSTRAINT webhook_events_feedback_source_fk, DROP COLUMN feedback_id;
		ALTER TABLE webhook_events ADD CONSTRAINT webhook_events_exactly_one_source CHECK (
			(delivery_id IS NOT NULL AND attempt_id IS NOT NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND outcome NOT IN ('bounce_received_unverified','preparation_failed','tracking_abandoned')) OR
			(delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NOT NULL AND tracking_event_id IS NULL AND outcome = 'bounce_received_unverified' AND smtp_code = 0) OR
			(delivery_id IS NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND outcome = 'preparation_failed' AND smtp_code = 0) OR
			(delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NOT NULL AND outcome IN ('opened','clicked') AND smtp_code = 0) OR
			(delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND outcome = 'tracking_abandoned' AND smtp_code = 0));
		DROP TABLE recipient_feedback; ALTER TABLE apps DROP COLUMN list_unsubscribe;
		ALTER TABLE runtime_gate
		ADD COLUMN retry_max_attempts integer NOT NULL DEFAULT 128 CHECK (retry_max_attempts BETWEEN 1 AND 1000),
		ADD COLUMN retry_initial_ms bigint NOT NULL DEFAULT 1800000 CHECK (retry_initial_ms BETWEEN 60000 AND 86400000),
		ADD COLUMN retry_max_delay_ms bigint NOT NULL DEFAULT 10800000 CHECK (retry_max_delay_ms BETWEEN 60000 AND 86400000),
		ADD COLUMN retry_max_age_ms bigint NOT NULL DEFAULT 432000000 CHECK (retry_max_age_ms BETWEEN 3600000 AND 2592000000),
		ADD CHECK (retry_max_delay_ms >= retry_initial_ms AND retry_max_age_ms >= retry_initial_ms);
		ALTER TABLE delivery_attempts DROP COLUMN smtp_stage, DROP COLUMN smtp_enhanced;
		DROP TABLE delivery_retry_policy; ALTER TABLE audit_events ALTER COLUMN app_id SET NOT NULL;
		DROP INDEX webhook_deliveries_unfinished_idx, tracking_links_delivery_idx;
		DROP TABLE suppressions; ALTER TABLE webhook_events DROP COLUMN suppression_reason;
		ALTER TABLE messages DROP COLUMN sender_address, DROP COLUMN sender_name;
		ALTER TABLE webhook_events DROP COLUMN delivery_state;
		ALTER TABLE webhook_subscriptions DROP CONSTRAINT webhook_subscriptions_live_secret, DROP CONSTRAINT webhook_subscriptions_unsigned_format, DROP CONSTRAINT webhook_subscriptions_auth_summary,
		DROP CONSTRAINT webhook_subscriptions_deleted_no_secrets, DROP COLUMN format, DROP COLUMN auth_ciphertext, DROP COLUMN auth_username, DROP COLUMN auth_header_names;
		ALTER TABLE webhook_subscriptions RENAME COLUMN events TO outcomes;
		ALTER TABLE webhook_subscriptions DROP CONSTRAINT webhook_subscriptions_deleted_inactive,
		DROP COLUMN description, DROP COLUMN outcomes, DROP COLUMN secret_ciphertext, DROP COLUMN secret_created_at, DROP COLUMN created_at, DROP COLUMN deleted_at, ADD COLUMN secret_ref text NOT NULL;
		DROP TABLE message_recipients; DROP INDEX deliveries_id_prefix_idx, messages_app_id_prefix_idx;
		ALTER TABLE messages DROP CONSTRAINT messages_id_app_key, DROP COLUMN subject, DROP COLUMN recipient_count, DROP COLUMN sender_domain, DROP COLUMN sender_domain_rev;
		ALTER TABLE messages RENAME COLUMN enhanced_protection TO require_tls;
		DELETE FROM thinpost_schema_migrations WHERE version IN (28,29,30,31,32,33,34,35,36,37,38,39,40,41,42,43,44,45,46);
		UPDATE runtime_gate SET generation='`+testGeneration+`',mode='open',revision=5,quarantine_complete=true,changed_by='fixture' WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	role := pgx.Identifier{f.role}.Sanitize()
	var database string
	var publicTemp bool
	if err = root.QueryRowContext(ctx, `SELECT datname, EXISTS (SELECT 1 FROM aclexplode(COALESCE(datacl,acldefault('d',datdba))) a WHERE a.grantee=0 AND a.privilege_type='TEMPORARY') FROM pg_database WHERE datname=current_database()`).Scan(&database, &publicTemp); err != nil {
		t.Fatal(err)
	}
	quotedDatabase := pgx.Identifier{database}.Sanitize()
	if _, err = root.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'disposable-runtime-password'"); err != nil {
		t.Fatal(err)
	}
	if publicTemp {
		if _, err = root.ExecContext(ctx, "REVOKE TEMPORARY ON DATABASE "+quotedDatabase+" FROM PUBLIC"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_, _ = root.ExecContext(context.Background(), "DROP OWNED BY "+role)
		_, _ = root.ExecContext(context.Background(), "DROP ROLE "+role)
		if publicTemp {
			_, _ = root.ExecContext(context.Background(), "GRANT TEMPORARY ON DATABASE "+quotedDatabase+" TO PUBLIC")
		}
		db.Close()
		root.Close()
	})
	for _, statement := range []string{
		"GRANT USAGE ON SCHEMA " + schema + " TO " + role,
		"GRANT SELECT, UPDATE (lock_guard) ON TABLE " + schema + ".runtime_gate TO " + role,
	} {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) queueMessage() {
	f.t.Helper()
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO apps(id,name) VALUES ('app','app') ON CONFLICT DO NOTHING`); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO messages(id,app_id,object_key,require_tls,state) VALUES ('m1','app','source/m1',true,'queued')`); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) state() (int, postgres.GateStatus) {
	f.t.Helper()
	version, err := postgres.SchemaVersion(f.ctx, f.db)
	if err != nil {
		f.t.Fatal(err)
	}
	status, err := postgres.InspectGate(f.ctx, f.db)
	if err != nil {
		f.t.Fatal(err)
	}
	return version, status
}

// queueAfterClose simulates a submission that lands between the drain and the
// closure: the gate has been closed, so the next check sees a new message.
type queueAfterClose struct {
	Postgres
	f *fixture
}

func (q queueAfterClose) Close(ctx context.Context, generation, actor string, revision int64) (int64, error) {
	next, err := q.Postgres.Close(ctx, generation, actor, revision)
	if err == nil {
		q.f.queueMessage()
	}
	return next, err
}

func integrationPre(f *fixture) PreConfig {
	cfg := preConfig()
	cfg.RuntimeRole = f.role
	cfg.DrainTimeout = 200 * time.Millisecond
	return cfg
}

func TestIntegrationPreAndPostHooks(t *testing.T) {
	f := newFixture(t)
	db := Postgres{DB: f.db}
	cfg := integrationPre(f)
	cfg.Check = true
	plan, err := RunPre(f.ctx, db, cfg)
	if err != nil || plan.Action != "close_and_migrate" {
		t.Fatal(plan, err)
	}
	if version, status := f.state(); version != 27 || status.Mode != "open" || status.Revision != 5 {
		t.Fatal("--check changed state", version, status)
	}
	target, err := postgres.DeployTarget()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Check = false
	// A non-default policy set through the old maintenance command must survive the move to schema 33.
	if _, err = f.db.ExecContext(f.ctx, `UPDATE runtime_gate SET retry_max_attempts=17,retry_initial_ms=120000,retry_max_delay_ms=7200000,retry_max_age_ms=172800000 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	result, err := RunPre(f.ctx, db, cfg)
	if err != nil || result.From != 27 || result.To != target || result.Revision != 7 {
		t.Fatal(result, err)
	}
	if version, status := f.state(); version != target || status.Mode != "closed" || status.ChangedBy != testActor {
		t.Fatal(version, status)
	}
	if status, policyErr := postgres.InspectRetryPolicy(f.ctx, f.db); policyErr != nil || status.Policy.MaxAttempts != 17 || status.Policy.InitialDelay != 2*time.Minute ||
		status.Policy.MaxDelay != 2*time.Hour || status.Policy.MaxAge != 48*time.Hour || status.Revision != 1 || status.ChangedBy != "migration:033" {
		t.Fatal("retry policy was not carried over", status, policyErr)
	}
	// Schema 35 is applied by the same hook: its table, column and event branch exist,
	// and the one-click opt-in defaults to off for every existing app.
	var feedbackTable, optInDefaultOff bool
	if err = f.db.QueryRowContext(f.ctx, `SELECT to_regclass('recipient_feedback') IS NOT NULL,
		NOT EXISTS (SELECT 1 FROM apps WHERE list_unsubscribe)
		AND EXISTS (SELECT 1 FROM pg_constraint WHERE conname='webhook_events_exactly_one_source' AND pg_get_constraintdef(oid) LIKE '%feedback_id%')`).Scan(&feedbackTable, &optInDefaultOff); err != nil || !feedbackTable || !optInDefaultOff {
		t.Fatal("schema 35 was not applied", feedbackTable, optInDefaultOff, err)
	}
	// Schema 36 is applied by the same hook: the feedback-loop tables exist and
	// are empty (no mailbox, so nothing is received; no reporter, so nothing is
	// accepted).
	var loopTables bool
	if err = f.db.QueryRowContext(f.ctx, `SELECT to_regclass('feedback_mailbox') IS NOT NULL AND to_regclass('feedback_reporters') IS NOT NULL AND to_regclass('inbound_feedback_daily') IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM feedback_mailbox) AND NOT EXISTS (SELECT 1 FROM feedback_reporters)`).Scan(&loopTables); err != nil || !loopTables {
		t.Fatal("schema 36 was not applied", loopTables, err)
	}
	// Schema 37 is applied by the same hook: both tracking defaults exist and are
	// off for every app.
	var trackingDefaultsOff bool
	if err = f.db.QueryRowContext(f.ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='apps' AND column_name='default_track_opens')
		AND NOT EXISTS (SELECT 1 FROM apps WHERE default_track_opens OR default_track_clicks)`).Scan(&trackingDefaultsOff); err != nil || !trackingDefaultsOff {
		t.Fatal("schema 37 was not applied", trackingDefaultsOff, err)
	}
	// Schema 38 is applied by the same hook: existing messages use the
	// per-delivery Message-ID form.
	var perDeliveryForm bool
	if err = f.db.QueryRowContext(f.ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='messages' AND column_name='shared_message_id')
		AND NOT EXISTS (SELECT 1 FROM messages WHERE shared_message_id)`).Scan(&perDeliveryForm); err != nil || !perDeliveryForm {
		t.Fatal("schema 38 was not applied", perDeliveryForm, err)
	}
	// Schema 39 is applied by the same hook: the inbound tables are empty, every
	// app has the default limits, and every webhook delivery is on the event lane.
	var inboundSchema bool
	if err = f.db.QueryRowContext(f.ctx, `SELECT to_regclass('inbound_domains') IS NOT NULL AND to_regclass('inbound_messages') IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM inbound_domains) AND NOT EXISTS (SELECT 1 FROM inbound_messages)
		AND NOT EXISTS (SELECT 1 FROM apps WHERE inbound_daily_limit<>2000 OR inbound_pending_limit<>500)
		AND NOT EXISTS (SELECT 1 FROM webhook_deliveries WHERE lane<>'event')`).Scan(&inboundSchema); err != nil || !inboundSchema {
		t.Fatal("schema 39 was not applied", inboundSchema, err)
	}
	// Schema 40 is applied by the same hook: existing apps keep no sending IP
	// and the pool starts empty.
	var noSendingIPs bool
	if err = f.db.QueryRowContext(f.ctx, `SELECT to_regclass('sending_ips') IS NOT NULL AND to_regclass('edge_source_reports') IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM sending_ips) AND NOT EXISTS (SELECT 1 FROM apps WHERE sending_ip_id IS NOT NULL)
		AND NOT EXISTS (SELECT 1 FROM delivery_attempts WHERE source_address IS NOT NULL)`).Scan(&noSendingIPs); err != nil || !noSendingIPs {
		t.Fatal("schema 40 was not applied", noSendingIPs, err)
	}
	// A re-run of the pre step on the current schema adopts this closure for
	// the next attempt and never migrates again.
	retry := cfg
	retry.Actor = testPrefix + "r8"
	again, err := RunPre(f.ctx, db, retry)
	if err != nil || again.Action != "readopt_closure" {
		t.Fatal(again, err)
	}
	post := postConfig()
	post.Actor, post.RuntimeRole = retry.Actor, f.role
	post.RolloutTimeout = 100 * time.Millisecond
	// The rollout is incomplete: nothing reopens.
	stale := &fakeKube{pods: [][]Pod{append(readyPods(post.Image, 2), readyPods("img@sha256:old", 1)...)}}
	if _, err = RunPost(f.ctx, db, stale, post); reason(err) != "rollout_timeout" {
		t.Fatal(err)
	}
	if _, status := f.state(); status.Mode != "closed" {
		t.Fatal("gate opened before the rollout finished")
	}
	// The earlier attempt's actor no longer owns the gate.
	other := post
	other.Actor = testActor
	if res, postErr := RunPost(f.ctx, db, &fakeKube{err: context.Canceled}, other); postErr != nil || res.Action != "not_owned" {
		t.Fatal(res, postErr)
	}
	done, err := RunPost(f.ctx, db, &fakeKube{pods: [][]Pod{readyPods(post.Image, 2)}}, post)
	if err != nil || done.Action != "reopened" {
		t.Fatal(done, err)
	}
	if version, status := f.state(); version != target || status.Mode != "open" || status.Generation != testGeneration || status.Revision != 9 {
		t.Fatal(version, status)
	}
	// A second run of the whole sequence is a no-op that never closes the gate.
	noop := cfg
	noop.Actor = testPrefix + "r9"
	if res, noopErr := RunPre(f.ctx, db, noop); noopErr != nil || res.Action != "current" {
		t.Fatal(res, noopErr)
	}
	if _, status := f.state(); status.Mode != "open" || status.Revision != 9 {
		t.Fatal(status)
	}
}

func TestIntegrationQueuedMessageDuringClosureFailsBack(t *testing.T) {
	f := newFixture(t)
	_, err := RunPre(f.ctx, queueAfterClose{Postgres{DB: f.db}, f}, integrationPre(f))
	if reason(err) != "unprepared_messages" {
		t.Fatal(err)
	}
	version, status := f.state()
	if version != 27 || status.Mode != "open" || status.Revision != 7 || status.ChangedBy != testActor {
		t.Fatal("gate was not restored", version, status)
	}
	var actions string
	if err = f.db.QueryRowContext(f.ctx, `SELECT string_agg(action,',' ORDER BY occurred_at,id) FROM recovery_events`).Scan(&actions); err != nil || actions != "gate_close,gate_open" {
		t.Fatal(actions, err)
	}
}

func TestIntegrationQueuedMessagesTimeOutTheDrainWithoutClosing(t *testing.T) {
	f := newFixture(t)
	f.queueMessage()
	_, err := RunPre(f.ctx, Postgres{DB: f.db}, integrationPre(f))
	if reason(err) != "drain_timeout" {
		t.Fatal(err)
	}
	if version, status := f.state(); version != 27 || status.Mode != "open" || status.Revision != 5 {
		t.Fatal(version, status)
	}
}

func TestIntegrationMigrationFailureFailsBackWithoutDatabaseText(t *testing.T) {
	f := newFixture(t)
	// A conflicting object makes migration 28 fail inside the transaction.
	if _, err := f.db.ExecContext(f.ctx, `CREATE TABLE message_recipients(secret_marker text)`); err != nil {
		t.Fatal(err)
	}
	_, err := RunPre(f.ctx, Postgres{DB: f.db}, integrationPre(f))
	var failure *Failure
	if err == nil || reason(err) != "migrate_failed" {
		t.Fatal(err)
	}
	failure, _ = err.(*Failure)
	if !strings.Contains(failure.Detail, "migration_failed") || !strings.Contains(failure.Detail, "migration=28") || !strings.Contains(failure.Detail, "sqlstate=42P07") || strings.Contains(err.Error(), "message_recipients") {
		t.Fatal(err)
	}
	if version, status := f.state(); version != 27 || status.Mode != "open" {
		t.Fatal("failed migration was not rolled back and restored", version, status)
	}
}

// blockThenCancel holds a conflicting table lock from a second connection once
// the gate is closed, so DeployMigrate blocks on its own table lock, then
// cancels the hook's context as a SIGTERM would.
type blockThenCancel struct {
	Postgres
	f      *fixture
	cancel context.CancelFunc
	conn   *sql.Conn
	tx     *sql.Tx
}

func (b *blockThenCancel) Close(ctx context.Context, generation, actor string, revision int64) (int64, error) {
	next, err := b.Postgres.Close(ctx, generation, actor, revision)
	if err != nil {
		return next, err
	}
	if b.conn, err = b.f.db.Conn(b.f.ctx); err != nil {
		b.f.t.Fatal(err)
	}
	if b.tx, err = b.conn.BeginTx(b.f.ctx, nil); err != nil {
		b.f.t.Fatal(err)
	}
	// ACCESS SHARE conflicts with the ACCESS EXCLUSIVE lock DeployMigrate takes.
	if _, err = b.tx.ExecContext(b.f.ctx, `SELECT count(*) FROM runtime_gate`); err != nil {
		b.f.t.Fatal(err)
	}
	return next, nil
}

func (b *blockThenCancel) Migrate(ctx context.Context, role, actor string, revision int64, keep bool) (int, int, int64, error) {
	timer := time.AfterFunc(300*time.Millisecond, b.cancel)
	defer timer.Stop()
	return b.Postgres.Migrate(ctx, role, actor, revision, keep)
}

func TestIntegrationFailbackSurvivesCancelledContextWhileMigrateIsBlocked(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	blocker := &blockThenCancel{Postgres: Postgres{DB: f.db}, f: f, cancel: cancel}
	t.Cleanup(func() {
		if blocker.tx != nil {
			_ = blocker.tx.Rollback()
			_ = blocker.conn.Close()
		}
	})
	cfg := integrationPre(f)
	cfg.FailbackTimeout = 10 * time.Second
	started := time.Now()
	_, err := RunPre(ctx, blocker, cfg)
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatal("hook was not bounded", elapsed)
	}
	if reason(err) != "migrate_failed" || ctx.Err() == nil {
		t.Fatal(err, ctx.Err())
	}
	version, status := f.state()
	if version != 27 || status.Mode != "open" || status.Revision != 7 || status.ChangedBy != testActor {
		t.Fatal("gate was not reopened by fail-back", version, status)
	}
	var actions string
	if err = f.db.QueryRowContext(f.ctx, `SELECT string_agg(action,',' ORDER BY occurred_at,id) FROM recovery_events`).Scan(&actions); err != nil || actions != "gate_close,gate_open" {
		t.Fatal(actions, err)
	}
}
