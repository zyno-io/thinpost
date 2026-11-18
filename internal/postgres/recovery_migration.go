package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

var recoveryMigrationFiles = []string{
	"migrations/018_tracking_references.sql",
	"migrations/019_tracking_app_budget.sql",
	"migrations/020_retired_tracking_objects.sql",
	"migrations/021_retirement_audit_privileges.sql",
	"migrations/022_tracking_hold_disposition.sql",
	"migrations/023_tracking_send_window.sql",
	"migrations/024_submission_burst.sql",
	"migrations/025_recovery_hold_history_index.sql",
	"migrations/026_delivery_retry_policy.sql",
	"migrations/027_indefinite_tracking.sql",
	"migrations/028_message_metadata.sql",
}

func recoveryMigrationChainSupported() bool {
	if len(migrations) < 28 {
		return false
	}
	for i, file := range recoveryMigrationFiles {
		if migrations[17+i].version != 18+i || migrations[17+i].file != file {
			return false
		}
	}
	return true
}

// CheckRecoveryBootstrapSchema admits only the reviewed schema window for
// inspection, beginning recovery, and the scoped recovery migration.
// It never enables the mail runtime or other maintenance operations.
func CheckRecoveryBootstrapSchema(ctx context.Context, db *sql.DB) error {
	if db == nil || !recoveryMigrationChainSupported() {
		return errors.New("unsupported recovery bootstrap schema")
	}
	count, err := appliedCount(ctx, db)
	if err != nil {
		return err
	}
	if count < 17 {
		return errors.New("unsupported recovery bootstrap schema")
	}
	return nil
}

// CheckRecoveryTargetSchema admits the operations that follow the scoped
// recovery migration. Later migrations are applied by the deploy once recovery
// has finished, so the schema may be newer than the recovery chain.
func CheckRecoveryTargetSchema(ctx context.Context, db *sql.DB) error {
	if err := CheckRecoveryBootstrapSchema(ctx, db); err != nil {
		return err
	}
	if count, err := appliedCount(ctx, db); err != nil || count < 28 {
		return errors.New("unsupported recovery schema")
	}
	return nil
}

// RecoveryMigrateRestoredSchema applies only the reviewed migrations 18–28 to
// schema 17 or newer while the exact gate is in recovery. Later migrations are
// left to the deploy, so held webhook work is not touched by them. Callers must fence all
// old runtimes and isolate the restored database. The transaction advances the
// gate revision and records the actor, invalidating earlier report cursors.
func RecoveryMigrateRestoredSchema(ctx context.Context, db *sql.DB, generation, run, actor string, expectedRevision int64) error {
	if db == nil || !recoveryID(generation) || !recoveryID(run) || !adminText(actor, 256) || expectedRevision <= 0 || expectedRevision == math.MaxInt64 || !recoveryMigrationChainSupported() {
		return errors.New("invalid recovery migration request")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `LOCK TABLE runtime_gate IN ACCESS EXCLUSIVE MODE`); err != nil {
		return err
	}
	var mode string
	var storedGeneration, storedRun sql.NullString
	var revision int64
	if err = tx.QueryRowContext(ctx, `SELECT mode,generation,recovery_id,revision FROM runtime_gate WHERE singleton FOR UPDATE`).Scan(&mode, &storedGeneration, &storedRun, &revision); err != nil {
		return err
	}
	if mode != "recovery" || storedGeneration.String != generation || storedRun.String != run {
		return ErrRecoveryConflict
	}
	count, err := appliedCount(ctx, tx)
	if err != nil {
		return err
	}
	if count >= 28 && revision == expectedRevision+1 {
		var receipt bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM recovery_events WHERE recovery_id=$1 AND generation=$2 AND action ~ '^migrate_schema_(17|18|19|20|21|22|23|24|25|26|27)_to_28$' AND actor=$3 AND revision=$4)`, run, generation, actor, revision).Scan(&receipt)
		if err != nil {
			return err
		}
		if receipt {
			return tx.Commit()
		}
	}
	if count < 17 || count >= 28 || revision != expectedRevision {
		return ErrRecoveryConflict
	}
	for _, item := range migrations[count:28] {
		body, checksum, bodyErr := migrationBody(item)
		if bodyErr != nil {
			return bodyErr
		}
		if _, err = tx.ExecContext(ctx, string(body)); err != nil {
			// PostgreSQL constraint diagnostics can include restored row data.
			// Report the migration number without exposing those diagnostics.
			return fmt.Errorf("recovery migration %d failed", item.version)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, item.version, checksum); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_gate SET revision=revision+1,changed_at=clock_timestamp(),changed_by=$1 WHERE singleton`, actor); err != nil {
		return err
	}
	if err = recordRecoveryEvent(ctx, tx, run, generation, fmt.Sprintf("migrate_schema_%d_to_28", count), actor, revision+1); err != nil {
		return err
	}
	return tx.Commit()
}
