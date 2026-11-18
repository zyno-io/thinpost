package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

// Each event is fanned out once per active subscription. Keep this bound small
// enough for transaction and backlog capacity; inactive history is retained.
const maxActiveWebhooks = 10

const webhookColumns = `id,format,endpoint_url,description,COALESCE(array_to_string(events,','),''),active,created_at,
	CASE WHEN format='postmark' THEN NULL ELSE secret_created_at END,auth_username,array_to_string(auth_header_names,',')`

func scanWebhook(row rowScanner) (admin.Webhook, error) {
	var item admin.Webhook
	var events string
	var secretCreated sql.NullTime
	var authUser, authHeaders sql.NullString
	if err := row.Scan(&item.ID, &item.Format, &item.Endpoint, &item.Description, &events, &item.Active, &item.CreatedAt, &secretCreated, &authUser, &authHeaders); err != nil {
		return admin.Webhook{}, err
	}
	// Event names and header names contain no comma.
	if events != "" {
		item.Events = strings.Split(events, ",")
	}
	if secretCreated.Valid {
		item.SecretCreatedAt = &secretCreated.Time
	}
	if authUser.Valid || authHeaders.Valid {
		item.Auth = &admin.WebhookAuthSummary{Username: authUser.String}
		if authHeaders.Valid && authHeaders.String != "" {
			item.Auth.HeaderNames = strings.Split(authHeaders.String, ",")
		}
	}
	return item, nil
}

func validWebhookDescription(description string) bool {
	return description == "" || adminText(description, 256)
}

// webhookEvents maps an empty filter to NULL (every event of the format).
func webhookEvents(name string, events []string) ([]string, bool) {
	if len(events) == 0 {
		return nil, true
	}
	return events, format.ValidEvents(name, events)
}

// validSealedAuth checks the display summary the caller stored beside the
// ciphertext; both must be present, and a summary names no secret value.
func validSealedAuth(a *admin.SealedWebhookAuth) bool {
	if a == nil {
		return true
	}
	if len(a.Ciphertext) == 0 {
		return a.Summary.Username == "" && len(a.Summary.HeaderNames) == 0
	}
	if len(a.Ciphertext) > 16384 || len(a.Summary.HeaderNames) > admin.MaxWebhookHeaders || a.Summary.Username == "" && len(a.Summary.HeaderNames) == 0 {
		return false
	}
	for _, name := range a.Summary.HeaderNames {
		if name == "" || strings.ContainsRune(name, ',') {
			return false
		}
	}
	return !strings.ContainsRune(a.Summary.Username, 0)
}

// authSummaryArgs returns the nullable summary columns for a sealed value.
func authSummaryArgs(a *admin.SealedWebhookAuth) (cipher []byte, user *string, names []string) {
	if a == nil || len(a.Ciphertext) == 0 {
		return nil, nil, nil
	}
	if a.Summary.Username != "" {
		user = &a.Summary.Username
	}
	return a.Ciphertext, user, a.Summary.HeaderNames
}

func (r *Repository) ListWebhooks(ctx context.Context, appID string) ([]admin.Webhook, error) {
	if !opaqueID(appID) {
		return nil, admin.ErrInvalidInput
	}
	var found bool
	err := r.DB.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1`, appID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, admin.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT `+webhookColumns+` FROM webhook_subscriptions WHERE app_id=$1 AND deleted_at IS NULL ORDER BY created_at,id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []admin.Webhook
	for rows.Next() {
		item, scanErr := scanWebhook(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// countOtherActiveWebhooks is bounded so legacy over-limit data stays cheap.
func countOtherActiveWebhooks(ctx context.Context, tx *sql.Tx, appID, id string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM webhook_subscriptions WHERE app_id=$1 AND active AND id<>$2 LIMIT $3) bounded`, appID, id, maxActiveWebhooks).Scan(&count)
	return count, err
}

