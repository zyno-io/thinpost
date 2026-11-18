package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrRecoveryConflict = errors.New("recovery state conflict")

type RecoveryStatus struct {
	Generation, Mode, RecoveryID string
	Revision                     int64
	QuarantineComplete           bool
	ChangedAt                    time.Time
}

func recoveryID(value string) bool { return opaqueID(value) && value == strings.ToLower(value) }

// RecoveryInspect is for a maintenance connection, not a runtime API.
func RecoveryInspect(ctx context.Context, db *sql.DB) (RecoveryStatus, error) {
	var result RecoveryStatus
	var generation, recovery sql.NullString
	err := db.QueryRowContext(ctx, `SELECT generation,mode,recovery_id,revision,quarantine_complete,changed_at FROM runtime_gate WHERE singleton=true`).Scan(&generation, &result.Mode, &recovery, &result.Revision, &result.QuarantineComplete, &result.ChangedAt)
	result.Generation, result.RecoveryID = generation.String, recovery.String
	return result, err
}

// RecoveryBegin must only be called after external worker fencing and DB
// isolation. It rotates the independently configured generation and closes
// admission atomically. Attestations are operational prerequisites, not proof.
func RecoveryBegin(ctx context.Context, db *sql.DB, expectedRevision int64, generation, run, actor string) error {
	if expectedRevision <= 0 || !recoveryID(generation) || !recoveryID(run) || !adminText(actor, 256) {
		return errors.New("invalid recovery request")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldGeneration, oldRun sql.NullString
	var mode string
	var revision int64
	if err = tx.QueryRowContext(ctx, `SELECT generation,mode,recovery_id,revision FROM runtime_gate WHERE singleton=true FOR UPDATE`).Scan(&oldGeneration, &mode, &oldRun, &revision); err != nil {
		return err
	}
	if mode == "recovery" && oldGeneration.String == generation && oldRun.String == run {
		return tx.Commit()
	}
	if revision != expectedRevision || mode == "recovery" || oldGeneration.Valid && oldGeneration.String == generation {
		return ErrRecoveryConflict
	}
	var used bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM recovery_events WHERE recovery_id=$1 OR generation=$2)`, run, generation).Scan(&used); err != nil {
		return err
	}
	if used {
		return ErrRecoveryConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='recovery',recovery_id=$2,revision=revision+1,quarantine_complete=false,changed_at=clock_timestamp(),changed_by=$3 WHERE singleton`, generation, run, actor); err != nil {
		return err
	}
	if err = recordRecoveryEvent(ctx, tx, run, generation, "begin", actor, revision+1); err != nil {
		return err
	}
	return tx.Commit()
}

