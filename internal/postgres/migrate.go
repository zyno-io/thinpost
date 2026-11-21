package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

const migrationLock int64 = 0x5448494e504f5354

// ErrUnknownMigration reports a ledger entry this binary does not know, which
// includes a database that is newer than the binary. ErrMigrationChecksum
// reports an applied migration whose recorded checksum differs.
var (
	ErrUnknownMigration  = errors.New("unknown or out-of-order migration")
	ErrMigrationChecksum = errors.New("migration checksum mismatch")
)

// migrationApplyError keeps the failing version available without parsing text.
type migrationApplyError struct {
	version int
	err     error
}

func (e *migrationApplyError) Error() string {
	return fmt.Sprintf("apply migration %d: %v", e.version, e.err)
}
func (e *migrationApplyError) Unwrap() error { return e.err }

type migration struct {
	version int
	file    string
}

// Once a migration has been applied anywhere, never change its bytes. Append
// future migrations in order and add their files to the Schema embed pattern.
var migrations = []migration{{version: 1, file: "schema.sql"}, {version: 2, file: "migrations/002_sender_domain_verification.sql"}, {version: 3, file: "migrations/003_bounce_addresses.sql"}, {version: 4, file: "migrations/004_bounce_receipts.sql"}, {version: 5, file: "migrations/005_delivery_start_authorization.sql"}, {version: 6, file: "migrations/006_reconciliation_index.sql"}, {version: 7, file: "migrations/007_runtime_gate.sql"}, {version: 8, file: "migrations/008_route_attempts.sql"}, {version: 9, file: "migrations/009_app_recipient_quotas.sql"}, {version: 10, file: "migrations/010_start_journal_identity.sql"}, {version: 11, file: "migrations/011_bounce_receipt_webhooks.sql"}, {version: 12, file: "migrations/012_preparation_retries.sql"}, {version: 13, file: "migrations/013_pending_message_quota.sql"}, {version: 14, file: "migrations/014_pending_message_index.sql"}, {version: 15, file: "migrations/015_object_gc_horizon.sql"}, {version: 16, file: "migrations/016_resend_batch_receipts.sql"}, {version: 17, file: "migrations/017_webhook_history_indexes.sql"}, {version: 18, file: "migrations/018_tracking_references.sql"}, {version: 19, file: "migrations/019_tracking_app_budget.sql"}, {version: 20, file: "migrations/020_retired_tracking_objects.sql"}, {version: 21, file: "migrations/021_retirement_audit_privileges.sql"}, {version: 22, file: "migrations/022_tracking_hold_disposition.sql"}, {version: 23, file: "migrations/023_tracking_send_window.sql"}, {version: 24, file: "migrations/024_submission_burst.sql"}, {version: 25, file: "migrations/025_recovery_hold_history_index.sql"}, {version: 26, file: "migrations/026_delivery_retry_policy.sql"}, {version: 27, file: "migrations/027_indefinite_tracking.sql"}, {version: 28, file: "migrations/028_message_metadata.sql"}, {version: 29, file: "migrations/029_webhook_endpoints.sql"}, {version: 30, file: "migrations/030_webhook_formats.sql"}, {version: 31, file: "migrations/031_message_sender.sql"}, {version: 32, file: "migrations/032_suppressions.sql"}, {version: 33, file: "migrations/033_tracking_operations.sql"}, {version: 34, file: "migrations/034_attempt_smtp_stage.sql"}, {version: 35, file: "migrations/035_recipient_feedback.sql"}, {version: 36, file: "migrations/036_feedback_loops.sql"}, {version: 37, file: "migrations/037_tracking_defaults.sql"}, {version: 38, file: "migrations/038_shared_message_id.sql"}, {version: 39, file: "migrations/039_inbound.sql"}, {version: 40, file: "migrations/040_sending_ips.sql"}, {version: 41, file: "migrations/041_recipient_kind.sql"}, {version: 42, file: "migrations/042_message_hold.sql"}, {version: 43, file: "migrations/043_console_test_overrides.sql"}, {version: 44, file: "migrations/044_smtp_submission.sql"}, {version: 45, file: "migrations/045_sending_ips_from_edge.sql"}, {version: 46, file: "migrations/046_dkim_keys.sql"}}

// MessageMetadataSchema is the first schema that records submission metadata
// (subject, sender domain and envelope recipients).
const MessageMetadataSchema = 28

func migrationBody(item migration) ([]byte, string, error) {
	body, err := Schema.ReadFile(item.file)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func appliedCount(ctx context.Context, db queryer) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT version,checksum FROM thinpost_schema_migrations ORDER BY version`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var version int
		var checksum string
		if err = rows.Scan(&version, &checksum); err != nil {
			return 0, err
		}
		if count >= len(migrations) || migrations[count].version != version {
			return 0, ErrUnknownMigration
		}
		_, expected, err := migrationBody(migrations[count])
		if err != nil {
			return 0, err
		}
		if checksum != expected {
			return 0, ErrMigrationChecksum
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

// Migrate applies missing embedded migrations under a transaction-scoped
// advisory lock. It must be invoked explicitly, never at application startup.
func Migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	count, err := appliedCount(ctx, tx)
	if err != nil {
		return err
	}
	// Existing installations must close admission before a schema change.
	// Acquire the table lock before the row lock: a later ALTER TABLE of the
	// gate must not deadlock with admitted transactions waiting on that row.
	if count < len(migrations) && count >= 7 {
		if _, err = tx.ExecContext(ctx, `LOCK TABLE runtime_gate IN ACCESS EXCLUSIVE MODE`); err != nil {
			return errors.New("cannot lock admission gate for migration")
		}
		var mode string
		if err = tx.QueryRowContext(ctx, `SELECT mode FROM runtime_gate WHERE singleton=true FOR UPDATE`).Scan(&mode); err != nil || mode != "closed" {
			return errors.New("schema migration requires closed admission")
		}
	}
	if err = applyMigrations(ctx, tx, count); err != nil {
		return err
	}
	return tx.Commit()
}

// applyMigrations applies the embedded migrations after the first `from`
// verified ones inside the caller's transaction, which must already hold the
// advisory lock and, for an existing installation, the closed admission gate.
func applyMigrations(ctx context.Context, tx *sql.Tx, from int) error {
	for _, item := range migrations[from:] {
		body, checksum, err := migrationBody(item)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(body)); err != nil {
			return &migrationApplyError{version: item.version, err: err}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, item.version, checksum); err != nil {
			return err
		}
	}
	return nil
}

// CheckSchema fails closed when a migration is missing, changed, or newer than
// the binary knows about. It performs no DDL.
func CheckSchema(ctx context.Context, db *sql.DB) error {
	count, err := appliedCount(ctx, db)
	if err != nil {
		return err
	}
	if count != len(migrations) {
		return errors.New("schema migration is missing")
	}
	return nil
}

// SchemaVersion reports the number of applied, verified migrations. It performs
// no DDL and fails closed on unknown, reordered or modified migrations.
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	return appliedCount(ctx, db)
}
