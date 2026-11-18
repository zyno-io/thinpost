package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/zyno-io/thinpost/internal/admin"
)

// authorizeContent resolves an app-scoped object reference for a content read.
// The query must return the object key and the message's Enhanced Protection
// flag. Only Enhanced Protection reads are audited; the audit row (actor, app,
// action and IDs, never content) commits before the key is returned, so no
// object is fetched without it. The flag never changes after insert, so
// the decision is stable within the transaction.
func (r *Repository) authorizeContent(ctx context.Context, actor, appID, action, resource, query string, args ...any) (string, bool, error) {
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var key string
	var protected bool
	err = tx.QueryRowContext(ctx, query, args...).Scan(&key, &protected)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, admin.ErrNotFound
	}
	if err != nil {
		return "", false, err
	}
	if protected {
		if err = recordAudit(ctx, tx, actor, appID, action, resource); err != nil {
			return "", false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", false, err
	}
	return key, protected, nil
}

// AuthorizeBodyRead returns the source object reference. For an Enhanced
// Protection message it first commits a message.body.read audit event.
// Callers must fetch and return that object only for this app.
func (r *Repository) AuthorizeBodyRead(ctx context.Context, actor, appID, messageID string) (string, bool, error) {
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(messageID) {
		return "", false, admin.ErrInvalidInput
	}
	return r.authorizeContent(ctx, actor, appID, "message.body.read", messageID,
		`SELECT object_key,enhanced_protection FROM messages WHERE app_id=$1 AND id=$2`, appID, messageID)
}

// AuthorizeAttachmentRead returns the source object reference for one
// attachment. An index outside 0 through 19 is not found. For an Enhanced
// Protection message it first commits message.attachment.read (resource
// "<messageId>:<index>").
func (r *Repository) AuthorizeAttachmentRead(ctx context.Context, actor, appID, messageID string, index int) (string, bool, error) {
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(messageID) {
		return "", false, admin.ErrInvalidInput
	}
	if index < 0 || index > admin.MaxAttachmentIndex {
		return "", false, admin.ErrNotFound
	}
	return r.authorizeContent(ctx, actor, appID, "message.attachment.read", messageID+":"+strconv.Itoa(index),
		`SELECT object_key,enhanced_protection FROM messages WHERE app_id=$1 AND id=$2`, appID, messageID)
}

// AuthorizeDeliveryHeadersRead returns the final signed MIME object reference
// of a delivery of this app's message. A delivery exists only after
// preparation, so an unprepared message is not found. For an Enhanced
// Protection message it first commits message.delivery.headers.read (resource
// "<messageId>:<deliveryId>").
func (r *Repository) AuthorizeDeliveryHeadersRead(ctx context.Context, actor, appID, messageID, deliveryID string) (string, bool, error) {
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(messageID) || !opaqueID(deliveryID) {
		return "", false, admin.ErrInvalidInput
	}
	return r.authorizeContent(ctx, actor, appID, "message.delivery.headers.read", messageID+":"+deliveryID,
		`SELECT d.final_object_key,m.enhanced_protection FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE m.app_id=$1 AND m.id=$2 AND d.id=$3`, appID, messageID, deliveryID)
}
