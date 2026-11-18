package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/providerid"
)

// messageIDHeader is the RFC 5322 Message-ID a delivery carries, derived from
// the message's shared_message_id flag (read in the same statement as the
// delivery rows). It is empty without a recorded sender domain and for an
// event with no delivery (such as preparation_failed), even when a later
// retry prepared the message in the shared form.
func messageIDHeader(shared bool, messageID, deliveryID, domain string) string {
	switch {
	case domain == "" || deliveryID == "":
		return ""
	case shared:
		return providerid.MessageIDHeader(messageID, domain)
	}
	return providerid.MessageIDHeader(deliveryID, domain)
}

// loadMessageIDHeader reads the message's flag and domain in one statement. A
// missing message leaves the value empty.
func loadMessageIDHeader(ctx context.Context, tx *sql.Tx, messageID, appID, deliveryID string) (string, error) {
	var shared bool
	var domain string
	err := tx.QueryRowContext(ctx, `SELECT m.shared_message_id,COALESCE(m.sender_domain,'') FROM messages m WHERE m.id=$1 AND m.app_id=$2`, messageID, appID).Scan(&shared, &domain)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return messageIDHeader(shared, messageID, deliveryID, domain), nil
}
