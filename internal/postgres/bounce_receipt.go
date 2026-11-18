package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/zyno-io/thinpost/internal/bounceingest"
	"github.com/zyno-io/thinpost/internal/objectkey"
)

// CheckBounce avoids uploading known duplicates and over-limit receipts. The
// subsequent commit rechecks under a row lock because this is only a hint.
func (r *Repository) CheckBounce(ctx context.Context, hash, fingerprint [32]byte) (bounceingest.Status, error) {
	var duplicate bool
	var count int
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return bounceingest.Unknown, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM bounce_receipts WHERE delivery_id=d.id AND fingerprint=$2), (SELECT count(*) FROM bounce_receipts WHERE delivery_id=d.id) FROM deliveries d WHERE d.bounce_token_hash=$1`, hash[:], fingerprint[:]).Scan(&duplicate, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return bounceingest.Unknown, nil
	}
	if err != nil {
		return bounceingest.Unknown, err
	}
	if err = tx.Commit(); err != nil {
		return bounceingest.Unknown, err
	}
	if duplicate {
		return bounceingest.Duplicate, nil
	}
	if count >= 32 {
		return bounceingest.Full, nil
	}
	return bounceingest.New, nil
}

// CommitBounce serializes receipts per delivery, accepts exact replays, and
// limits distinct untrusted receipts to bound abuse until retention is defined.
// It does not alter delivery state or assert that the bounce is authentic.
func (r *Repository) CommitBounce(ctx context.Context, record bounceingest.Record) error {
	object, err := objectkey.Parse(record.ObjectKey)
	if err != nil || object.Kind != "bounces" || object.ID != record.ID || strings.ContainsAny(record.ObjectKey, "\r\n") {
		return errors.New("invalid bounce record")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var deliveryID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM deliveries WHERE bounce_token_hash=$1 FOR UPDATE`, record.TokenHash[:]).Scan(&deliveryID)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("unknown bounce recipient")
	}
	if err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM bounce_receipts WHERE delivery_id=$1 AND fingerprint=$2)`, deliveryID, record.Fingerprint[:]).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return tx.Commit()
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM bounce_receipts WHERE delivery_id=$1`, deliveryID).Scan(&count); err != nil {
		return err
	}
	if count >= 32 {
		return errors.New("bounce receipt limit reached")
	}
	if err = r.lockAndVerifyObjects(ctx, tx, []string{record.ObjectKey}); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO bounce_receipts(id,delivery_id,object_key,fingerprint) VALUES ($1,$2,$3,$4)`, record.ID, deliveryID, record.ObjectKey, record.Fingerprint[:]); err != nil {
		return err
	}
	var appID string
	err = tx.QueryRowContext(ctx, `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,bounce_receipt_id,outcome,smtp_code)
		SELECT $1,m.app_id,m.id,d.id,$1,'bounce_received_unverified',0 FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE d.id=$2 RETURNING app_id`, record.ID, deliveryID).Scan(&appID)
	if err != nil {
		return err
	}
	if err = enqueueWebhookDeliveries(ctx, tx, record.ID, appID); err != nil {
		return err
	}
	return tx.Commit()
}
