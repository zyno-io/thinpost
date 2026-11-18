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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const deployTestGeneration = "0123456789abcdef0123456789abcdef"

func TestDeployReviewIsCurrent(t *testing.T) {
	if deployReviewedThrough != len(migrations) {
		t.Fatalf("deploy safety review covers migrations through %d but %d exist: review lock, drain and compatibility behavior, then bump deployReviewedThrough and runtimeGrantSchemaVersion", deployReviewedThrough, len(migrations))
	}
	if target, err := DeployTarget(); err != nil || target != len(migrations) {
		t.Fatal(target, err)
	}
	if DeployMigrationFloor >= len(migrations) {
		t.Fatal("deploy floor is not below the target")
	}
}

func TestDeployErrorsCarryNoDatabaseText(t *testing.T) {
	err := deployDBError(errors.New(`duplicate key value violates unique constraint "x" DETAIL: Key (subject)=(secret)`), DeployCategoryMigration, 28)
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "duplicate") || !strings.Contains(err.Error(), "migration=28") {
		t.Fatal(err)
	}
}

type deployFixture struct {
	t    *testing.T
	ctx  context.Context
	db   *sql.DB
	role string
	repo *Repository
}

// newDeployFixture builds a disposable schema at migration 27 with an open
// gate in the test generation and a login role with only gate privileges.
func newDeployFixture(t *testing.T) *deployFixture {
	t.Helper()
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	if os.Getenv("THINPOST_TEST_DISPOSABLE_DB") != "1" {
		t.Fatal("deploy gate tests create roles and require THINPOST_TEST_DISPOSABLE_DB=1")
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
	schema := "deploy_gate_" + suffix
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
	db.SetMaxOpenConns(8)
	f := &deployFixture{t: t, ctx: ctx, db: db, role: "Deploy_test_" + suffix}
	f.repo = &Repository{DB: db, Generation: deployTestGeneration}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:DeployMigrationFloor] {
		body, checksum, bodyErr := migrationBody(item)
		if bodyErr != nil {
			t.Fatal(bodyErr)
		}
		if _, err = tx.ExecContext(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, item.version, checksum); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='open',revision=5,quarantine_complete=true,changed_by='fixture' WHERE singleton`, deployTestGeneration); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
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

func (f *deployFixture) gate() GateStatus {
	f.t.Helper()
	status, err := InspectGate(f.ctx, f.db)
	if err != nil {
		f.t.Fatal(err)
	}
	return status
}

func (f *deployFixture) version() int {
	f.t.Helper()
	count, err := SchemaVersion(f.ctx, f.db)
	if err != nil {
		f.t.Fatal(err)
	}
	return count
}

// events lists "action actor revision" in order.
func (f *deployFixture) events() []string {
	f.t.Helper()
	rows, err := f.db.QueryContext(f.ctx, `SELECT action||' '||actor||' '||revision FROM recovery_events ORDER BY occurred_at,id`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, line)
	}
	return out
}

func (f *deployFixture) setGate(statement string, args ...any) {
	f.t.Helper()
	if _, err := f.db.ExecContext(f.ctx, `UPDATE runtime_gate SET `+statement+` WHERE singleton`, args...); err != nil {
		f.t.Fatal(err)
	}
}

func requireCategory(t *testing.T, err error, category string) {
	t.Helper()
	if DeployErrorCategory(err) != category {
		t.Fatalf("expected category %q, got %v", category, err)
	}
}

func (f *deployFixture) closeGate(actor string) int64 {
	f.t.Helper()
	revision, err := CloseGate(f.ctx, f.db, deployTestGeneration, actor, f.gate().Revision)
	if err != nil {
		f.t.Fatal(err)
	}
	return revision
}

func TestDeployGateLifecycle(t *testing.T) {
	f := newDeployFixture(t)
	const actor = "deploy:ns/release:r1"
	tx, err := f.repo.beginRuntime(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	closed := f.closeGate(actor)
	if closed != 6 || f.gate().Mode != "closed" || f.gate().ChangedBy != actor {
		t.Fatal(f.gate())
	}
	if _, err = f.repo.beginRuntime(f.ctx); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("runtime admitted under a closed gate", err)
	}
	from, to, revision, err := DeployMigrate(f.ctx, f.db, f.role, actor, closed, false)
	if err != nil || from != 27 || to != len(migrations) || revision != 7 {
		t.Fatal(from, to, revision, err)
	}
	if f.version() != len(migrations) || f.gate().Revision != 7 || f.gate().Mode != "closed" || f.gate().ChangedBy != actor {
		t.Fatal(f.version(), f.gate())
	}
	var granted bool
	if err = f.db.QueryRowContext(f.ctx, `SELECT has_table_privilege($1,'message_recipients','SELECT,INSERT')`, f.role).Scan(&granted); err != nil || !granted {
		t.Fatal("runtime grants were not applied by the deploy", err)
	}
	if err = CheckRuntimeRole(f.ctx, f.db, f.role); err != nil {
		t.Fatal(err)
	}
	// A stale revision never reopens.
	if err = ReopenGate(f.ctx, f.db, deployTestGeneration, actor, f.role, closed, len(migrations)); DeployErrorCategory(err) != DeployCategoryConflict {
		t.Fatal("reopened at a stale revision", err)
	}
	// Wrong schema is refused.
	requireCategory(t, ReopenGate(f.ctx, f.db, deployTestGeneration, actor, f.role, 7, 27), DeployCategoryLedger)
	if err = ReopenGate(f.ctx, f.db, deployTestGeneration, actor, f.role, 7, len(migrations)); err != nil {
		t.Fatal(err)
	}
	if err = ReopenGate(f.ctx, f.db, deployTestGeneration, actor, f.role, 7, len(migrations)); err != nil {
		t.Fatal("reopen is not idempotent", err)
	}
	if status := f.gate(); status.Mode != "open" || status.Revision != 8 || status.Generation != deployTestGeneration {
		t.Fatal(status)
	}
	tx, err = f.repo.beginRuntime(f.ctx)
	if err != nil {
		t.Fatal("runtime not admitted after reopen", err)
	}
	tx.Rollback()
	want := []string{"gate_close " + actor + " 6", fmt.Sprintf("deploy_migrate_27_to_%d ", len(migrations)) + actor + " 7", "gate_open " + actor + " 8"}
	if got := f.events(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatal(got)
	}
	// All events of one closure share an opaque recovery ID.
	var distinct int
	if err = f.db.QueryRowContext(f.ctx, `SELECT count(DISTINCT recovery_id) FROM recovery_events`).Scan(&distinct); err != nil || distinct != 1 {
		t.Fatal(distinct, err)
	}
	// The runtime role cannot read the evidence or alter the gate.
	if err = CheckRuntimeRole(f.ctx, f.db, f.role); err != nil {
		t.Fatal(err)
	}
}

func TestCloseGateWaitsForAdmittedTransactionsAndHonorsLockTimeout(t *testing.T) {
	f := newDeployFixture(t)
	held, err := f.repo.beginRuntime(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback()
	previous := deployCloseLockTimeout
	defer func() { deployCloseLockTimeout = previous }()
	deployCloseLockTimeout = 300 * time.Millisecond
	started := time.Now()
	_, err = CloseGate(f.ctx, f.db, deployTestGeneration, "deploy:ns/release:r1", 5)
	requireCategory(t, err, DeployCategoryLock)
	if time.Since(started) < 250*time.Millisecond || f.gate().Mode != "open" || f.gate().Revision != 5 {
		t.Fatal("close did not wait for the admitted transaction or changed the gate", time.Since(started), f.gate())
	}
	deployCloseLockTimeout = 10 * time.Second
	done := make(chan error, 1)
	released := make(chan time.Time, 1)
	go func() {
		_, closeErr := CloseGate(f.ctx, f.db, deployTestGeneration, "deploy:ns/release:r1", 5)
		released <- time.Now()
		done <- closeErr
	}()
	time.Sleep(400 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("close completed while a runtime transaction was admitted")
	default:
	}
	commitAt := time.Now()
	if err = held.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if (<-released).Before(commitAt) || f.gate().Mode != "closed" {
		t.Fatal("close did not wait for the release")
	}
}

func TestDeployMigrateRollsBackWhenGrantFails(t *testing.T) {
	f := newDeployFixture(t)
	const actor = "deploy:ns/release:r1"
	closed := f.closeGate(actor)
	for _, role := range []string{"no_such_role_" + f.role[len(f.role)-6:], f.role} {
		if role == f.role {
			// An unsafe existing grant must fail the privilege check.
			if _, err := f.db.ExecContext(f.ctx, "GRANT UPDATE ON runtime_gate TO "+pgx.Identifier{f.role}.Sanitize()); err != nil {
				t.Fatal(err)
			}
		}
		_, _, _, err := DeployMigrate(f.ctx, f.db, role, actor, closed, false)
		requireCategory(t, err, DeployCategoryGrant)
		if f.version() != 27 || f.gate().Revision != closed || f.gate().Mode != "closed" {
			t.Fatal("failed grant left a partial migration", f.version(), f.gate())
		}
		var column bool
		if err = f.db.QueryRowContext(f.ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='messages' AND column_name='require_tls')`).Scan(&column); err != nil || !column {
			t.Fatal("migration DDL was not rolled back", err)
		}
	}
	if got := f.events(); len(got) != 1 || !strings.HasPrefix(got[0], "gate_close ") {
		t.Fatal(got)
	}
}

