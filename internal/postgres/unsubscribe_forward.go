package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/unsubforward"
	"github.com/zyno-io/thinpost/internal/webhook"
)

// The unsubscribe_forwards table (schema 51) holds, per delivery whose original
// message had its own List-Unsubscribe headers, the host of the sender's link
// and the state of forwarding a one-click unsubscribe to it. The link itself is
// not copied (it can carry a per-recipient token): the worker re-reads it from
// the delivery's stored source and requires the same host. Nothing here is
// logged or returned by the admin API except the host.

// checkedForward returns the forward to write for a prepared delivery. One that
// is not exactly what unsubforward.Decide produces is recorded as unusable
// instead of failing the preparation: a forward is never a reason to lose mail.
func checkedForward(f *preparation.Forward) *preparation.Forward {
	if validForward(f) {
		return f
	}
	return &preparation.Forward{Reason: unsubforward.ReasonUnusable}
}

func validForward(f *preparation.Forward) bool {
	if f.Host != "" {
		host, ok := admin.NormalizeDomain(f.Host)
		return ok && host == f.Host && f.Reason == ""
	}
	switch f.Reason {
	case unsubforward.ReasonMailtoOnly, unsubforward.ReasonNoOneClick, unsubforward.ReasonUnusable:
		return true
	}
	return false
}

