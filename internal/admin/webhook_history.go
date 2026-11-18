package admin

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// WebhookDelivery contains operational metadata only; credentials, endpoints,
// request bodies, recipients and claim tokens are never returned here.
type WebhookDelivery struct {
	EventID        string     `json:"event_id"`
	SubscriptionID string     `json:"subscription_id"`
	MessageID      string     `json:"message_id,omitempty"`
	InboundID      string     `json:"inbound_id,omitempty"`
	DeliveryID     string     `json:"delivery_id,omitempty"`
	Outcome        string     `json:"outcome"`
	State          string     `json:"state"`
	Attempts       int        `json:"attempts"`
	OccurredAt     time.Time  `json:"occurred_at"`
	NextAttemptAt  *time.Time `json:"next_attempt_at,omitempty"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
}

type WebhookDeliveryPage struct {
	Deliveries []WebhookDelivery `json:"deliveries"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type WebhookAttempt struct {
	State      string     `json:"state"`
	HTTPStatus *int       `json:"http_status,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type WebhookAttemptPage struct {
	Attempts  []WebhookAttempt `json:"attempts"`
	Truncated bool             `json:"truncated"`
}

// WebhookHistoryStore must scope every operation to appID and atomically audit
// reads and replay. Replay is restricted to a failed delivery of a currently
// active, undeleted endpoint.
type WebhookHistoryStore interface {
	ListWebhookDeliveries(context.Context, string, string, string, int) (WebhookDeliveryPage, error)
	ListWebhookAttempts(context.Context, string, string, string, string) (WebhookAttemptPage, error)
	ReplayWebhookDelivery(context.Context, string, string, string, string) error
}

func webhookPageQuery(raw string) (string, int, error) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return "", 0, ErrInvalidInput
	}
	for key, values := range query {
		if (key != "limit" && key != "cursor") || len(values) != 1 || values[0] == "" {
			return "", 0, ErrInvalidInput
		}
	}
	limit := 50
	if value := query.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			return "", 0, ErrInvalidInput
		}
	}
	cursor := query.Get("cursor")
	if len(cursor) > 512 {
		return "", 0, ErrInvalidInput
	}
	return cursor, limit, nil
}

func (h *Handler) webhookDeliveries(w http.ResponseWriter, r *http.Request, actor, appID string) {
	if r.Method != http.MethodGet {
		method(w, "GET")
		return
	}
	if h.WebhookHistory == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	cursor, limit, err := webhookPageQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, http.StatusBadRequest)
		return
	}
	page, err := h.WebhookHistory.ListWebhookDeliveries(r.Context(), actor, appID, cursor, limit)
	if err != nil {
		storeError(w, err)
		return
	}
	if page.Deliveries == nil {
		page.Deliveries = []WebhookDelivery{}
	}
	respond(w, http.StatusOK, page)
}

func (h *Handler) webhookDeliveryAction(w http.ResponseWriter, r *http.Request, actor, appID, eventID, subscriptionID, action string) {
	if h.WebhookHistory == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	if r.URL.RawQuery != "" || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		fail(w, http.StatusBadRequest)
		return
	}
	switch action {
	case "attempts":
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		page, err := h.WebhookHistory.ListWebhookAttempts(r.Context(), actor, appID, eventID, subscriptionID)
		if err != nil {
			storeError(w, err)
			return
		}
		if page.Attempts == nil {
			page.Attempts = []WebhookAttempt{}
		}
		respond(w, http.StatusOK, page)
	case "replay":
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		if err := h.WebhookHistory.ReplayWebhookDelivery(r.Context(), actor, appID, eventID, subscriptionID); err != nil {
			storeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}