func TestDeployGateCompareAndSwap(t *testing.T) {
	f := newDeployFixture(t)
	const actor = "deploy:ns/release:r1"
	_, err := CloseGate(f.ctx, f.db, deployTestGeneration, actor, 4)
	requireCategory(t, err, DeployCategoryConflict)
	_, err = CloseGate(f.ctx, f.db, "ffffffffffffffffffffffffffffffff", actor, 5)
	requireCategory(t, err, DeployCategoryConflict)
	for _, bad := range []struct {
		generation, actor string
		revision          int64
	}{{"UPPER", actor, 5}, {deployTestGeneration, "", 5}, {deployTestGeneration, actor, 0}, {deployTestGeneration, " " + actor, 5}} {
		_, err = CloseGate(f.ctx, f.db, bad.generation, bad.actor, bad.revision)
		requireCategory(t, err, DeployCategoryInvalid)
	}
	if f.gate().Revision != 5 || f.gate().Mode != "open" {
		t.Fatal(f.gate())
	}
	closed := f.closeGate(actor)
	// Closing again, at either revision, conflicts: the gate is not open.
	for _, revision := range []int64{5, closed} {
		_, err = CloseGate(f.ctx, f.db, deployTestGeneration, actor, revision)
		requireCategory(t, err, DeployCategoryConflict)
	}
	_, _, _, err = DeployMigrate(f.ctx, f.db, f.role, actor, closed-1, false)
	requireCategory(t, err, DeployCategoryConflict)
	_, _, _, err = DeployMigrate(f.ctx, f.db, f.role, actor, closed+1, false)
	requireCategory(t, err, DeployCategoryConflict)
	// Reopen requires the owner for the deploy path, and exact revision.
	requireCategory(t, ReopenGate(f.ctx, f.db, deployTestGeneration, "deploy:ns/release:r2", f.role, closed, 27), DeployCategoryConflict)
	requireCategory(t, ReopenGate(f.ctx, f.db, deployTestGeneration, actor, f.role, closed-1, 27), DeployCategoryConflict)
	requireCategory(t, ReopenGate(f.ctx, f.db, "ffffffffffffffffffffffffffffffff", actor, f.role, closed, 27), DeployCategoryConflict)
	if f.gate().Mode != "closed" || f.gate().Revision != closed {
		t.Fatal(f.gate())
	}
	// The operator path identifies the closure by revision alone.
	if err = OperatorReopenGate(f.ctx, f.db, deployTestGeneration, "alice", f.role, closed, 27); err != nil {
		t.Fatal(err)
	}
	if status := f.gate(); status.Mode != "open" || status.ChangedBy != "alice" || status.Revision != closed+1 {
		t.Fatal(status)
	}
}

