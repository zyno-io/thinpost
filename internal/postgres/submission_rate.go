package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/submission"
)

const (
	submissionTokenUnit int64 = 60_000_000
	maxSubmissionRate         = 60_000
	maxSubmissionBurst        = 10_000
	// At one message per minute, this fills the largest possible bucket.
	maxRefillMicroseconds int64 = 600_000_000_000
)

// ReserveSubmission charges the app before object storage is touched. The
// transaction serializes all replicas on the app row and rechecks key state.
// A reservation is consumed even when the later upload or commit fails.
func (r *Repository) ReserveSubmission(ctx context.Context, appID, keyID string, messages int) error {
	if keyID == "" || len(keyID) > 256 {
		return submission.ErrInvalidMessage
	}
	return r.reserveSubmission(ctx, appID, keyID, false, messages)
}

// ReserveOperatorSubmission is ReserveSubmission for an operator submission:
// the same bucket and rate checks, without a key. An inactive app is paused.
func (r *Repository) ReserveOperatorSubmission(ctx context.Context, appID string, messages int) error {
	return r.reserveSubmission(ctx, appID, "", true, messages)
}

func (r *Repository) reserveSubmission(ctx context.Context, appID, keyID string, operator bool, messages int) error {
	if appID == "" || len(appID) > 256 || messages < 1 || messages > 100 {
		return submission.ErrInvalidMessage
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rate, burst int
	var tokens int64
	var last, now time.Time
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT submission_rate_per_minute,submission_burst,submission_tokens,submission_refilled_at,clock_timestamp(),active
		FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&rate, &burst, &tokens, &last, &now, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return submission.ErrInvalidMessage
	}
	if err != nil {
		return err
	}
	if !active {
		if operator {
			return submission.ErrAppPaused
		}
		return submission.ErrInvalidMessage
	}
	if !operator {
		var exists bool
		err = tx.QueryRowContext(ctx, `SELECT true FROM api_keys WHERE id=$1 AND app_id=$2 AND active FOR SHARE`, keyID, appID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return submission.ErrInvalidKey
		}
		if err != nil {
			return err
		}
	}
	if rate < 1 || rate > maxSubmissionRate || burst < 1 || burst > maxSubmissionBurst || tokens < 0 || tokens > int64(burst)*submissionTokenUnit {
		return submission.ErrRateLimited
	}
	available, refilledAt := refillSubmissionTokens(rate, burst, tokens, last, now)
	cost := int64(messages) * submissionTokenUnit
	if available < cost {
		return submission.ErrRateLimited
	}
	if _, err = tx.ExecContext(ctx, `UPDATE apps SET submission_tokens=$2,submission_refilled_at=$3 WHERE id=$1`, appID, available-cost, refilledAt); err != nil {
		return err
	}
	return tx.Commit()
}

func refillSubmissionTokens(rate, burst int, tokens int64, last, now time.Time) (int64, time.Time) {
	elapsed := now.Sub(last).Microseconds()
	if elapsed < 0 {
		elapsed = 0
		now = last
	}
	if elapsed > maxRefillMicroseconds {
		elapsed = maxRefillMicroseconds
	}
	return min(tokens+elapsed*int64(rate), int64(burst)*submissionTokenUnit), now
}

func validSubmissionRate(rate, burst int) bool {
	return (rate == 0 && burst == 0) || (rate >= 1 && rate <= maxSubmissionRate && burst >= 1 && burst <= maxSubmissionBurst)
}

func (r *Repository) GetSubmissionRate(ctx context.Context, appID string) (admin.SubmissionRate, error) {
	if !opaqueID(appID) {
		return admin.SubmissionRate{}, admin.ErrInvalidInput
	}
	var config admin.SubmissionRate
	err := r.DB.QueryRowContext(ctx, `SELECT submission_rate_per_minute,submission_burst FROM apps WHERE id=$1`, appID).Scan(&config.PerMinute, &config.Burst)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.SubmissionRate{}, admin.ErrNotFound
	}
	return config, err
}

// SetSubmissionRate preserves accrued refill using the previous rate before
// applying a changed capacity. An identical save is a no-op. Enabling from
// disabled fills the bucket once; disabling clears it. The app row serializes
// changes with submissions on every replica.
func (r *Repository) SetSubmissionRate(ctx context.Context, actor, appID string, rate, burst int) (admin.SubmissionRate, error) {
	if !adminText(actor, 256) || !opaqueID(appID) || !validSubmissionRate(rate, burst) {
		return admin.SubmissionRate{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.SubmissionRate{}, err
	}
	defer tx.Rollback()
	var oldRate, oldBurst int
	var tokens int64
	var last, now time.Time
	err = tx.QueryRowContext(ctx, `SELECT submission_rate_per_minute,submission_burst,submission_tokens,submission_refilled_at,clock_timestamp() FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&oldRate, &oldBurst, &tokens, &last, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.SubmissionRate{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.SubmissionRate{}, err
	}
	if oldRate == rate && oldBurst == burst {
		if err = tx.Commit(); err != nil {
			return admin.SubmissionRate{}, err
		}
		return admin.SubmissionRate{PerMinute: rate, Burst: burst}, nil
	}
	if now.Before(last) {
		now = last
	}
	switch {
	case rate == 0:
		tokens = 0
	case oldRate == 0:
		tokens = int64(burst) * submissionTokenUnit
	default:
		tokens, now = refillSubmissionTokens(oldRate, oldBurst, tokens, last, now)
		tokens = min(tokens, int64(burst)*submissionTokenUnit)
	}
	_, err = tx.ExecContext(ctx, `UPDATE apps SET submission_rate_per_minute=$2::integer,submission_burst=$3::integer,
		submission_tokens=$4,submission_refilled_at=$5 WHERE id=$1`, appID, rate, burst, tokens, now)
	if err != nil {
		return admin.SubmissionRate{}, err
	}
	auditID, err := newOpaqueID()
	if err != nil {
		return admin.SubmissionRate{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
		VALUES ($1,$2,$3,'app.submission_rate.updated',$3,jsonb_build_object('previous_per_minute',$4::integer,'previous_burst',$5::integer,'per_minute',$6::integer,'burst',$7::integer))`, auditID, actor, appID, oldRate, oldBurst, rate, burst)
	if err != nil {
		return admin.SubmissionRate{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.SubmissionRate{}, err
	}
	return admin.SubmissionRate{PerMinute: rate, Burst: burst}, nil
}