// RecoveryQuarantineBatch holds a bounded number of records for this exact
// recovery. It preserves their existing fields and records the former state.
// Repeat until complete. No held work is automatically redriven.
func RecoveryQuarantineBatch(ctx context.Context, db *sql.DB, generation, run, actor string) (complete bool, err error) {
	if !recoveryID(generation) || !recoveryID(run) || !adminText(actor, 256) {
		return false, errors.New("invalid recovery request")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var mode string
	var storedGeneration, storedRun sql.NullString
	var revision int64
	var done bool
	err = tx.QueryRowContext(ctx, `SELECT mode,generation,recovery_id,revision,quarantine_complete FROM runtime_gate WHERE singleton=true FOR UPDATE`).Scan(&mode, &storedGeneration, &storedRun, &revision, &done)
	if err != nil {
		return false, err
	}
	if mode != "recovery" || storedGeneration.String != generation || storedRun.String != run {
		return false, ErrRecoveryConflict
	}
	if done {
		pending, pendingErr := recoveryWorkPending(ctx, tx, run)
		if pendingErr != nil {
			return false, pendingErr
		}
		if !pending {
			return true, tx.Commit()
		}
		if _, err = tx.ExecContext(ctx, `UPDATE runtime_gate SET quarantine_complete=false,revision=revision+1,changed_at=clock_timestamp(),changed_by=$1 WHERE singleton`, actor); err != nil {
			return false, err
		}
		revision++
	}
	queries := []string{
		`WITH selected AS (SELECT m.id,m.state FROM messages m WHERE m.state IN ('queued','preparing','recovery_hold')
		 AND NOT EXISTS (SELECT 1 FROM recovery_holds h WHERE h.recovery_id=$1 AND h.kind='message' AND h.entity_id=m.id AND h.secondary_id='') ORDER BY m.id FOR UPDATE LIMIT 128),
		 recorded AS (INSERT INTO recovery_holds(recovery_id,kind,entity_id,original_state) SELECT $1,'message',id,state FROM selected RETURNING entity_id)
		 UPDATE messages SET state='recovery_hold' FROM recorded WHERE messages.id=recorded.entity_id`,
		`WITH selected AS (SELECT d.id,d.state FROM deliveries d WHERE d.state IN ('queued','in_flight','recovery_hold')
		 AND NOT EXISTS (SELECT 1 FROM recovery_holds h WHERE h.recovery_id=$1 AND h.kind='delivery' AND h.entity_id=d.id AND h.secondary_id='') ORDER BY d.id FOR UPDATE LIMIT 128),
		 recorded AS (INSERT INTO recovery_holds(recovery_id,kind,entity_id,original_state) SELECT $1,'delivery',id,state FROM selected RETURNING entity_id)
		 UPDATE deliveries SET state='recovery_hold' FROM recorded WHERE deliveries.id=recorded.entity_id`,
		`WITH selected AS (SELECT w.event_id,w.subscription_id,w.state FROM webhook_deliveries w WHERE w.state IN ('queued','in_flight','recovery_hold')
		 AND NOT EXISTS (SELECT 1 FROM recovery_holds h WHERE h.recovery_id=$1 AND h.kind='webhook' AND h.entity_id=w.event_id AND h.secondary_id=w.subscription_id) ORDER BY w.event_id,w.subscription_id FOR UPDATE LIMIT 128),
		 recorded AS (INSERT INTO recovery_holds(recovery_id,kind,entity_id,secondary_id,original_state) SELECT $1,'webhook',event_id,subscription_id,state FROM selected RETURNING entity_id,secondary_id)
		 UPDATE webhook_deliveries SET state='recovery_hold' FROM recorded WHERE webhook_deliveries.event_id=recorded.entity_id AND webhook_deliveries.subscription_id=recorded.secondary_id`,
	}
	var count int64
	for _, query := range queries {
		result, execErr := tx.ExecContext(ctx, query, run)
		if execErr != nil {
			return false, execErr
		}
		n, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return false, rowsErr
		}
		count += n
	}
	if count == 0 {
		pending, pendingErr := recoveryWorkPending(ctx, tx, run)
		if pendingErr != nil {
			return false, pendingErr
		}
		if pending {
			// A current hold record whose entity was made active again is
			// inconsistent; do not silently complete or loop indefinitely.
			return false, ErrRecoveryConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE runtime_gate SET quarantine_complete=true,revision=revision+1,changed_at=clock_timestamp(),changed_by=$1 WHERE singleton`, actor); err != nil {
			return false, err
		}
		if err = recordRecoveryEvent(ctx, tx, run, generation, "quarantine_complete", actor, revision+1); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return count == 0, nil
}

// RecoveryOpen allows new work only after the recovery quarantine completed.
// It does not release historical holds or verify external fencing/evidence.
func RecoveryOpen(ctx context.Context, db *sql.DB, generation, run, actor string, expectedRevision int64) error {
	if !recoveryID(generation) || !recoveryID(run) || !adminText(actor, 256) || expectedRevision <= 0 {
		return errors.New("invalid recovery request")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var mode string
	var storedGeneration, storedRun sql.NullString
	var revision int64
	var complete bool
	err = tx.QueryRowContext(ctx, `SELECT mode,generation,recovery_id,revision,quarantine_complete FROM runtime_gate WHERE singleton=true FOR UPDATE`).Scan(&mode, &storedGeneration, &storedRun, &revision, &complete)
	if err != nil {
		return err
	}
	if mode == "open" && storedGeneration.String == generation && storedRun.String == run && complete && revision == expectedRevision+1 {
		return tx.Commit()
	}
	if mode != "recovery" || storedGeneration.String != generation || storedRun.String != run || revision != expectedRevision || !complete {
		return ErrRecoveryConflict
	}
	pending, err := recoveryWorkPending(ctx, tx, run)
	if err != nil {
		return err
	}
	if pending {
		return ErrRecoveryConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_gate SET mode='open',revision=revision+1,changed_at=clock_timestamp(),changed_by=$1 WHERE singleton`, actor); err != nil {
		return err
	}
	if err = recordRecoveryEvent(ctx, tx, run, generation, "open", actor, expectedRevision+1); err != nil {
		return err
	}
	return tx.Commit()
}

func recoveryWorkPending(ctx context.Context, tx *sql.Tx, run string) (bool, error) {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages m WHERE m.state IN ('queued','preparing') OR (m.state='recovery_hold' AND NOT EXISTS (SELECT 1 FROM recovery_holds h WHERE h.recovery_id=$1 AND h.kind='message' AND h.entity_id=m.id AND h.secondary_id='')))
		OR EXISTS (SELECT 1 FROM deliveries d WHERE d.state IN ('queued','in_flight') OR (d.state='recovery_hold' AND NOT EXISTS (SELECT 1 FROM recovery_holds h WHERE h.recovery_id=$1 AND h.kind='delivery' AND h.entity_id=d.id AND h.secondary_id='')))
		OR EXISTS (SELECT 1 FROM webhook_deliveries w WHERE w.state IN ('queued','in_flight') OR (w.state='recovery_hold' AND NOT EXISTS (SELECT 1 FROM recovery_holds h WHERE h.recovery_id=$1 AND h.kind='webhook' AND h.entity_id=w.event_id AND h.secondary_id=w.subscription_id)))`, run).Scan(&pending)
	return pending, err
}

func recordRecoveryEvent(ctx context.Context, tx *sql.Tx, run, generation, action, actor string, revision int64) error {
	id, err := newOpaqueID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO recovery_events(id,recovery_id,generation,action,actor,revision) VALUES ($1,$2,$3,$4,$5,$6)`, id, run, generation, action, actor, revision)
	return err
}