func TestConcurrentDeployMigrateOnlyOneWins(t *testing.T) {
	f := newDeployFixture(t)
	closed := f.closeGate("deploy:ns/release:r1")
	var wg sync.WaitGroup
	results := make([]error, 3)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, results[i] = DeployMigrate(f.ctx, f.db, f.role, "deploy:ns/release:r"+string(rune('1'+i)), closed, false)
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range results {
		if err == nil {
			wins++
		} else if DeployErrorCategory(err) != DeployCategoryConflict {
			t.Fatal(err)
		}
	}
	if wins != 1 || f.version() != len(migrations) || f.gate().Revision != closed+1 {
		t.Fatal(wins, f.version(), f.gate())
	}
	var migrateEvents int
	if err := f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM recovery_events WHERE action LIKE 'deploy_migrate_%'`).Scan(&migrateEvents); err != nil || migrateEvents != 1 {
		t.Fatal(migrateEvents, err)
	}
}

func TestDeployMigrateKeepsOperatorOwnership(t *testing.T) {
	f := newDeployFixture(t)
	f.setGate(`mode='closed',revision=6,changed_by='alice',recovery_id=NULL`)
	_, _, revision, err := DeployMigrate(f.ctx, f.db, f.role, "deploy:ns/release:r1", 6, true)
	if err != nil || revision != 7 {
		t.Fatal(revision, err)
	}
	if status := f.gate(); status.ChangedBy != "alice" || status.Mode != "closed" {
		t.Fatal("deploy took over an operator's closure", status)
	}
	if got := f.events(); len(got) != 1 || got[0] != fmt.Sprintf("deploy_migrate_27_to_%d deploy:ns/release:r1 7", len(migrations)) {
		t.Fatal(got)
	}
	// The deploy's own post step therefore leaves it closed.
	requireCategory(t, ReopenGate(f.ctx, f.db, deployTestGeneration, "deploy:ns/release:r1", f.role, 7, len(migrations)), DeployCategoryConflict)
}

func TestAdoptDeployClosureRules(t *testing.T) {
	f := newDeployFixture(t)
	const prefix = "deploy:ns/release:"
	closed := f.closeGate(prefix + "r1")
	_, err := AdoptDeployClosure(f.ctx, f.db, prefix, prefix+"r2", closed-1, time.Hour)
	requireCategory(t, err, DeployCategoryConflict)
	_, err = AdoptDeployClosure(f.ctx, f.db, "deploy:ns/other:", "deploy:ns/other:r2", closed, time.Hour)
	requireCategory(t, err, DeployCategoryConflict)
	_, err = AdoptDeployClosure(f.ctx, f.db, "alice:", "alice:r2", closed, time.Hour)
	requireCategory(t, err, DeployCategoryInvalid)
	_, err = AdoptDeployClosure(f.ctx, f.db, prefix, "deploy:ns/other:r2", closed, time.Hour)
	requireCategory(t, err, DeployCategoryInvalid)
	f.setGate(`changed_at=clock_timestamp()-interval '48 hours'`)
	_, err = AdoptDeployClosure(f.ctx, f.db, prefix, prefix+"r2", closed, 24*time.Hour)
	requireCategory(t, err, DeployCategoryConflict)
	f.setGate(`changed_at=clock_timestamp()-interval '23 hours'`)
	closedAt := f.gate().ChangedAt
	adopted, err := AdoptDeployClosure(f.ctx, f.db, prefix, prefix+"r2", closed, 24*time.Hour)
	if err != nil || adopted != closed+1 || f.gate().ChangedBy != prefix+"r2" {
		t.Fatal(adopted, err, f.gate())
	}
	// Adoption must not restart the clock: changed_at is still the closure time.
	if !f.gate().ChangedAt.Equal(closedAt) {
		t.Fatal("adoption changed changed_at", closedAt, f.gate().ChangedAt)
	}
	// Repeated adoption keeps measuring from the closure, so it expires.
	f.setGate(`changed_at=clock_timestamp()-interval '25 hours'`)
	_, err = AdoptDeployClosure(f.ctx, f.db, prefix, prefix+"r3", adopted, 24*time.Hour)
	requireCategory(t, err, DeployCategoryConflict)
	f.setGate(`changed_at=clock_timestamp()`)
	// A closure owned by an operator, or an open gate, is never adopted.
	f.setGate(`changed_by='alice'`)
	_, err = AdoptDeployClosure(f.ctx, f.db, prefix, prefix+"r3", adopted, time.Hour)
	requireCategory(t, err, DeployCategoryConflict)
	f.setGate(`changed_by=$1,mode='open'`, prefix+"r2")
	_, err = AdoptDeployClosure(f.ctx, f.db, prefix, prefix+"r3", adopted, time.Hour)
	requireCategory(t, err, DeployCategoryConflict)
	got := f.events()
	if len(got) != 2 || got[1] != "gate_adopt "+prefix+"r2 "+"7" {
		t.Fatal(got)
	}
}

func TestDeployMigrateRefusals(t *testing.T) {
	f := newDeployFixture(t)
	const actor = "deploy:ns/release:r1"
	closed := f.closeGate(actor)

	// Recovery mode: the gate is not closed, so a deploy never proceeds.
	f.setGate(`mode='recovery'`)
	_, _, _, err := DeployMigrate(f.ctx, f.db, f.role, actor, closed, false)
	requireCategory(t, err, DeployCategoryConflict)
	f.setGate(`mode='closed'`)

	// A manual-only migration is refused.
	deployManual[28] = "needs an attended window"
	_, _, _, err = DeployMigrate(f.ctx, f.db, f.role, actor, closed, false)
	delete(deployManual, 28)
	requireCategory(t, err, DeployCategoryPolicy)

	// Below the floor.
	if _, err = f.db.ExecContext(f.ctx, `DELETE FROM thinpost_schema_migrations WHERE version=27`); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = DeployMigrate(f.ctx, f.db, f.role, actor, closed, false)
	requireCategory(t, err, DeployCategoryPolicy)
	_, body27, bodyErr := migrationBody(migrations[26])
	if bodyErr != nil {
		t.Fatal(bodyErr)
	}
	if _, err = f.db.ExecContext(f.ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES (27,$1)`, body27); err != nil {
		t.Fatal(err)
	}

	// A database ahead of the image, and a modified migration.
	if _, err = f.db.ExecContext(f.ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES (28,'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err = SchemaVersion(f.ctx, f.db); !errors.Is(err, ErrMigrationChecksum) {
		t.Fatal(err)
	}
	if _, err = f.db.ExecContext(f.ctx, `UPDATE thinpost_schema_migrations SET version=30 WHERE version=28`); err != nil {
		t.Fatal(err)
	}
	if _, err = SchemaVersion(f.ctx, f.db); !errors.Is(err, ErrUnknownMigration) {
		t.Fatal(err)
	}
	_, _, _, err = DeployMigrate(f.ctx, f.db, f.role, actor, closed, false)
	requireCategory(t, err, DeployCategoryLedger)
	if _, err = f.db.ExecContext(f.ctx, `DELETE FROM thinpost_schema_migrations WHERE version=30`); err != nil {
		t.Fatal(err)
	}
	if f.version() != 27 || f.gate().Revision != closed {
		t.Fatal("a refusal changed state", f.version(), f.gate())
	}
	// Invalid requests.
	for _, role := range []string{"", "PUBLIC", strings.Repeat("r", 64)} {
		_, _, _, err = DeployMigrate(f.ctx, f.db, role, actor, closed, false)
		requireCategory(t, err, DeployCategoryInvalid)
	}
}

