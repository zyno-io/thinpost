package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/objectkey"
)

const minObjectRetention = 24 * time.Hour
const maxObjectRetention = 365 * 24 * time.Hour

// AdvanceObjectGCHorizon requires a separate maintenance identity. It drains
// admitted runtime transactions through the exclusive gate lock and advances
// a durable fence before any collector may delete old objects. It does not
// establish external writer fencing or make a restored database safe.
func AdvanceObjectGCHorizon(ctx context.Context, db *sql.DB, generation, actor string, retention time.Duration) (time.Time, error) {
	if db == nil || !recoveryID(generation) || !adminText(actor, 256) || retention < minObjectRetention || retention > maxObjectRetention || retention%time.Second != 0 {
		return time.Time{}, errors.New("invalid object collection horizon request")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	var stored sql.NullString
	var horizon sql.NullTime
	var mode string
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT generation,mode,object_gc_horizon,clock_timestamp() FROM runtime_gate WHERE singleton FOR UPDATE`).Scan(&stored, &mode, &horizon, &now); err != nil {
		return time.Time{}, errors.New("cannot inspect object collection gate")
	}
	if !stored.Valid || stored.String != generation || mode != "open" {
		return time.Time{}, ErrRuntimeClosed
	}
	cutoff := now.Add(-retention)
	// A backward clock change or longer retention must not silently make an
	// earlier, persisted horizon authorize premature deletion.
	if horizon.Valid && horizon.Time.After(cutoff) {
		return time.Time{}, errors.New("object collection horizon exceeds retention cutoff")
	}
	var persisted time.Time
	if err = tx.QueryRowContext(ctx, `UPDATE runtime_gate SET object_gc_horizon=$1,revision=revision+1,changed_at=clock_timestamp(),changed_by=$2 WHERE singleton RETURNING object_gc_horizon`, cutoff, actor).Scan(&persisted); err != nil {
		return time.Time{}, errors.New("cannot advance object collection horizon")
	}
	if err = tx.Commit(); err != nil {
		return time.Time{}, errors.New("object collection horizon commit not confirmed")
	}
	return persisted, nil
}

// CollectUnreferenced attempts one deletion. The storage callback must use the
// same bucket and namespace as the writers, and must not disclose keys or data
// in errors. A returned error may mean a delete completed later. The durable
// horizon prevents subsequent references to that exact expired key.
func (r *Repository) CollectUnreferenced(ctx context.Context, key string, expectedHorizon time.Time, remove func(context.Context, string, time.Time) (bool, error)) (bool, error) {
	parsed, err := objectkey.Parse(key)
	if err != nil || expectedHorizon.IsZero() || remove == nil {
		return false, errors.New("invalid collection candidate")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var horizon sql.NullTime
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT object_gc_horizon,clock_timestamp() FROM runtime_gate WHERE singleton`).Scan(&horizon, &now); err != nil || !horizon.Valid {
		return false, errors.New("object collection horizon unavailable")
	}
	if !horizon.Time.Equal(expectedHorizon) {
		return false, errors.New("object collection horizon changed")
	}
	if horizon.Time.After(now.Add(-minObjectRetention)) {
		return false, errors.New("object collection horizon exceeds retention cutoff")
	}
	if parsed.Issued.After(horizon.Time) {
		if err = tx.Commit(); err != nil {
			return false, errors.New("object collection gate check did not commit")
		}
		return false, nil
	}
	lock, err := managedObjectLock(key)
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1::integer,$2::integer)`, lock.first, lock.second); err != nil {
		return false, errors.New("cannot lock object references")
	}
	// This statement obtains a fresh READ COMMITTED snapshot after the lock.
	var referenced bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE object_key=$1) OR EXISTS(SELECT 1 FROM deliveries WHERE final_object_key=$1) OR EXISTS(SELECT 1 FROM bounce_receipts WHERE object_key=$1) OR EXISTS(SELECT 1 FROM tracking_objects WHERE object_key=$1) OR EXISTS(SELECT 1 FROM inbound_messages WHERE object_key=$1)`, key).Scan(&referenced); err != nil {
		return false, errors.New("cannot inspect object references")
	}
	if referenced {
		if err = tx.Commit(); err != nil {
			return false, errors.New("object reference check did not commit")
		}
		return false, nil
	}
	deleted, err := remove(ctx, key, horizon.Time)
	if err != nil {
		return false, errors.New("object deletion not confirmed")
	}
	if err = tx.Commit(); err != nil {
		return false, errors.New("object collection commit not confirmed")
	}
	return deleted, nil
}
