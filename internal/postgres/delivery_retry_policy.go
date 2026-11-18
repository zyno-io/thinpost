package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
)

// RetryPolicyStatus is the stored policy with its revision. The policy lives
// in delivery_retry_policy (schema 33); before that it lived on runtime_gate.
type RetryPolicyStatus struct {
	Policy    delivery.RetryPolicy
	Revision  int64
	ChangedAt time.Time
	ChangedBy string
}

const retryPolicyColumns = `max_attempts,initial_ms,max_delay_ms,max_age_ms,revision,changed_at,COALESCE(changed_by,'')`

func readRetryPolicy(row *sql.Row, status *RetryPolicyStatus) error {
	var initial, maxDelay, maxAge int64
	if err := row.Scan(&status.Policy.MaxAttempts, &initial, &maxDelay, &maxAge, &status.Revision, &status.ChangedAt, &status.ChangedBy); err != nil {
		return err
	}
	status.Policy.InitialDelay = time.Duration(initial) * time.Millisecond
	status.Policy.MaxDelay = time.Duration(maxDelay) * time.Millisecond
	status.Policy.MaxAge = time.Duration(maxAge) * time.Millisecond
	return status.Policy.Validate()
}

func queryRetryPolicy(ctx context.Context, q rowQueryer, suffix string) (RetryPolicyStatus, error) {
	var status RetryPolicyStatus
	err := readRetryPolicy(q.QueryRowContext(ctx, `SELECT `+retryPolicyColumns+` FROM delivery_retry_policy WHERE singleton`+suffix), &status)
	return status, err
}

// deliveryRetryPolicy reads the policy in the caller's transaction. A claim or
// result racing a policy change sees either version, which is harmless: the
// policy applies to future scheduling only.
func deliveryRetryPolicy(ctx context.Context, tx *sql.Tx) (delivery.RetryPolicy, error) {
	status, err := queryRetryPolicy(ctx, tx, "")
	return status.Policy, err
}

func InspectRetryPolicy(ctx context.Context, db *sql.DB) (RetryPolicyStatus, error) {
	return queryRetryPolicy(ctx, db, "")
}

func adminRetryPolicy(s RetryPolicyStatus) admin.RetryPolicy {
	return admin.RetryPolicy{
		MaxAttempts:    s.Policy.MaxAttempts,
		InitialDelayMS: s.Policy.InitialDelay.Milliseconds(),
		MaxDelayMS:     s.Policy.MaxDelay.Milliseconds(),
		MaxAgeMS:       s.Policy.MaxAge.Milliseconds(),
		Revision:       s.Revision,
		ChangedAt:      s.ChangedAt,
		ChangedBy:      s.ChangedBy,
	}
}

// SetRetryPolicy replaces the policy from the console. It runs under the open
// gate; a stale revision is a conflict and identical values change nothing and
// write no audit row. The audit row is system-scoped (no app) and carries only
// the old and new numbers.
func (r *Repository) SetRetryPolicy(ctx context.Context, actor string, update admin.RetryPolicyUpdate) (admin.RetryPolicy, error) {
	policy := delivery.RetryPolicy{
		MaxAttempts:  update.MaxAttempts,
		InitialDelay: time.Duration(update.InitialDelayMS) * time.Millisecond,
		MaxDelay:     time.Duration(update.MaxDelayMS) * time.Millisecond,
		MaxAge:       time.Duration(update.MaxAgeMS) * time.Millisecond,
	}
	if !adminText(actor, 256) || update.ExpectedRevision <= 0 || update.InitialDelayMS < 0 || update.MaxDelayMS < 0 || update.MaxAgeMS < 0 ||
		update.InitialDelayMS > 1<<40 || update.MaxDelayMS > 1<<40 || update.MaxAgeMS > 1<<40 || policy.Validate() != nil {
		return admin.RetryPolicy{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.RetryPolicy{}, err
	}
	defer tx.Rollback()
	old, err := queryRetryPolicy(ctx, tx, " FOR UPDATE")
	if err != nil {
		return admin.RetryPolicy{}, err
	}
	if old.Revision != update.ExpectedRevision {
		return admin.RetryPolicy{}, admin.ErrConflict
	}
	if old.Policy == policy {
		if err = tx.Commit(); err != nil {
			return admin.RetryPolicy{}, err
		}
		return adminRetryPolicy(old), nil
	}
	var now RetryPolicyStatus
	err = readRetryPolicy(tx.QueryRowContext(ctx, `UPDATE delivery_retry_policy SET max_attempts=$1,initial_ms=$2,max_delay_ms=$3,max_age_ms=$4,revision=revision+1,changed_at=clock_timestamp(),changed_by=$5
		WHERE singleton AND revision=$6 RETURNING `+retryPolicyColumns, policy.MaxAttempts, update.InitialDelayMS, update.MaxDelayMS, update.MaxAgeMS, actor, update.ExpectedRevision), &now)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.RetryPolicy{}, admin.ErrConflict
	}
	if err != nil {
		return admin.RetryPolicy{}, err
	}
	auditID, err := newOpaqueID()
	if err != nil {
		return admin.RetryPolicy{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
		VALUES ($1,$2,NULL,'retry_policy.updated','delivery_retry_policy',jsonb_build_object(
			'old',jsonb_build_object('max_attempts',$3::integer,'initial_ms',$4::bigint,'max_delay_ms',$5::bigint,'max_age_ms',$6::bigint,'revision',$11::bigint),
			'new',jsonb_build_object('max_attempts',$7::integer,'initial_ms',$8::bigint,'max_delay_ms',$9::bigint,'max_age_ms',$10::bigint,'revision',$12::bigint)))`,
		auditID, actor,
		old.Policy.MaxAttempts, old.Policy.InitialDelay.Milliseconds(), old.Policy.MaxDelay.Milliseconds(), old.Policy.MaxAge.Milliseconds(),
		now.Policy.MaxAttempts, now.Policy.InitialDelay.Milliseconds(), now.Policy.MaxDelay.Milliseconds(), now.Policy.MaxAge.Milliseconds(),
		old.Revision, now.Revision)
	if err != nil {
		return admin.RetryPolicy{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.RetryPolicy{}, err
	}
	return adminRetryPolicy(now), nil
}