// insertForward records a delivery's forward inside the preparation commit.
func insertForward(ctx context.Context, tx *sql.Tx, deliveryID string, f *preparation.Forward) error {
	if f.Host != "" {
		_, err := tx.ExecContext(ctx, `INSERT INTO unsubscribe_forwards(delivery_id,state,target_host) VALUES ($1,'armed',$2)`, deliveryID, f.Host)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO unsubscribe_forwards(delivery_id,state,reason) VALUES ($1,'not_forwarded',$2)`, deliveryID, f.Reason)
	return err
}

// queueForward turns an armed forward into a queued one in the unsubscribe's
// transaction and reports whether it did. It runs for the first request and for
// repeats, but only an armed row changes, so a delivery is forwarded at most
// once however often the recipient clicks or the POST is replayed.
func queueForward(ctx context.Context, tx *sql.Tx, deliveryID string) (bool, error) {
	// Not for a deleted app: its forwards were ended with it.
	result, err := tx.ExecContext(ctx, `UPDATE unsubscribe_forwards f SET state='queued',requested_at=clock_timestamp(),next_attempt_at=clock_timestamp()
		WHERE f.delivery_id=$1 AND f.state='armed'
		AND EXISTS (SELECT 1 FROM deliveries d JOIN messages m ON m.id=d.message_id JOIN apps a ON a.id=m.app_id WHERE d.id=f.delivery_id AND a.deleted_at IS NULL)`, deliveryID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// forwardLine is an oplog line written only after its transaction commits.
type forwardLine struct{ app, message, delivery, state string }

func (l forwardLine) emit() { oplog.UnsubscribeForward(l.app, l.message, l.delivery, l.state) }

// ClaimForward leases the next due forward: queued and due, or in flight with
// an expired lease (a worker died; the POST may already have gone out, which
// the sender's endpoint must tolerate). Leases of forwards that already used
// every attempt are ended as failed first.
func (r *Repository) ClaimForward(ctx context.Context, token string, ttl time.Duration) (unsubforward.Claim, error) {
	var claim unsubforward.Claim
	if !opaqueID(token) || ttl < 20*time.Second || ttl > 5*time.Minute {
		return claim, errors.New("invalid unsubscribe forward claim")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return claim, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `UPDATE unsubscribe_forwards SET state='failed',claim_token=NULL,claim_until=NULL,last_failure='unknown',finished_at=clock_timestamp()
		WHERE delivery_id IN (SELECT delivery_id FROM unsubscribe_forwards WHERE state='in_flight' AND claim_until<=clock_timestamp() AND attempts>=$1 FOR UPDATE SKIP LOCKED LIMIT 16)
		RETURNING delivery_id`, unsubforward.MaxAttempts)
	if err != nil {
		return claim, err
	}
	var ended []string
	var lines []forwardLine
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return claim, err
		}
		ended = append(ended, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return claim, err
	}
	rows.Close()
	for _, id := range ended {
		line, lineErr := forwardIDs(ctx, tx, id, "failed")
		if lineErr != nil {
			return claim, lineErr
		}
		lines = append(lines, line)
	}
	err = tx.QueryRowContext(ctx, `SELECT f.delivery_id,f.target_host,f.attempts,m.object_key,m.enhanced_protection FROM unsubscribe_forwards f
		JOIN deliveries d ON d.id=f.delivery_id JOIN messages m ON m.id=d.message_id JOIN apps a ON a.id=m.app_id
		WHERE f.attempts<$1 AND a.deleted_at IS NULL AND ((f.state='queued' AND f.next_attempt_at<=clock_timestamp()) OR (f.state='in_flight' AND f.claim_until<=clock_timestamp()))
		ORDER BY COALESCE(f.next_attempt_at,f.claim_until),f.delivery_id FOR UPDATE OF f SKIP LOCKED LIMIT 1`, unsubforward.MaxAttempts).Scan(&claim.DeliveryID, &claim.Host, &claim.Attempt, &claim.SourceKey, &claim.EnhancedProtection)
	if errors.Is(err, sql.ErrNoRows) {
		if commitErr := tx.Commit(); commitErr != nil {
			return claim, commitErr
		}
		for _, line := range lines {
			line.emit()
		}
		return unsubforward.Claim{}, webhook.ErrNoWork
	}
	if err != nil {
		return unsubforward.Claim{}, err
	}
	claim.Attempt++
	claim.Token = token
	if _, err = tx.ExecContext(ctx, `UPDATE unsubscribe_forwards SET state='in_flight',attempts=$2,claim_token=$3,claim_until=clock_timestamp()+$4*interval '1 millisecond' WHERE delivery_id=$1`, claim.DeliveryID, claim.Attempt, token, ttl.Milliseconds()); err != nil {
		return unsubforward.Claim{}, err
	}
	if err = tx.Commit(); err != nil {
		return unsubforward.Claim{}, err
	}
	for _, line := range lines {
		line.emit()
	}
	return claim, nil
}

// CommitForward records an attempt's outcome, only for the live lease. A done
// forward is delivered; a retryable failure is queued again with backoff until
// the attempts are used up; anything else is failed. Finished forwards drop the
// sender's link.
func (r *Repository) CommitForward(ctx context.Context, claim unsubforward.Claim, outcome unsubforward.Outcome) error {
	if !opaqueID(claim.Token) || !opaqueID(claim.DeliveryID) || outcome.Status < 0 || outcome.Status > 599 || outcome.Status > 0 && outcome.Status < 100 {
		return errors.New("invalid unsubscribe forward result")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT attempts FROM unsubscribe_forwards WHERE delivery_id=$1 AND state='in_flight' AND claim_token=$2 AND claim_until>clock_timestamp() FOR UPDATE`, claim.DeliveryID, claim.Token).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return webhook.ErrLostClaim
	}
	if err != nil {
		return err
	}
	var status any
	if outcome.Status > 0 {
		status = outcome.Status
	}
	var failure any
	if outcome.Failure != "" {
		failure = outcome.Failure
	}
	state := "failed"
	switch {
	case outcome.Done:
		state, failure = "delivered", nil
	case outcome.Retry && attempts < unsubforward.MaxAttempts:
		state = "queued"
	}
	delay := unsubforward.RetryDelay(attempts)
	if state == "queued" {
		_, err = tx.ExecContext(ctx, `UPDATE unsubscribe_forwards SET state='queued',claim_token=NULL,claim_until=NULL,last_status=$2,last_failure=$3,next_attempt_at=clock_timestamp()+$4*interval '1 millisecond' WHERE delivery_id=$1`, claim.DeliveryID, status, failure, delay.Milliseconds())
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE unsubscribe_forwards SET state=$4,claim_token=NULL,claim_until=NULL,next_attempt_at=NULL,last_status=$2,last_failure=$3,finished_at=clock_timestamp() WHERE delivery_id=$1`, claim.DeliveryID, status, failure, state)
	}
	if err != nil {
		return err
	}
	var line forwardLine
	if state != "queued" {
		if line, err = forwardIDs(ctx, tx, claim.DeliveryID, state); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if line.state != "" {
		line.emit()
	}
	return nil
}

// forwardIDs reads the IDs the oplog line for a forward carries.
func forwardIDs(ctx context.Context, tx *sql.Tx, deliveryID, state string) (forwardLine, error) {
	line := forwardLine{delivery: deliveryID, state: state}
	err := tx.QueryRowContext(ctx, `SELECT m.app_id,m.id FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE d.id=$1`, deliveryID).Scan(&line.app, &line.message)
	return line, err
}

// forwardStatus is what the admin API shows for a forward after the recipient
// unsubscribed. queued covers waiting and retrying.
func forwardStatus(state string) string {
	switch state {
	case "delivered", "failed", "not_forwarded":
		return state
	}
	return "queued"
}
