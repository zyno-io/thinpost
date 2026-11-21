package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// DeployMigrationFloor is the oldest schema a deploy may migrate. Older
// databases need the manual, attested recovery chain.
const DeployMigrationFloor = 27

// deployReviewedThrough forces a deploy-safety review whenever a migration is
// added: the lock, drain and compatibility behavior of every migration up to
// and including this one has been reviewed for unattended application.
// A test asserts it equals len(migrations).
const deployReviewedThrough = 45

// deployManual lists migrations that must never be applied by a deploy, keyed
// by version, with the reason. A deploy that would cross one is refused.
var deployManual = map[int]string{}

// DeployActorPrefix starts the gate owner recorded by a deploy.
const DeployActorPrefix = "deploy:"

// Lock waits are bounded so a deploy fails visibly instead of queueing behind
// runtime transactions. Tests shorten them.
var (
	deployCloseLockTimeout   = 30 * time.Second
	deployMigrateLockTimeout = 60 * time.Second
)

// Deploy failure categories. They carry no database message text.
const (
	DeployCategoryInvalid   = "invalid_request"
	DeployCategoryConflict  = "gate_conflict"
	DeployCategoryLock      = "lock_timeout"
	DeployCategoryLedger    = "ledger"
	DeployCategoryPolicy    = "policy_refused"
	DeployCategoryMigration = "migration_failed"
	DeployCategoryGrant     = "grant_failed"
	DeployCategoryDatabase  = "database_error"
	// DeployCategoryAmbiguous means the commit outcome is unknown; the caller
	// must leave the gate as it is and re-inspect.
	DeployCategoryAmbiguous = "commit_ambiguous"
)

// DeployError identifies a failed deploy operation by category, migration
// number and SQLSTATE only. Database message text can quote data and is never
// retained.
type DeployError struct {
	Category  string
	Migration int
	SQLState  string
}

func (e *DeployError) Error() string {
	text := "deploy gate: " + e.Category
	if e.Migration != 0 {
		text += fmt.Sprintf(" migration=%d", e.Migration)
	}
	if e.SQLState != "" {
		text += " sqlstate=" + e.SQLState
	}
	return text
}

// DeployErrorCategory returns the category of err, or "" if it is not one.
func DeployErrorCategory(err error) string {
	var deploy *DeployError
	if errors.As(err, &deploy) {
		return deploy.Category
	}
	return ""
}

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && len(pg.Code) == 5 {
		return pg.Code
	}
	return ""
}

// deployDBError classifies a database error without keeping its text.
func deployDBError(err error, category string, migration int) error {
	state := sqlState(err)
	if state == "55P03" {
		category = DeployCategoryLock
	}
	var applied *migrationApplyError
	if errors.As(err, &applied) {
		migration = applied.version
	}
	return &DeployError{Category: category, Migration: migration, SQLState: state}
}

// ErrDeployGateConflict reports that the gate is not in the state the caller
// expected (mode, owner, generation, schema or revision).
var ErrDeployGateConflict = &DeployError{Category: DeployCategoryConflict}

// GateStatus is the gate as read by a maintenance connection.
type GateStatus struct {
	Mode, Generation, ChangedBy string
	Revision                    int64
	ChangedAt                   time.Time
}

// InspectGate reads the gate without locking it.
func InspectGate(ctx context.Context, db *sql.DB) (GateStatus, error) {
	var status GateStatus
	var generation, changedBy sql.NullString
	err := db.QueryRowContext(ctx, `SELECT mode,generation,changed_by,revision,changed_at FROM runtime_gate WHERE singleton=true`).Scan(&status.Mode, &generation, &changedBy, &status.Revision, &status.ChangedAt)
	if err != nil {
		return GateStatus{}, deployDBError(err, DeployCategoryDatabase, 0)
	}
	status.Generation, status.ChangedBy = generation.String, changedBy.String
	return status, nil
}

// LedgerExists reports whether the migration ledger is present, so a deploy
// can refuse an uninitialized database instead of creating one.
func LedgerExists(ctx context.Context, db *sql.DB) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('thinpost_schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return false, deployDBError(err, DeployCategoryDatabase, 0)
	}
	return exists, nil
}

// DeployTarget is the schema version this binary expects. It fails if the
// deploy safety review is stale for the embedded migrations.
func DeployTarget() (int, error) {
	if len(migrations) != deployReviewedThrough {
		return 0, &DeployError{Category: DeployCategoryPolicy}
	}
	return len(migrations), nil
}

