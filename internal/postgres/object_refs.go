package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"sort"
	"time"

	"github.com/zyno-io/thinpost/internal/objectkey"
)

// ObjectVerifier must use a storage endpoint with strong read-after-write and
// read-after-delete semantics. Errors must not expose object contents or keys.
type ObjectVerifier interface {
	VerifyPresent(context.Context, string) error
}

type objectLock struct{ first, second int32 }

func managedObjectLock(key string) (objectLock, error) {
	if _, err := objectkey.Parse(key); err != nil {
		return objectLock{}, errors.New("invalid managed object key")
	}
	sum := sha256.Sum256([]byte("thinpost-object-reference-v1\x00" + key))
	return objectLock{int32(binary.BigEndian.Uint32(sum[:4])), int32(binary.BigEndian.Uint32(sum[4:8]))}, nil
}

// lockAndVerifyObjects acquires transaction-scoped locks in a global order,
// then checks each object before any reference can be committed. An eventual
// collector must take the same lock and recheck references; it must also honor
// key expiry because a timed-out delete can finish after the lock is released.
// A missing or uncheckable object aborts the transaction.
func (r *Repository) lockAndVerifyObjects(ctx context.Context, tx *sql.Tx, keys []string) error {
	// Preparation may commit a final object and one tracking object for each
	// of its at most 100 recipients. Other callers retain their own limits.
	if r.Objects == nil || len(keys) == 0 || len(keys) > 200 {
		return errors.New("object verification unavailable")
	}
	locks := make([]objectLock, 0, len(keys))
	seen := make(map[objectLock]bool, len(keys))
	for _, key := range keys {
		lock, err := managedObjectLock(key)
		if err != nil {
			return err
		}
		if !seen[lock] {
			locks = append(locks, lock)
			seen[lock] = true
		}
	}
	sort.Slice(locks, func(i, j int) bool {
		return locks[i].first < locks[j].first || locks[i].first == locks[j].first && locks[i].second < locks[j].second
	})
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, lock := range locks {
		if _, err := tx.ExecContext(verifyCtx, `SELECT pg_advisory_xact_lock($1::integer,$2::integer)`, lock.first, lock.second); err != nil {
			return errors.New("cannot lock object references")
		}
	}
	for _, key := range keys {
		if err := r.Objects.VerifyPresent(verifyCtx, key); err != nil {
			return errors.New("object presence cannot be verified")
		}
	}
	// A delete can return an error and still finish later, after its lock has
	// gone away. Never create a new reference to an expired upload key: a
	// collector may already have issued a delete for that exact key.
	var now time.Time
	var horizon sql.NullTime
	if err := tx.QueryRowContext(verifyCtx, `SELECT clock_timestamp(),object_gc_horizon FROM runtime_gate WHERE singleton=true`).Scan(&now, &horizon); err != nil {
		return errors.New("database clock unavailable")
	}
	for _, key := range keys {
		parsed, err := objectkey.Parse(key)
		if err != nil || !parsed.ReferencableAt(now) || (horizon.Valid && !parsed.Issued.After(horizon.Time)) {
			return errors.New("object reference window expired")
		}
	}
	return nil
}
