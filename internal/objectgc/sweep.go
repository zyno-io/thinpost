// Package objectgc implements a bounded orphan sweep. It is not started by
// the runtime; deployment requires separately provisioned collector identities
// and the recovery and fencing controls documented for object storage.
package objectgc

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/objectkey"
)

type Store interface {
	List(context.Context, string, string, int) ([]string, error)
	RemoveOlderThan(context.Context, string, time.Time) (bool, error)
}

type Repository interface {
	CollectUnreferenced(context.Context, string, time.Time, func(context.Context, string, time.Time) (bool, error)) (bool, error)
}

type Result struct {
	Cursor                          string
	Scanned, Deleted, Kept, Skipped int
	Done                            bool
}

// Sweep processes at most limit objects in one prefix. Cursor is opaque and
// must not be logged: on error it identifies the last fully handled object so
// the failed object is retried. A complete scan starts again with an empty
// cursor; objects uploaded after a scan passes their key appear on a later one.
func Sweep(ctx context.Context, repo Repository, store Store, kind, cursor string, horizon time.Time, limit int) (Result, error) {
	result := Result{Cursor: cursor}
	if repo == nil || store == nil || horizon.IsZero() || limit < 1 || limit > 500 || (kind != "submissions" && kind != "final" && kind != "bounces" && kind != "tracking" && kind != "inbound") {
		return result, errors.New("invalid object collection configuration")
	}
	prefix := kind + "/v1/"
	if cursor != "" && (!strings.HasPrefix(cursor, prefix) || len(cursor) > 1024) {
		return result, errors.New("invalid object collection cursor")
	}
	if ctx.Err() != nil {
		return result, errors.New("object collection unavailable")
	}
	keys, err := store.List(ctx, prefix, cursor, limit)
	if err != nil || ctx.Err() != nil || len(keys) > limit {
		return result, errors.New("object listing unavailable")
	}
	for _, key := range keys {
		if ctx.Err() != nil {
			return result, errors.New("object collection unavailable")
		}
		if !strings.HasPrefix(key, prefix) || len(key) > 1024 || key <= result.Cursor {
			return result, errors.New("invalid object listing order")
		}
		parsed, err := objectkey.Parse(key)
		if err != nil || parsed.Kind != kind {
			result.Skipped++
		} else {
			// Version-one keys sort by their fixed-width issuance timestamp.
			// Later valid keys cannot be eligible in this scan.
			if parsed.Issued.After(horizon) {
				result.Done = true
				return result, nil
			}
			deleted, err := repo.CollectUnreferenced(ctx, key, horizon, store.RemoveOlderThan)
			if err != nil {
				return result, errors.New("object collection unavailable")
			}
			if deleted {
				result.Deleted++
			} else {
				result.Kept++
			}
		}
		result.Scanned++
		result.Cursor = key
	}
	result.Done = len(keys) < limit
	return result, nil
}
