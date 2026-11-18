package postgres

import (
	"context"
	"database/sql"
	"errors"
)

type RecoveryObjectCursor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type RecoveryObjectPage struct {
	Generation string                 `json:"generation"`
	RecoveryID string                 `json:"recovery_id"`
	Revision   int64                  `json:"revision"`
	Checked    int                    `json:"checked"`
	Failed     []RecoveryObjectCursor `json:"failed"`
	Next       *RecoveryObjectCursor  `json:"next"`
}

// RecoveryObjectInventoryPage reads and authenticates a bounded page of references.
// verify must perform a full authenticated read, not a presence check. The
// caller must independently fence writers and collectors, isolate the database
// and keep the restored database, bucket and keyring stable for the whole scan.
// A revision-bound page cannot establish that separate backups are coordinated.
func RecoveryObjectInventoryPage(ctx context.Context, db *sql.DB, generation, run string, revision int64, after RecoveryObjectCursor, limit int, verify func(context.Context, string) error) (RecoveryObjectPage, error) {
	var page RecoveryObjectPage
	kinds := []struct{ name, query string }{
		{"bounce", `SELECT id,object_key FROM bounce_receipts WHERE ($1::boolean OR id>$2) ORDER BY id LIMIT $3`},
		{"delivery", `SELECT id,final_object_key FROM deliveries WHERE ($1::boolean OR id>$2) ORDER BY id LIMIT $3`},
		{"inbound", `SELECT id,object_key FROM inbound_messages WHERE content_expires_at>clock_timestamp() AND ($1::boolean OR id>$2) ORDER BY id LIMIT $3`},
		{"message", `SELECT id,object_key FROM messages WHERE ($1::boolean OR id>$2) ORDER BY id LIMIT $3`},
		{"tracking", `SELECT delivery_id,object_key FROM tracking_objects WHERE object_key IS NOT NULL AND ($1::boolean OR delivery_id>$2) ORDER BY delivery_id LIMIT $3`},
	}
	validCursor := after == (RecoveryObjectCursor{})
	for _, kind := range kinds {
		if after.Kind == kind.name && adminText(after.ID, 512) {
			validCursor = true
		}
	}
	if db == nil || verify == nil || !recoveryID(generation) || !recoveryID(run) || revision <= 0 || limit < 1 || limit > 100 || !validCursor {
		return page, errors.New("invalid recovery object inventory request")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var mode string
	var storedGeneration, storedRun sql.NullString
	var storedRevision int64
	var complete bool
	err = tx.QueryRowContext(ctx, `SELECT mode,generation,recovery_id,revision,quarantine_complete FROM runtime_gate WHERE singleton FOR SHARE`).Scan(&mode, &storedGeneration, &storedRun, &storedRevision, &complete)
	if err != nil {
		return page, err
	}
	if mode != "recovery" || storedGeneration.String != generation || storedRun.String != run || storedRevision != revision || !complete {
		return page, ErrRecoveryConflict
	}
	pending, err := recoveryWorkPending(ctx, tx, run)
	if err != nil {
		return page, err
	}
	if pending {
		return page, ErrRecoveryConflict
	}
	type reference struct {
		RecoveryObjectCursor
		key string
	}
	// A restored database can still be at a schema before 39, which has no
	// inbound_messages; resolve it once for this page and skip the kind then.
	var hasInbound bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass('inbound_messages') IS NOT NULL`).Scan(&hasInbound); err != nil {
		return page, err
	}
	refs := make([]reference, 0, limit+1)
	for _, kind := range kinds {
		if kind.name == "inbound" && !hasInbound {
			continue
		}
		if after.Kind != "" && kind.name < after.Kind {
			continue
		}
		start := ""
		if kind.name == after.Kind {
			start = after.ID
		}
		rows, queryErr := tx.QueryContext(ctx, kind.query, kind.name != after.Kind, start, limit+1-len(refs))
		if queryErr != nil {
			return page, queryErr
		}
		for rows.Next() {
			ref := reference{RecoveryObjectCursor: RecoveryObjectCursor{Kind: kind.name}}
			if err = rows.Scan(&ref.ID, &ref.key); err != nil {
				rows.Close()
				return page, err
			}
			if !adminText(ref.ID, 512) {
				rows.Close()
				return page, errors.New("invalid recovery object reference")
			}
			refs = append(refs, ref)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return page, err
		}
		rows.Close()
		if len(refs) > limit {
			break
		}
	}
	page = RecoveryObjectPage{Generation: generation, RecoveryID: run, Revision: revision, Failed: []RecoveryObjectCursor{}}
	if len(refs) > limit {
		refs = refs[:limit]
		last := refs[len(refs)-1].RecoveryObjectCursor
		page.Next = &last
	}
	for _, ref := range refs {
		if err = verify(ctx, ref.key); err != nil {
			if ctx.Err() != nil {
				return RecoveryObjectPage{}, ctx.Err()
			}
			page.Failed = append(page.Failed, ref.RecoveryObjectCursor)
		}
		page.Checked++
	}
	if err = tx.Commit(); err != nil {
		return RecoveryObjectPage{}, err
	}
	return page, nil
}