// DeployManualBetween reports the first manual-only migration in (from, to].
func DeployManualBetween(from, to int) (int, bool) {
	for version := from + 1; version <= to; version++ {
		if _, manual := deployManual[version]; manual {
			return version, true
		}
	}
	return 0, false
}

// UnpreparedMessages counts messages whose source object a schema-27 image
// still has to read: they are queued or being prepared.
func UnpreparedMessages(ctx context.Context, db *sql.DB) (int64, error) {
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE state IN ('queued','preparing')`).Scan(&count); err != nil {
		return 0, deployDBError(err, DeployCategoryDatabase, 0)
	}
	return count, nil
}

// CheckRuntimeRole verifies a runtime login role's privileges from a
// maintenance connection.
func CheckRuntimeRole(ctx context.Context, db *sql.DB, role string) error {
	if role == "" || len(role) > 63 || role == "PUBLIC" {
		return &DeployError{Category: DeployCategoryInvalid}
	}
	if err := checkRuntimeRolePrivileges(ctx, db, role, true); err != nil {
		return &DeployError{Category: DeployCategoryGrant, SQLState: sqlState(err)}
	}
	return nil
}

type gateRow struct {
	mode                              string
	generation, recoveryID, changedBy sql.NullString
	revision                          int64
	ageSeconds                        float64
}

// lockGate takes the gate row lock. It waits for admitted runtime
// transactions, which hold the row FOR SHARE, until the lock timeout.
func lockGate(ctx context.Context, tx *sql.Tx, timeout time.Duration) (gateRow, error) {
	var row gateRow
	if _, err := tx.ExecContext(ctx, `SELECT set_config('lock_timeout',$1,true)`, fmt.Sprintf("%dms", timeout.Milliseconds())); err != nil {
		return row, deployDBError(err, DeployCategoryDatabase, 0)
	}
	err := tx.QueryRowContext(ctx, `SELECT mode,generation,recovery_id,changed_by,revision,EXTRACT(EPOCH FROM clock_timestamp()-changed_at)::float8 FROM runtime_gate WHERE singleton=true FOR UPDATE`).Scan(&row.mode, &row.generation, &row.recoveryID, &row.changedBy, &row.revision, &row.ageSeconds)
	if err != nil {
		return row, deployDBError(err, DeployCategoryDatabase, 0)
	}
	return row, nil
}

func validDeployRequest(actor string, revision int64) bool {
	return adminText(actor, 256) && revision > 0 && revision < math.MaxInt64
}

func recordGateEvent(ctx context.Context, tx *sql.Tx, recoveryID, generation, action, actor string, revision int64) error {
	if err := recordRecoveryEvent(ctx, tx, recoveryID, generation, action, actor, revision); err != nil {
		return deployDBError(err, DeployCategoryDatabase, 0)
	}
	return nil
}

func commitDeploy(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return &DeployError{Category: DeployCategoryAmbiguous, SQLState: sqlState(err)}
	}
	return nil
}

// CloseGate closes an open gate for a deploy by compare-and-swap on the
// revision. The configured generation must match the stored one. The row lock
// waits for admitted runtime transactions up to the lock timeout. Each closure
// gets a fresh opaque ID, recorded with the closing event.
func CloseGate(ctx context.Context, db *sql.DB, generation, actor string, expectedRevision int64) (int64, error) {
	if !recoveryID(generation) || !validDeployRequest(actor, expectedRevision) {
		return 0, &DeployError{Category: DeployCategoryInvalid}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, deployDBError(err, DeployCategoryDatabase, 0)
	}
	defer tx.Rollback()
	row, err := lockGate(ctx, tx, deployCloseLockTimeout)
	if err != nil {
		return 0, err
	}
	if row.mode != "open" || row.revision != expectedRevision || !row.generation.Valid || row.generation.String != generation {
		return 0, ErrDeployGateConflict
	}
	id, err := newOpaqueID()
	if err != nil {
		return 0, &DeployError{Category: DeployCategoryDatabase}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_gate SET mode='closed',recovery_id=$1,revision=revision+1,changed_at=clock_timestamp(),changed_by=$2 WHERE singleton`, id, actor); err != nil {
		return 0, deployDBError(err, DeployCategoryDatabase, 0)
	}
	if err = recordGateEvent(ctx, tx, id, generation, "gate_close", actor, expectedRevision+1); err != nil {
		return 0, err
	}
	if err = commitDeploy(tx); err != nil {
		return 0, err
	}
	return expectedRevision + 1, nil
}

