package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/delivery"
)

// AssignBounceAddress persists correlation before a return path can be used.
// Existing addresses remain tied to their original key version across retries.
func (r *Repository) AssignBounceAddress(ctx context.Context, claim delivery.Claim, active string, hashes map[string][32]byte) (string, error) {
	if !opaqueID(claim.DeliveryID) || !opaqueID(claim.AttemptID) || !leaseID(claim.WorkerID) || claim.Epoch == 0 || active == "" || len(active) > 64 {
		return "", errors.New("invalid bounce assignment")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var version sql.NullString
	var stored []byte
	err = tx.QueryRowContext(ctx, `SELECT d.bounce_key_version,d.bounce_token_hash FROM deliveries d JOIN delivery_attempts a ON a.id=d.current_attempt_id AND a.delivery_id=d.id WHERE d.id=$1 AND d.state='in_flight' AND a.id=$2 AND a.worker_id=$3 AND a.epoch=$4 AND a.state='in_flight' FOR UPDATE OF d`, claim.DeliveryID, claim.AttemptID, claim.WorkerID, claim.Epoch).Scan(&version, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return "", delivery.ErrAttemptSuperseded
	}
	if err != nil {
		return "", err
	}
	selected := active
	if version.Valid {
		selected = version.String
	}
	hash, ok := hashes[selected]
	if !ok {
		return "", errors.New("delivery bounce key unavailable")
	}
	if version.Valid {
		if !bytes.Equal(stored, hash[:]) {
			return "", errors.New("delivery bounce key mismatch")
		}
	} else {
		if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET bounce_key_version=$2,bounce_token_hash=$3 WHERE id=$1`, claim.DeliveryID, selected, hash[:]); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return selected, nil
}
