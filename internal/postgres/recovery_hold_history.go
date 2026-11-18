package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type RecoveryHoldHistoryEntry struct {
	RecoveryID    string    `json:"recovery_id"`
	OriginalState string    `json:"original_state"`
	RecordedAt    time.Time `json:"recorded_at"`
}

type RecoveryHoldHistory struct {
	Generation string                     `json:"generation"`
	RecoveryID string                     `json:"recovery_id"`
	Revision   int64                      `json:"revision"`
	Entity     RecoveryCursor             `json:"entity"`
	Entries    []RecoveryHoldHistoryEntry `json:"entries"`
	Next       string                     `json:"next,omitempty"`
}

// RecoveryHoldHistoryPage returns retained hold records for one entity. Order
// is by recovery ID for stable pagination, not by time or causal order. The
// operator must correlate these records with independently preserved evidence.
func RecoveryHoldHistoryPage(ctx context.Context, db *sql.DB, generation, run string, revision int64, entity RecoveryCursor, after string, limit int) (RecoveryHoldHistory, error) {
	var result RecoveryHoldHistory
	if db == nil || !recoveryID(generation) || !recoveryID(run) || revision <= 0 || entity == (RecoveryCursor{}) || !validRecoveryCursor(entity) || after != "" && !recoveryID(after) || limit < 1 || limit > 100 {
		return result, errors.New("invalid recovery hold history request")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var mode string
	var storedGeneration, storedRun sql.NullString
	var storedRevision int64
	var complete bool
	if err = tx.QueryRowContext(ctx, `SELECT mode,generation,recovery_id,revision,quarantine_complete FROM runtime_gate WHERE singleton FOR SHARE`).Scan(&mode, &storedGeneration, &storedRun, &storedRevision, &complete); err != nil {
		return result, err
	}
	if mode != "recovery" || storedGeneration.String != generation || storedRun.String != run || storedRevision != revision || !complete {
		return result, ErrRecoveryConflict
	}
	pending, err := recoveryWorkPending(ctx, tx, run)
	if err != nil {
		return result, err
	}
	if pending {
		return result, ErrRecoveryConflict
	}
	var current bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM recovery_holds WHERE recovery_id=$1 AND kind=$2 AND entity_id=$3 AND secondary_id=$4)`, run, entity.Kind, entity.EntityID, entity.SecondaryID).Scan(&current)
	if err != nil {
		return result, err
	}
	if !current {
		return result, ErrRecoveryConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT recovery_id,original_state,recorded_at FROM recovery_holds WHERE kind=$1 AND entity_id=$2 AND secondary_id=$3 AND recovery_id>$4 ORDER BY recovery_id LIMIT $5`, entity.Kind, entity.EntityID, entity.SecondaryID, after, limit+1)
	if err != nil {
		return result, err
	}
	entries := make([]RecoveryHoldHistoryEntry, 0, limit+1)
	for rows.Next() {
		var entry RecoveryHoldHistoryEntry
		if err = rows.Scan(&entry.RecoveryID, &entry.OriginalState, &entry.RecordedAt); err != nil {
			rows.Close()
			return result, err
		}
		if !recoveryID(entry.RecoveryID) || !adminText(entry.OriginalState, 512) || entry.RecordedAt.IsZero() {
			rows.Close()
			return result, errors.New("invalid recovery hold history record")
		}
		entries = append(entries, entry)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	result = RecoveryHoldHistory{Generation: generation, RecoveryID: run, Revision: revision, Entity: entity, Entries: entries}
	if len(entries) > limit {
		result.Entries = entries[:limit]
		result.Next = result.Entries[len(result.Entries)-1].RecoveryID
	}
	if err = tx.Commit(); err != nil {
		return RecoveryHoldHistory{}, err
	}
	return result, nil
}