func TestLedgerExistsAndRefusalOfUninitializedDatabase(t *testing.T) {
	f := newDeployFixture(t)
	if exists, err := LedgerExists(f.ctx, f.db); err != nil || !exists {
		t.Fatal(exists, err)
	}
	if _, err := f.db.ExecContext(f.ctx, `ALTER TABLE thinpost_schema_migrations RENAME TO ledger_hidden`); err != nil {
		t.Fatal(err)
	}
	if exists, err := LedgerExists(f.ctx, f.db); err != nil || exists {
		t.Fatal("ledger reported present", exists, err)
	}
}

func TestReopenRefusesNullGenerationAndMissingPrivileges(t *testing.T) {
	f := newDeployFixture(t)
	const actor = "deploy:ns/release:r1"
	closed := f.closeGate(actor)
	f.setGate(`generation=NULL`)
	requireCategory(t, ReopenGate(f.ctx, f.db, deployTestGeneration, actor, f.role, closed, 27), DeployCategoryConflict)
	requireCategory(t, OperatorReopenGate(f.ctx, f.db, deployTestGeneration, actor, f.role, closed, 27), DeployCategoryConflict)
	f.setGate(`generation=$1`, deployTestGeneration)
	// A role without gate privileges is refused.
	if _, err := f.db.ExecContext(f.ctx, "REVOKE UPDATE (lock_guard) ON runtime_gate FROM "+pgx.Identifier{f.role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	requireCategory(t, ReopenGate(f.ctx, f.db, deployTestGeneration, actor, f.role, closed, 27), DeployCategoryGrant)
	if f.gate().Mode != "closed" {
		t.Fatal("reopened despite invalid privileges")
	}
}

func TestUnpreparedMessages(t *testing.T) {
	f := newDeployFixture(t)
	if n, err := UnpreparedMessages(f.ctx, f.db); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO apps(id,name) VALUES ('app','app')`); err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{"queued", "preparing", "prepared"} {
		if _, err := f.db.ExecContext(f.ctx, `INSERT INTO messages(id,app_id,object_key,require_tls,state) VALUES ($1,'app','source/'||$1,true,$2)`, "message"+string(rune('a'+i)), state); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := UnpreparedMessages(f.ctx, f.db); err != nil || n != 2 {
		t.Fatal(n, err)
	}
}

func TestDeployMigratePreservesClosureTimeAndNullOwner(t *testing.T) {
	f := newDeployFixture(t)
	f.setGate(`mode='closed',revision=6,changed_by=NULL,recovery_id=NULL,changed_at=clock_timestamp()-interval '5 hours'`)
	closedAt := f.gate().ChangedAt
	if _, _, _, err := DeployMigrate(f.ctx, f.db, f.role, "deploy:ns/release:r1", 6, true); err != nil {
		t.Fatal(err)
	}
	var owner sql.NullString
	if err := f.db.QueryRowContext(f.ctx, `SELECT changed_by FROM runtime_gate WHERE singleton`).Scan(&owner); err != nil || owner.Valid {
		t.Fatal("NULL owner was not preserved", owner, err)
	}
	if !f.gate().ChangedAt.Equal(closedAt) {
		t.Fatal("migrate changed the closure time")
	}
}
