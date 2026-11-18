package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/apikey"
)

func (r *Repository) ListApps(ctx context.Context) ([]admin.App, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT id,name,active,created_at,sending_ip_id FROM apps ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var apps []admin.App
	for rows.Next() {
		var app admin.App
		var sendingIP sql.NullString
		if err = rows.Scan(&app.ID, &app.Name, &app.Active, &app.CreatedAt, &sendingIP); err != nil {
			return nil, err
		}
		if sendingIP.Valid {
			app.SendingIPID = &sendingIP.String
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}
func (r *Repository) CreateApp(ctx context.Context, actor, name string) (admin.App, error) {
	return r.CreateAppWithSendingIP(ctx, actor, name, nil)
}

// CreateAppWithSendingIP creates an app, optionally pinned to a sending IP
// (nil: the Edge's primary address). The IP row is key-share locked so it
// cannot be deleted while the app is inserted; an unknown IP is admin.ErrNotFound.
func (r *Repository) CreateAppWithSendingIP(ctx context.Context, actor, name string, sendingIPID *string) (admin.App, error) {
	var app admin.App
	if !adminText(actor, 256) || !adminText(name, 128) || sendingIPID != nil && !opaqueID(*sendingIPID) {
		return app, admin.ErrInvalidInput
	}
	id, err := newOpaqueID()
	if err != nil {
		return app, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return app, err
	}
	defer tx.Rollback()
	var target any
	if sendingIPID != nil {
		var exists bool
		err = tx.QueryRowContext(ctx, `SELECT true FROM sending_ips WHERE id=$1 FOR KEY SHARE`, *sendingIPID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return admin.App{}, admin.ErrNotFound
		}
		if err != nil {
			return admin.App{}, err
		}
		target = *sendingIPID
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO apps(id,name,sending_ip_id) VALUES ($1,$2,$3) RETURNING id,name,active,created_at`, id, name, target).Scan(&app.ID, &app.Name, &app.Active, &app.CreatedAt)
	if err != nil {
		return app, err
	}
	app.SendingIPID = sendingIPID
	if err = recordAudit(ctx, tx, actor, id, "app.created", id); err != nil {
		return admin.App{}, err
	}
	if sendingIPID != nil {
		auditID, auditErr := newOpaqueID()
		if auditErr != nil {
			return admin.App{}, auditErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
			VALUES ($1,$2,$3,'app.sending_ip.updated',$3,jsonb_build_object('from',NULL::text,'to',$4::text))`, auditID, actor, id, *sendingIPID); err != nil {
			return admin.App{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return admin.App{}, err
	}
	return app, nil
}

// SetAppActive serializes status changes with admission and new SMTP permits.
// A permit committed before deactivation may reach Edge after it and cannot be
// recalled, even if the operator has received the successful pause response.
func (r *Repository) SetAppActive(ctx context.Context, actor, appID string, active bool) error {
	if !adminText(actor, 256) || !opaqueID(appID) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous bool
	err = tx.QueryRowContext(ctx, `SELECT active FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if previous == active {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE apps SET active=$2 WHERE id=$1`, appID, active); err != nil {
		return err
	}
	action := "app.deactivated"
	if active {
		action = "app.activated"
	}
	if err = recordAudit(ctx, tx, actor, appID, action, appID); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *Repository) ListKeys(ctx context.Context, appID string) ([]admin.Key, error) {
	var exists bool
	err := r.DB.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, admin.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT id,name,active,created_at FROM api_keys WHERE app_id=$1 ORDER BY created_at,id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []admin.Key
	for rows.Next() {
		var key admin.Key
		if err = rows.Scan(&key.ID, &key.Name, &key.Active, &key.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
func (r *Repository) IssueKeyForOperator(ctx context.Context, actor, appID, name string) (admin.IssuedKey, error) {
	var issued admin.IssuedKey
	if !adminText(actor, 256) || !opaqueID(appID) || !adminText(name, 128) {
		return issued, admin.ErrInvalidInput
	}
	key, err := apikey.Generate()
	if err != nil {
		return issued, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return issued, err
	}
	defer tx.Rollback()
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 AND active FOR SHARE`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.IssuedKey{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.IssuedKey{}, err
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO api_keys(id,digest,app_id,name) VALUES ($1,$2,$3,$4) RETURNING id,name,active,created_at`, key.ID, key.Digest[:], appID, name).Scan(&issued.Key.ID, &issued.Key.Name, &issued.Key.Active, &issued.Key.CreatedAt)
	if err != nil {
		return admin.IssuedKey{}, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "api_key.created", key.ID); err != nil {
		return admin.IssuedKey{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.IssuedKey{}, err
	}
	issued.Secret = key.Secret
	return issued, nil
}
func (r *Repository) RevokeKeyForOperator(ctx context.Context, actor, appID, keyID string) error {
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(keyID) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `UPDATE api_keys SET active=false WHERE id=$1 AND app_id=$2 AND active RETURNING id`, keyID, appID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err = recordAudit(ctx, tx, actor, appID, "api_key.revoked", keyID); err != nil {
		return err
	}
	return tx.Commit()
}
func recordAudit(ctx context.Context, tx *sql.Tx, actor, appID, action, resource string) error {
	id, err := newOpaqueID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id) VALUES ($1,$2,$3,$4,$5)`, id, actor, appID, action, resource)
	return err
}
func newOpaqueID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
func adminText(value string, max int) bool {
	if len(value) == 0 || len(value) > max || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}