// InsertWebhook stores a new, enabled endpoint. The caller sealed the secret
// (absent for unsigned formats) with SecretAAD and any authentication with
// AuthAAD. Lock order: gate, app, subscription.
func (r *Repository) InsertWebhook(ctx context.Context, actor, appID, id string, in admin.WebhookInput, sealed []byte, auth *admin.SealedWebhookAuth) (admin.Webhook, error) {
	if in.Format == "" {
		in.Format = format.Thinpost
	}
	endpoint, internal, ok := r.WebhookDestinations.NormalizeEndpoint(in.Endpoint)
	_, known := format.Lookup(in.Format)
	events, eventsOK := webhookEvents(in.Format, in.Events)
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(id) || !ok || !known || !eventsOK || !validWebhookDescription(in.Description) ||
		format.Unsigned(in.Format) != (len(sealed) == 0) || !validSealedAuth(auth) ||
		internal && !admin.InternalEndpointPermitted(in.Format, auth != nil && len(auth.Ciphertext) > 0) {
		return admin.Webhook{}, admin.ErrInvalidInput
	}
	if len(sealed) == 0 {
		sealed = nil
	}
	cipher, user, names := authSummaryArgs(auth)
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.Webhook{}, err
	}
	defer tx.Rollback()
	var found bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 AND active FOR UPDATE`, appID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.Webhook{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.Webhook{}, err
	}
	others, err := countOtherActiveWebhooks(ctx, tx, appID, id)
	if err != nil {
		return admin.Webhook{}, err
	}
	if others >= maxActiveWebhooks {
		return admin.Webhook{}, admin.ErrConflict
	}
	item, err := scanWebhook(tx.QueryRowContext(ctx, `INSERT INTO webhook_subscriptions(id,app_id,format,endpoint_url,description,events,secret_ciphertext,auth_ciphertext,auth_username,auth_header_names,active)
		VALUES ($1,$2,$3,$4,$5,$6::text[],$7,$8,$9,$10::text[],true) RETURNING `+webhookColumns, id, appID, in.Format, endpoint, in.Description, events, sealed, cipher, user, names))
	if err != nil {
		return admin.Webhook{}, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "webhook.created", id); err != nil {
		return admin.Webhook{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.Webhook{}, err
	}
	return item, nil
}

// UpdateWebhook applies a partial update. Lock order: gate, app, subscription.
// A non-nil auth replaces the sealed authentication; one with an empty
// ciphertext removes it. The event filter is validated against the row's own
// format.
func (r *Repository) UpdateWebhook(ctx context.Context, actor, appID, id string, p admin.WebhookPatch, auth *admin.SealedWebhookAuth) (admin.Webhook, error) {
	var endpoint *string
	if p.Endpoint != nil {
		normalized, _, ok := r.WebhookDestinations.NormalizeEndpoint(*p.Endpoint)
		if !ok {
			return admin.Webhook{}, admin.ErrInvalidInput
		}
		endpoint = &normalized
	}
	content := p.Endpoint != nil || p.Description != nil || p.Events != nil || auth != nil
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(id) || !content && p.Active == nil || !validSealedAuth(auth) || p.Description != nil && !validWebhookDescription(*p.Description) {
		return admin.Webhook{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.Webhook{}, err
	}
	defer tx.Rollback()
	var appActive bool
	err = tx.QueryRowContext(ctx, `SELECT active FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&appActive)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.Webhook{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.Webhook{}, err
	}
	var wasActive, hadAuth bool
	var rowFormat, rowEndpoint string
	err = tx.QueryRowContext(ctx, `SELECT active,format,endpoint_url,auth_ciphertext IS NOT NULL FROM webhook_subscriptions WHERE app_id=$1 AND id=$2 AND deleted_at IS NULL FOR UPDATE`, appID, id).Scan(&wasActive, &rowFormat, &rowEndpoint, &hadAuth)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.Webhook{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.Webhook{}, err
	}
	// The resulting endpoint, format and credentials together must satisfy the
	// internal destination rules, whichever of them this patch changes.
	finalEndpoint, finalAuth := rowEndpoint, hadAuth
	if endpoint != nil {
		finalEndpoint = *endpoint
	}
	if auth != nil {
		finalAuth = len(auth.Ciphertext) > 0
	}
	if _, internal, known := r.WebhookDestinations.NormalizeEndpoint(finalEndpoint); known && internal && !admin.InternalEndpointPermitted(rowFormat, finalAuth) {
		return admin.Webhook{}, admin.ErrInvalidInput
	}
	var events []string
	if p.Events != nil {
		var eventsOK bool
		if events, eventsOK = webhookEvents(rowFormat, *p.Events); !eventsOK {
			return admin.Webhook{}, admin.ErrInvalidInput
		}
	}
	cipher, user, names := authSummaryArgs(auth)
	toggled := p.Active != nil && *p.Active != wasActive
	if toggled && *p.Active {
		if !appActive {
			return admin.Webhook{}, admin.ErrNotFound
		}
		others, countErr := countOtherActiveWebhooks(ctx, tx, appID, id)
		if countErr != nil {
			return admin.Webhook{}, countErr
		}
		if others >= maxActiveWebhooks {
			return admin.Webhook{}, admin.ErrConflict
		}
	}
	item, err := scanWebhook(tx.QueryRowContext(ctx, `UPDATE webhook_subscriptions SET endpoint_url=COALESCE($3,endpoint_url),description=COALESCE($4,description),
		events=CASE WHEN $5 THEN $6::text[] ELSE events END,active=COALESCE($7,active),
		auth_ciphertext=CASE WHEN $8 THEN $9 ELSE auth_ciphertext END,auth_username=CASE WHEN $8 THEN $10 ELSE auth_username END,
		auth_header_names=CASE WHEN $8 THEN $11::text[] ELSE auth_header_names END
		WHERE app_id=$1 AND id=$2 RETURNING `+webhookColumns, appID, id, endpoint, p.Description, p.Events != nil, events, p.Active, auth != nil, cipher, user, names))
	if err != nil {
		return admin.Webhook{}, err
	}
	if content {
		if err = recordAudit(ctx, tx, actor, appID, "webhook.updated", id); err != nil {
			return admin.Webhook{}, err
		}
	}
	if toggled {
		action := "webhook.disabled"
		if *p.Active {
			action = "webhook.enabled"
		}
		if err = recordAudit(ctx, tx, actor, appID, action, id); err != nil {
			return admin.Webhook{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return admin.Webhook{}, err
	}
	return item, nil
}

// DeleteWebhook tombstones the endpoint, erases its secret and authentication, and cancels its
// queued and in-flight deliveries. History is kept. Lock order: gate, app,
// subscription, deliveries.
func (r *Repository) DeleteWebhook(ctx context.Context, actor, appID, id string) error {
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(id) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var found string
	err = tx.QueryRowContext(ctx, `SELECT id FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	// The tombstone waits for event transactions that fan out to this endpoint.
	err = tx.QueryRowContext(ctx, `UPDATE webhook_subscriptions SET active=false,deleted_at=clock_timestamp(),secret_ciphertext=NULL,auth_ciphertext=NULL,auth_username=NULL,auth_header_names=NULL WHERE app_id=$1 AND id=$2 AND deleted_at IS NULL RETURNING id`, appID, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	// Lock the pending delivery rows first, waiting out any racing claim. The
	// next statements take fresh snapshots (READ COMMITTED), so they see an
	// attempt that claim inserted; a single CTE would share one snapshot and
	// leave that attempt in flight.
	for _, query := range []string{
		`SELECT 1 FROM webhook_deliveries WHERE app_id=$1 AND subscription_id=$2 AND state IN ('queued','in_flight') FOR UPDATE`,
		`UPDATE webhook_attempts a SET state='unknown',finished_at=clock_timestamp() FROM webhook_deliveries w
			WHERE w.app_id=$1 AND w.subscription_id=$2 AND w.state='in_flight' AND a.id=w.claim_token AND a.state='in_flight'`,
		`UPDATE webhook_deliveries SET state='cancelled',claim_token=NULL,claim_until=NULL
			WHERE app_id=$1 AND subscription_id=$2 AND state IN ('queued','in_flight')`,
	} {
		if _, err = tx.ExecContext(ctx, query, appID, id); err != nil {
			return err
		}
	}
	if err = recordAudit(ctx, tx, actor, appID, "webhook.deleted", id); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceWebhookSecret swaps in a newly sealed secret with no overlap. The
// format must match the endpoint's, which is immutable; an unsigned endpoint
// never matches because it has no secret to replace.
func (r *Repository) ReplaceWebhookSecret(ctx context.Context, actor, appID, id, formatName string, sealed []byte) (admin.Webhook, error) {
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(id) || len(sealed) == 0 || format.Unsigned(formatName) {
		return admin.Webhook{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.Webhook{}, err
	}
	defer tx.Rollback()
	var found bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 FOR SHARE`, appID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.Webhook{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.Webhook{}, err
	}
	item, err := scanWebhook(tx.QueryRowContext(ctx, `UPDATE webhook_subscriptions SET secret_ciphertext=$3,secret_created_at=clock_timestamp()
		WHERE app_id=$1 AND id=$2 AND format=$4 AND deleted_at IS NULL RETURNING `+webhookColumns, appID, id, sealed, formatName))
	if errors.Is(err, sql.ErrNoRows) {
		return admin.Webhook{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.Webhook{}, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "webhook.secret_rotated", id); err != nil {
		return admin.Webhook{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.Webhook{}, err
	}
	return item, nil
}

// WebhookFormat returns an endpoint's immutable format.
func (r *Repository) WebhookFormat(ctx context.Context, appID, id string) (string, error) {
	if !opaqueID(appID) || !opaqueID(id) {
		return "", admin.ErrInvalidInput
	}
	var name string
	err := r.DB.QueryRowContext(ctx, `SELECT format FROM webhook_subscriptions WHERE app_id=$1 AND id=$2 AND deleted_at IS NULL`, appID, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", admin.ErrNotFound
	}
	return name, err
}

// WebhookTarget audits a test send and returns the stored endpoint, format and
// sealed secret and authentication. The caller sends outside the transaction.
func (r *Repository) WebhookTarget(ctx context.Context, actor, appID, id string) (admin.WebhookTarget, error) {
	var t admin.WebhookTarget
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(id) {
		return t, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return t, err
	}
	defer tx.Rollback()
	var found bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 FOR SHARE`, appID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return t, admin.ErrNotFound
	}
	if err != nil {
		return t, err
	}
	err = tx.QueryRowContext(ctx, `SELECT endpoint_url,format,secret_ciphertext,auth_ciphertext FROM webhook_subscriptions WHERE app_id=$1 AND id=$2 AND deleted_at IS NULL`, appID, id).Scan(&t.Endpoint, &t.Format, &t.SealedSecret, &t.SealedAuth)
	if errors.Is(err, sql.ErrNoRows) {
		return t, admin.ErrNotFound
	}
	if err != nil {
		return t, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "webhook.tested", id); err != nil {
		return t, err
	}
	if err = tx.Commit(); err != nil {
		return t, err
	}
	return t, nil
}
