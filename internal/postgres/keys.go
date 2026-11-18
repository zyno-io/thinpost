package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/apikey"
	"github.com/zyno-io/thinpost/internal/submission"
)

// IssueKey and RevokeKey are storage operations, not authorization checks.
// Only an authenticated administrator may be allowed to call them.
func (r *Repository) IssueKey(ctx context.Context, appID string) (apikey.Generated, error) {
	key, err := apikey.Generate()
	if err != nil {
		return apikey.Generated{}, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return apikey.Generated{}, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `INSERT INTO api_keys(id,digest,app_id) SELECT $1,$2,id FROM apps WHERE id=$3 AND active RETURNING id`, key.ID, key.Digest[:], appID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return apikey.Generated{}, submission.ErrInvalidMessage
	}
	if err != nil {
		return apikey.Generated{}, err
	}
	if err = tx.Commit(); err != nil {
		return apikey.Generated{}, err
	}
	return key, nil
}

func (r *Repository) RevokeKey(ctx context.Context, appID, keyID string) (bool, error) {
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE api_keys SET active=false WHERE id=$1 AND app_id=$2 AND active`, keyID, appID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return rows == 1, nil
}
