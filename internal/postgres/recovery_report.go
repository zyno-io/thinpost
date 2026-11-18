package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type RecoveryCursor struct {
	Kind        string `json:"kind"`
	EntityID    string `json:"entity_id"`
	SecondaryID string `json:"secondary_id"`
}

type RecoveryAttemptEvidence struct {
	ID              string `json:"id"`
	WorkerID        string `json:"worker_id"`
	Epoch           int64  `json:"epoch"`
	State           string `json:"state"`
	StartAuthorized bool   `json:"start_authorized"`
	JournalID       string `json:"journal_id,omitempty"`
}

type RecoveryDeliveryEvidence struct {
	MessageID        string                   `json:"message_id"`
	CurrentState     string                   `json:"current_state"`
	CurrentAttemptID string                   `json:"current_attempt_id,omitempty"`
	CurrentAttempt   *RecoveryAttemptEvidence `json:"current_attempt,omitempty"`
}

type RecoveryHold struct {
	RecoveryCursor
	OriginalState string                    `json:"original_state"`
	RecordedAt    time.Time                 `json:"recorded_at"`
	Delivery      *RecoveryDeliveryEvidence `json:"delivery,omitempty"`
}

type RecoveryReport struct {
	Generation string          `json:"generation"`
	RecoveryID string          `json:"recovery_id"`
	Revision   int64           `json:"revision"`
	Holds      []RecoveryHold  `json:"holds"`
	Next       *RecoveryCursor `json:"next"`
}

// RecoveryReportPage reads a bounded page of hold evidence under the same
// recovery gate revision. All pages require a completed quarantine and the gate
// to remain in recovery; opening or re-running quarantine invalidates the
// cursor. Delivery evidence reflects only the restored database at the time
// of this page; it does not prove that an attempt did or did not reach SMTP.
// It does not reconcile evidence outside the database.
func RecoveryReportPage(ctx context.Context, db *sql.DB, generation, run string, revision int64, after RecoveryCursor, limit int) (RecoveryReport, error) {
	var report RecoveryReport
	if db == nil || !recoveryID(generation) || !recoveryID(run) || revision <= 0 || limit < 1 || limit > 500 || !validRecoveryCursor(after) {
		return report, errors.New("invalid recovery report request")
	}
	// PostgreSQL row locks are not allowed in a read-only transaction. This
	// transaction performs no writes and holds the gate stable while reading.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	var mode string
	var storedGeneration, storedRun sql.NullString
	var storedRevision int64
	var complete bool
	err = tx.QueryRowContext(ctx, `SELECT mode,generation,recovery_id,revision,quarantine_complete FROM runtime_gate WHERE singleton FOR SHARE`).Scan(&mode, &storedGeneration, &storedRun, &storedRevision, &complete)
	if err != nil {
		return report, err
	}
	if mode != "recovery" || storedGeneration.String != generation || storedRun.String != run || storedRevision != revision || !complete {
		return report, ErrRecoveryConflict
	}
	pending, err := recoveryWorkPending(ctx, tx, run)
	if err != nil {
		return report, err
	}
	if pending {
		return report, ErrRecoveryConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT h.kind,h.entity_id,h.secondary_id,h.original_state,h.recorded_at,
		d.message_id,d.state,d.current_attempt_id,a.id,a.worker_id,a.epoch,a.state,a.start_authorized,a.start_journal_id
		FROM recovery_holds h
		LEFT JOIN deliveries d ON h.kind='delivery' AND d.id=h.entity_id
		LEFT JOIN delivery_attempts a ON a.id=d.current_attempt_id AND a.delivery_id=d.id
		WHERE h.recovery_id=$1 AND (h.kind,h.entity_id,h.secondary_id)>($2,$3,$4)
		ORDER BY h.kind,h.entity_id,h.secondary_id LIMIT $5`, run, after.Kind, after.EntityID, after.SecondaryID, limit+1)
	if err != nil {
		return report, err
	}
	holds := make([]RecoveryHold, 0, limit)
	for rows.Next() {
		var hold RecoveryHold
		var messageID, deliveryState, attemptID, foundAttemptID, workerID, attemptState, journalID sql.NullString
		var epoch sql.NullInt64
		var authorized sql.NullBool
		if err = rows.Scan(&hold.Kind, &hold.EntityID, &hold.SecondaryID, &hold.OriginalState, &hold.RecordedAt,
			&messageID, &deliveryState, &attemptID, &foundAttemptID, &workerID, &epoch, &attemptState, &authorized, &journalID); err != nil {
			rows.Close()
			return report, err
		}
		if hold.Kind == "delivery" && messageID.Valid && deliveryState.Valid {
			hold.Delivery = &RecoveryDeliveryEvidence{MessageID: messageID.String, CurrentState: deliveryState.String, CurrentAttemptID: attemptID.String}
			if foundAttemptID.Valid {
				hold.Delivery.CurrentAttempt = &RecoveryAttemptEvidence{ID: foundAttemptID.String, WorkerID: workerID.String, Epoch: epoch.Int64, State: attemptState.String, StartAuthorized: authorized.Bool, JournalID: journalID.String}
			}
		}
		holds = append(holds, hold)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return report, err
	}
	rows.Close()
	report = RecoveryReport{Generation: generation, RecoveryID: run, Revision: revision, Holds: holds}
	if len(holds) > limit {
		report.Holds = holds[:limit]
		last := report.Holds[len(report.Holds)-1].RecoveryCursor
		if !validRecoveryCursor(last) {
			return RecoveryReport{}, errors.New("recovery report has an invalid cursor")
		}
		report.Next = &last
	}
	if err = tx.Commit(); err != nil {
		return RecoveryReport{}, err
	}
	return report, nil
}

func validRecoveryCursor(cursor RecoveryCursor) bool {
	if cursor == (RecoveryCursor{}) {
		return true
	}
	if cursor.Kind != "message" && cursor.Kind != "delivery" && cursor.Kind != "webhook" {
		return false
	}
	if !adminText(cursor.EntityID, 512) {
		return false
	}
	if cursor.Kind == "webhook" {
		return adminText(cursor.SecondaryID, 512)
	}
	return cursor.SecondaryID == ""
}
