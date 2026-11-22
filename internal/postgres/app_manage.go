package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
)

// AppExists is false for an unknown or deleted app. Every app-scoped admin
// route asks it first, so a deleted app is a 404 everywhere.
func (r *Repository) AppExists(ctx context.Context, appID string) (bool, error) {
	if !opaqueID(appID) {
		return false, nil
	}
	var exists bool
	err := r.DB.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 AND deleted_at IS NULL`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return exists, err
}

// RenameApp changes an app's name. Names are not unique. The audit event
// carries the old and new name. Setting the current name changes and audits
// nothing.
func (r *Repository) RenameApp(ctx context.Context, actor, appID, name string) (admin.App, error) {
	var app admin.App
	if !adminText(actor, 256) || !opaqueID(appID) || !adminText(name, 128) {
		return app, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return app, err
	}
	defer tx.Rollback()
	var sendingIP sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,name,active,created_at,sending_ip_id FROM apps WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, appID).
		Scan(&app.ID, &app.Name, &app.Active, &app.CreatedAt, &sendingIP)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.App{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.App{}, err
	}
	if sendingIP.Valid {
		app.SendingIPID = &sendingIP.String
	}
	if app.Name == name {
		return app, tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE apps SET name=$2 WHERE id=$1`, appID, name); err != nil {
		return admin.App{}, err
	}
	auditID, err := newOpaqueID()
	if err != nil {
		return admin.App{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
		VALUES ($1,$2,$3,'app.renamed',$3,jsonb_build_object('from',$4::text,'to',$5::text))`, auditID, actor, appID, app.Name, name); err != nil {
		return admin.App{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.App{}, err
	}
	app.Name = name
	return app, nil
}

// DeleteApp soft-deletes an app in one transaction. The app row stays (every
// message, event and audit row keeps its owner) but it is marked deleted and
// inactive, so claims, admission and webhooks skip it. Its API keys and SMTP
// credentials are revoked, its receiving and bounce domains are deleted (a
// receiving domain can then be claimed by another app), its sending-IP pin is
// released, and its unfinished work is ended in states that already exist:
//
//	messages on_hold                                  -> cancelled (the hold-cancel transition), pending cleared
//	messages queued, preparing, preparation_failed    -> preparation_failed with prepare_failure 'app_deleted', pending cleared
//	deliveries queued, held (policy or tracking),
//	  uncertain                                       -> failed
//	prepared messages left with no live delivery      -> pending cleared
//	webhook endpoints                                 -> inactive
//	webhook deliveries queued, in_flight              -> cancelled (their attempts: unknown)
//
// Left alone: in-flight SMTP sends (a result is still recorded, and a retry
// after the app is deleted fails instead of requeueing) and recovery_hold rows
// (operator recovery owns them). Sender and tracking domain rows stay. The
// inbound advisory lock is taken first, as inbound commit does, and the app
// row is locked NO KEY UPDATE so it does not wait on the KEY SHARE lock that
// domain-check audit inserts take while they hold a bounce-domain row.
func (r *Repository) DeleteApp(ctx context.Context, actor, appID string) error {
	if !adminText(actor, 256) || !opaqueID(appID) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, appLockKey(appID)); err != nil {
		return err
	}
	var locked bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 AND deleted_at IS NULL FOR NO KEY UPDATE`, appID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE apps SET deleted_at=clock_timestamp(),active=false,sending_ip_id=NULL WHERE id=$1`, appID); err != nil {
		return err
	}
	count := func(query string) (int, error) {
		var n int
		err := tx.QueryRowContext(ctx, `WITH r AS (`+query+` RETURNING 1) SELECT count(*) FROM r`, appID).Scan(&n)
		return n, err
	}
	var keys, credentials, held, unprepared, deliveries, endpoints, webhooks int
	if keys, err = count(`UPDATE api_keys SET active=false WHERE app_id=$1 AND active`); err != nil {
		return err
	}
	if credentials, err = count(`UPDATE smtp_credentials SET revoked_at=clock_timestamp() WHERE app_id=$1 AND revoked_at IS NULL`); err != nil {
		return err
	}
	if held, err = count(`UPDATE messages SET state='cancelled',pending=false,hold_resolved_at=clock_timestamp() WHERE app_id=$1 AND state='on_hold'`); err != nil {
		return err
	}
	if unprepared, err = count(`UPDATE messages SET state='preparation_failed',pending=false,prepare_token=NULL,prepare_until=NULL,prepare_failure='app_deleted'
		WHERE app_id=$1 AND state IN ('queued','preparing','preparation_failed')`); err != nil {
		return err
	}
	if deliveries, err = count(`UPDATE deliveries d SET state='failed' FROM messages m
		WHERE m.id=d.message_id AND m.app_id=$1 AND d.state IN ('queued','recipient_policy_held','tracking_held','uncertain')`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE messages m SET pending=false WHERE m.app_id=$1 AND m.pending AND m.state='prepared'
		AND EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id=m.id)
		AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id=m.id AND d.state NOT IN ('accepted','failed'))`, appID); err != nil {
		return err
	}
	if endpoints, err = count(`UPDATE webhook_subscriptions SET active=false WHERE app_id=$1 AND active`); err != nil {
		return err
	}
	// Lock the pending delivery rows first, waiting out any racing claim; the
	// next statements take fresh snapshots and see an attempt the claim inserted.
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM webhook_deliveries WHERE app_id=$1 AND state IN ('queued','in_flight') FOR UPDATE`, appID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE webhook_attempts a SET state='unknown',finished_at=clock_timestamp() FROM webhook_deliveries w
		WHERE w.app_id=$1 AND w.state='in_flight' AND a.id=w.claim_token AND a.state='in_flight'`, appID); err != nil {
		return err
	}
	if webhooks, err = count(`UPDATE webhook_deliveries SET state='cancelled',claim_token=NULL,claim_until=NULL WHERE app_id=$1 AND state IN ('queued','in_flight')`); err != nil {
		return err
	}
	// End the app's forwards of recipients' unsubscribes to senders' own links.
	// Lock them first, waiting out a racing claim: a worker's later commit then
	// finds its lease gone, like a webhook attempt's.
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM unsubscribe_forwards f JOIN deliveries d ON d.id=f.delivery_id JOIN messages m ON m.id=d.message_id
		WHERE m.app_id=$1 AND f.state IN ('armed','queued','in_flight') FOR UPDATE OF f`, appID); err != nil {
		return err
	}
	var forwards int
	if forwards, err = count(`UPDATE unsubscribe_forwards f SET state='failed',claim_token=NULL,claim_until=NULL,next_attempt_at=NULL,last_failure='app_deleted',finished_at=clock_timestamp()
		FROM deliveries d JOIN messages m ON m.id=d.message_id
		WHERE d.id=f.delivery_id AND m.app_id=$1 AND f.state IN ('armed','queued','in_flight')`); err != nil {
		return err
	}
	domains, err := deleteAppDomains(ctx, tx, actor, appID, "inbound_domains", "inbound_domain.deleted")
	if err != nil {
		return err
	}
	bounce, err := deleteAppDomains(ctx, tx, actor, appID, "bounce_domains", "bounce_domain.removed")
	if err != nil {
		return err
	}
	auditID, err := newOpaqueID()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
		VALUES ($1,$2,$3,'app.deleted',$3,jsonb_build_object('api_keys_revoked',$4::integer,'smtp_credentials_revoked',$5::integer,'inbound_domains_removed',$6::integer,
		'bounce_domains_removed',$7::integer,'held_messages_cancelled',$8::integer,'unsent_messages_ended',$9::integer,'deliveries_failed',$10::integer,
		'webhook_endpoints_disabled',$11::integer,'webhook_deliveries_cancelled',$12::integer,'unsubscribe_forwards_ended',$13::integer))`,
		auditID, actor, appID, keys, credentials, domains, bounce, held, unprepared, deliveries, endpoints, webhooks, forwards); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteAppDomains deletes every row of an app's domain table and writes one
// audit event per domain, as the single-domain removal does.
func deleteAppDomains(ctx context.Context, tx *sql.Tx, actor, appID, table, action string) (int, error) {
	rows, err := tx.QueryContext(ctx, `DELETE FROM `+table+` WHERE app_id=$1 RETURNING domain`, appID)
	if err != nil {
		return 0, err
	}
	var domains []string
	for rows.Next() {
		var domain string
		if err = rows.Scan(&domain); err != nil {
			rows.Close()
			return 0, err
		}
		domains = append(domains, domain)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, domain := range domains {
		if err = recordAudit(ctx, tx, actor, appID, action, domain); err != nil {
			return 0, err
		}
	}
	return len(domains), nil
}