// AdoptDeployClosure re-stamps the owner of a closed gate that an earlier
// deploy attempt (an actor with the given prefix) closed no more than maxAge
// ago, so the current attempt's post step reopens it. changed_at is left
// alone: it stays the time the gate was closed, so repeated adoption (every
// chart reconcile re-runs the hooks) can never extend the stale limit.
func AdoptDeployClosure(ctx context.Context, db *sql.DB, actorPrefix, actor string, expectedRevision int64, maxAge time.Duration) (int64, error) {
	if !strings.HasPrefix(actorPrefix, DeployActorPrefix) || !strings.HasPrefix(actor, actorPrefix) || !validDeployRequest(actor, expectedRevision) || maxAge <= 0 {
		return 0, &DeployError{Category: DeployCategoryInvalid}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, deployDBError(err, DeployCategoryDatabase, 0)
	}
	defer tx.Rollback()
	row, err := lockGate(ctx, tx, deployCloseLockTimeout)
	if err != nil {
		return 0, err
	}
	if row.mode != "closed" || row.revision != expectedRevision || !row.generation.Valid || row.generation.String == "" ||
		!row.changedBy.Valid || !strings.HasPrefix(row.changedBy.String, actorPrefix) || row.ageSeconds > maxAge.Seconds() {
		return 0, ErrDeployGateConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_gate SET revision=revision+1,changed_by=$1 WHERE singleton`, actor); err != nil {
		return 0, deployDBError(err, DeployCategoryDatabase, 0)
	}
	if err = recordGateEvent(ctx, tx, row.recoveryID.String, row.generation.String, "gate_adopt", actor, expectedRevision+1); err != nil {
		return 0, err
	}
	if err = commitDeploy(tx); err != nil {
		return 0, err
	}
	return expectedRevision + 1, nil
}

// ReopenGate opens a gate this deploy closed (stored owner equals actor) by
// compare-and-swap on the revision. It requires the stored generation to equal
// the configured one, the schema to be exactly expectedSchema, and the runtime
// role's privileges to be valid. It is idempotent once the gate has been
// reopened by the same actor at expectedRevision+1.
func ReopenGate(ctx context.Context, db *sql.DB, generation, actor, runtimeRole string, expectedRevision int64, expectedSchema int) error {
	return reopenGate(ctx, db, generation, actor, runtimeRole, expectedRevision, expectedSchema, true)
}

// OperatorReopenGate is the manual path: the revision compare-and-swap, not an
// owner match, identifies the closure the operator inspected.
func OperatorReopenGate(ctx context.Context, db *sql.DB, generation, actor, runtimeRole string, expectedRevision int64, expectedSchema int) error {
	return reopenGate(ctx, db, generation, actor, runtimeRole, expectedRevision, expectedSchema, false)
}

func reopenGate(ctx context.Context, db *sql.DB, generation, actor, runtimeRole string, expectedRevision int64, expectedSchema int, sameOwner bool) error {
	if !recoveryID(generation) || !validDeployRequest(actor, expectedRevision) || runtimeRole == "" || len(runtimeRole) > 63 || runtimeRole == "PUBLIC" || expectedSchema < 1 || expectedSchema > len(migrations) {
		return &DeployError{Category: DeployCategoryInvalid}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return deployDBError(err, DeployCategoryDatabase, 0)
	}
	defer tx.Rollback()
	row, err := lockGate(ctx, tx, deployCloseLockTimeout)
	if err != nil {
		return err
	}
	if row.mode == "open" && row.generation.String == generation && row.changedBy.String == actor && row.revision == expectedRevision+1 {
		return commitDeploy(tx)
	}
	if row.mode != "closed" || row.revision != expectedRevision || !row.generation.Valid || row.generation.String == "" || row.generation.String != generation ||
		sameOwner && row.changedBy.String != actor {
		return ErrDeployGateConflict
	}
	count, err := appliedCount(ctx, tx)
	if err != nil || count != expectedSchema {
		return &DeployError{Category: DeployCategoryLedger}
	}
	if err = checkRuntimeRolePrivileges(ctx, tx, runtimeRole, true); err != nil {
		return &DeployError{Category: DeployCategoryGrant, SQLState: sqlState(err)}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_gate SET mode='open',revision=revision+1,changed_at=clock_timestamp(),changed_by=$1 WHERE singleton`, actor); err != nil {
		return deployDBError(err, DeployCategoryDatabase, 0)
	}
	if err = recordGateEvent(ctx, tx, row.recoveryID.String, generation, "gate_open", actor, expectedRevision+1); err != nil {
		return err
	}
	return commitDeploy(tx)
}

