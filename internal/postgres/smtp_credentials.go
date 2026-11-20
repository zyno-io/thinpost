package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/smtpcred"
)

const smtpCredentialColumns = `id,name,revoked_at IS NULL,created_at,revoked_at,last_used_at`

func scanSMTPCredential(row rowScanner, c *admin.SMTPCredential) error {
	var revoked, used sql.NullTime
	if err := row.Scan(&c.ID, &c.Name, &c.Active, &c.CreatedAt, &revoked, &used); err != nil {
		return err
	}
	c.Username = c.ID
	if revoked.Valid {
		c.RevokedAt = &revoked.Time
	}
	if used.Valid {
		c.LastUsedAt = &used.Time
	}
	return nil
}

// ListSMTPCredentials reads an app's credentials, active and revoked, without
// the runtime gate, like ListKeys. It never returns a digest.
func (r *Repository) ListSMTPCredentials(ctx context.Context, appID string) ([]admin.SMTPCredential, error) {
	if !opaqueID(appID) {
		return nil, admin.ErrInvalidInput
	}
	var exists bool
	err := r.DB.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, admin.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT `+smtpCredentialColumns+` FROM smtp_credentials WHERE app_id=$1 ORDER BY created_at,id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []admin.SMTPCredential{}
	for rows.Next() {
		var c admin.SMTPCredential
		if err = scanSMTPCredential(rows, &c); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// IssueSMTPCredential creates a credential under the apps row lock so that
// concurrent creates cannot pass the active-credential limit. Only the digest
// is stored; the password is returned once.
func (r *Repository) IssueSMTPCredential(ctx context.Context, actor, appID, name string) (admin.IssuedSMTPCredential, error) {
	var issued admin.IssuedSMTPCredential
	if !adminText(actor, 256) || !opaqueID(appID) || !adminText(name, 128) {
		return issued, admin.ErrInvalidInput
	}
	generated, err := smtpcred.Generate()
	if err != nil {
		return issued, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return issued, err
	}
	defer tx.Rollback()
	// Lock the app first and count in a second statement: a count in the same
	// statement would use a snapshot from before the lock wait and let
	// concurrent creates pass the limit.
	var locked bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 AND active FOR UPDATE`, appID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return issued, admin.ErrNotFound
	}
	if err != nil {
		return issued, err
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM smtp_credentials WHERE app_id=$1 AND revoked_at IS NULL`, appID).Scan(&active); err != nil {
		return issued, err
	}
	if active >= smtpcred.MaxActivePerApp {
		return issued, admin.ErrConflict
	}
	err = scanSMTPCredential(tx.QueryRowContext(ctx, `INSERT INTO smtp_credentials(id,app_id,name,digest) VALUES ($1,$2,$3,$4) RETURNING `+smtpCredentialColumns,
		generated.ID, appID, name, generated.Digest[:]), &issued.Credential)
	if err != nil {
		return admin.IssuedSMTPCredential{}, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "smtp_credential.created", generated.ID); err != nil {
		return admin.IssuedSMTPCredential{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.IssuedSMTPCredential{}, err
	}
	issued.Username, issued.Password = generated.ID, generated.Secret
	return issued, nil
}

// RevokeSMTPCredential revokes an active credential of the app. A revoked or
// unknown credential is ErrNotFound.
func (r *Repository) RevokeSMTPCredential(ctx context.Context, actor, appID, credentialID string) error {
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(credentialID) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lock the app before the credential, the same order SMTP submission takes
	// (app, then credential). Updating the credential first and then inserting
	// the audit row (whose foreign key locks the app) would deadlock against an
	// in-flight submission and abort the revocation. A paused app can still
	// revoke.
	var locked bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	var id string
	err = tx.QueryRowContext(ctx, `UPDATE smtp_credentials SET revoked_at=clock_timestamp() WHERE id=$1 AND app_id=$2 AND revoked_at IS NULL RETURNING id`, credentialID, appID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err = recordAudit(ctx, tx, actor, appID, "smtp_credential.revoked", credentialID); err != nil {
		return err
	}
	return tx.Commit()
}
