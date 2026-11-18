package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/zyno-io/thinpost/internal/webhook/format"
)

var errWebhookFanoutExceeded = errors.New("active webhook subscription limit exceeded")

// enqueueWebhookDeliveries queues the event for every active subscription whose
// format can represent it and whose filter wants it. Classification is the pure
// format.Wants function, so the queue and the claim agree on one mapping.
// LIMIT bounds the number read even when old data violates the current cap; a
// legacy over-limit app fails the caller's transaction rather than dropping an
// arbitrary subset of notifications.
// FOR SHARE OF s makes a concurrent disable, delete or edit wait for this
// transaction: the foreign-key check alone takes only KEY SHARE, which does not
// conflict with their UPDATE, so a stale snapshot could queue a delivery for an
// endpoint that was just removed. The rows stay locked until commit, so the
// insert below sees the same endpoints. Later events wait and re-evaluate active.
func enqueueWebhookDeliveries(ctx context.Context, tx *sql.Tx, eventID, appID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT s.id,s.format,COALESCE(array_to_string(s.events,','),''),e.outcome,e.smtp_code,COALESCE(e.delivery_state,'')
		FROM webhook_subscriptions s JOIN webhook_events e ON e.id=$1 AND e.app_id=s.app_id
		WHERE s.app_id=$2 AND s.active ORDER BY s.id LIMIT $3 FOR SHARE OF s`, eventID, appID, maxActiveWebhooks+1)
	if err != nil {
		return err
	}
	var kept []string
	count := 0
	for rows.Next() {
		var id, name, events string
		var fact format.Fact
		if err = rows.Scan(&id, &name, &events, &fact.Outcome, &fact.SMTPCode, &fact.DeliveryState); err != nil {
			rows.Close()
			return err
		}
		count++
		var filter []string
		if events != "" {
			// Event names contain no comma.
			filter = strings.Split(events, ",")
		}
		if format.Wants(name, filter, fact) {
			kept = append(kept, id)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if count > maxActiveWebhooks {
		return errWebhookFanoutExceeded
	}
	if len(kept) == 0 {
		return nil
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO webhook_deliveries(event_id,subscription_id,app_id) SELECT $1,unnest($3::text[]),$2`, eventID, appID, kept)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || int(n) != len(kept) {
		return errors.New("webhook fanout incomplete")
	}
	return nil
}