// DeployMigrate applies the missing migrations and the runtime grants in one
// transaction under the closed gate, then advances the gate revision and
// records the step. The gate must be closed at expectedRevision. When
// keepGateOwner is set the gate's recorded owner is preserved, so a closure
// made by an operator is never taken over by (and reopened by) the deploy.
// On any error nothing is committed, except an ambiguous commit, which is
// reported as DeployCategoryAmbiguous.
func DeployMigrate(ctx context.Context, db *sql.DB, runtimeRole, actor string, expectedRevision int64, keepGateOwner bool) (from, to int, revision int64, err error) {
	if runtimeRole == "" || len(runtimeRole) > 63 || runtimeRole == "PUBLIC" || !validDeployRequest(actor, expectedRevision) {
		return 0, 0, 0, &DeployError{Category: DeployCategoryInvalid}
	}
	target, err := DeployTarget()
	if err != nil {
		return 0, 0, 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, deployDBError(err, DeployCategoryDatabase, 0)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('lock_timeout',$1,true)`, fmt.Sprintf("%dms", deployMigrateLockTimeout.Milliseconds())); err != nil {
		return 0, 0, 0, deployDBError(err, DeployCategoryDatabase, 0)
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return 0, 0, 0, deployDBError(err, DeployCategoryLock, 0)
	}
	// Table lock before row lock, as Migrate does: a later ALTER TABLE of the
	// gate must not deadlock with admitted transactions waiting on the row.
	if _, err = tx.ExecContext(ctx, `LOCK TABLE runtime_gate IN ACCESS EXCLUSIVE MODE`); err != nil {
		return 0, 0, 0, deployDBError(err, DeployCategoryLock, 0)
	}
	var mode string
	var gateRevision int64
	var recoveryRef, generation sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT mode,generation,recovery_id,revision FROM runtime_gate WHERE singleton=true FOR UPDATE`).Scan(&mode, &generation, &recoveryRef, &gateRevision); err != nil {
		return 0, 0, 0, deployDBError(err, DeployCategoryDatabase, 0)
	}
	if mode != "closed" || gateRevision != expectedRevision {
		return 0, 0, 0, ErrDeployGateConflict
	}
	from, err = appliedCount(ctx, tx)
	if err != nil {
		return 0, 0, 0, &DeployError{Category: DeployCategoryLedger}
	}
	if from >= target {
		return 0, 0, 0, &DeployError{Category: DeployCategoryPolicy}
	}
	if from < DeployMigrationFloor {
		return 0, 0, 0, &DeployError{Category: DeployCategoryPolicy, Migration: from + 1}
	}
	if version, manual := DeployManualBetween(from, target); manual {
		return 0, 0, 0, &DeployError{Category: DeployCategoryPolicy, Migration: version}
	}
	if err = applyMigrations(ctx, tx, from); err != nil {
		return 0, 0, 0, deployDBError(err, DeployCategoryMigration, 0)
	}
	if err = grantRuntimeRoleTx(ctx, tx, runtimeRole); err != nil {
		return 0, 0, 0, deployDBError(err, DeployCategoryGrant, 0)
	}
	// changed_at stays the closure time; only Close and the reopen set it.
	ownerExpr := "changed_by=$1"
	if keepGateOwner {
		// Preserve the owner exactly, including NULL.
		ownerExpr = "changed_by=changed_by"
	}
	args := []any{}
	if !keepGateOwner {
		args = append(args, actor)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_gate SET revision=revision+1,`+ownerExpr+` WHERE singleton`, args...); err != nil {
		return 0, 0, 0, deployDBError(err, DeployCategoryDatabase, 0)
	}
	if err = recordGateEvent(ctx, tx, recoveryRef.String, generation.String, fmt.Sprintf("deploy_migrate_%d_to_%d", from, target), actor, expectedRevision+1); err != nil {
		return 0, 0, 0, err
	}
	if err = commitDeploy(tx); err != nil {
		return 0, 0, 0, err
	}
	return from, target, expectedRevision + 1, nil
}
