package admin

import (
	"context"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Suppression reasons and origins. Only manual entries made in the console are
// created by the admin API.
const (
	SuppressionReasonHardBounce    = "hard_bounce"
	SuppressionReasonSpamComplaint = "spam_complaint"
	SuppressionReasonUnsubscribe   = "unsubscribe"
	SuppressionReasonManual        = "manual"

	SuppressionOriginRecipient = "recipient"
	SuppressionOriginAPI       = "api"
	SuppressionOriginConsole   = "console"
)

// Suppression field values for a search.
const (
	SuppressionFieldAddress = "address"
	SuppressionFieldDomain  = "domain"
)

const (
	maxSuppressionAddressBytes = 998
	maxSuppressionBatch        = 100
)

// Suppression is one entry of an app's suppression list. It carries a
// recipient address: it must never reach a URL, log line, audit row or error.
type Suppression struct {
	ID              string    `json:"id"`
	Address         string    `json:"address"`
	Reason          string    `json:"reason"`
	Origin          string    `json:"origin"`
	CreatedAt       time.Time `json:"created_at"`
	SourceMessageID string    `json:"source_message_id,omitempty"`
}

type SuppressionPage struct {
	Suppressions []Suppression `json:"suppressions"`
	NextCursor   string        `json:"next_cursor,omitempty"`
}

// SuppressionQuery lists or searches. Q is secret operator input.
type SuppressionQuery struct {
	Q      string
	Field  string
	Reason string
	Cursor string
	Limit  int
}

// SuppressionResult reports one added address: "added", or "exists" when the
// address was already suppressed (the first reason wins).
type SuppressionResult struct {
	Address     string      `json:"address"`
	Status      string      `json:"status"`
	Suppression Suppression `json:"suppression"`
}

// SuppressionStore keeps the per-app suppression list. Addresses are accepted
// already normalized by NormalizeSuppressionAddress.
type SuppressionStore interface {
	ListSuppressions(ctx context.Context, appID string, query SuppressionQuery) (SuppressionPage, error)
	// AddSuppressions adds manual console entries in one transaction.
	AddSuppressions(ctx context.Context, actor, appID string, addresses []string) ([]SuppressionResult, error)
	// DeleteSuppressions removes entries by ID; unknown IDs and IDs of other
	// apps are ignored. It returns the number removed.
	DeleteSuppressions(ctx context.Context, actor, appID string, ids []string) (int, error)
}

func ValidSuppressionReason(reason string) bool {
	switch reason {
	case SuppressionReasonHardBounce, SuppressionReasonSpamComplaint, SuppressionReasonUnsubscribe, SuppressionReasonManual:
		return true
	}
	return false
}

// NormalizeSuppressionAddress parses one bare mailbox and returns the address
// as it is stored, with a quoted local part unquoted as for recipients. A
// display name, a list, a comment or a missing domain is rejected.
func NormalizeSuppressionAddress(value string) (string, bool) {
	if len(value) < 3 || len(value) > maxSuppressionAddressBytes+2 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return "", false
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" {
		return "", false
	}
	address := parsed.Address
	at := strings.LastIndexByte(address, '@')
	if len(address) < 3 || len(address) > maxSuppressionAddressBytes || at < 1 || at == len(address)-1 || !utf8.ValidString(address) || strings.IndexByte(address, 0) >= 0 {
		return "", false
	}
	// The store validates the lowercased form (address_lower has the same
	// bound), which can be longer: U+023A is 2 bytes and lowercases to 3.
	if len(strings.ToLower(address)) > maxSuppressionAddressBytes {
		return "", false
	}
	return address, true
}

func (h *Handler) suppressions(w http.ResponseWriter, r *http.Request, actor, appID string) {
	if h.Suppressions == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		query, ok := suppressionListQuery(r)
		if !ok {
			fail(w, http.StatusBadRequest)
			return
		}
		h.suppressionPage(w, r, appID, query)
	case http.MethodPost:
		// Addresses are accepted only in the body, never in the query string.
		if r.URL.RawQuery != "" {
			fail(w, http.StatusBadRequest)
			return
		}
		var req struct {
			Addresses []string `json:"addresses"`
		}
		// 100 addresses of up to 998 bytes, with JSON escaping headroom.
		if decodeLimit(r, &req, 128<<10) != nil || len(req.Addresses) < 1 || len(req.Addresses) > maxSuppressionBatch {
			fail(w, http.StatusBadRequest)
			return
		}
		normalized := make([]string, len(req.Addresses))
		for i, entry := range req.Addresses {
			address, ok := NormalizeSuppressionAddress(entry)
			if !ok {
				fail(w, http.StatusBadRequest)
				return
			}
			normalized[i] = address
		}
		results, err := h.Suppressions.AddSuppressions(r.Context(), actor, appID, normalized)
		if err != nil {
			storeError(w, err)
			return
		}
		if results == nil {
			results = []SuppressionResult{}
		}
		respond(w, http.StatusOK, map[string]any{"results": results})
	default:
		method(w, "GET, POST")
	}
}

func (h *Handler) suppressionSearch(w http.ResponseWriter, r *http.Request, appID string) {
	if h.Suppressions == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		method(w, "POST")
		return
	}
	if r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest)
		return
	}
	var req struct {
		Q      string `json:"q"`
		Field  string `json:"field"`
		Reason string `json:"reason"`
		Cursor string `json:"cursor"`
		Limit  *int   `json:"limit"`
	}
	if decode(r, &req) != nil {
		fail(w, http.StatusBadRequest)
		return
	}
	if req.Field == "" {
		req.Field = SuppressionFieldAddress
	}
	limit := 50
	if req.Limit != nil {
		limit = *req.Limit
	}
	if !ValidSearchText(req.Q) || (req.Field != SuppressionFieldAddress && req.Field != SuppressionFieldDomain) ||
		(req.Reason != "" && !ValidSuppressionReason(req.Reason)) || limit < 1 || limit > 100 || len(req.Cursor) > 512 {
		fail(w, http.StatusBadRequest)
		return
	}
	h.suppressionPage(w, r, appID, SuppressionQuery{Q: req.Q, Field: req.Field, Reason: req.Reason, Cursor: req.Cursor, Limit: limit})
}

func (h *Handler) suppressionPage(w http.ResponseWriter, r *http.Request, appID string, query SuppressionQuery) {
	page, err := h.Suppressions.ListSuppressions(r.Context(), appID, query)
	if err != nil {
		storeError(w, err)
		return
	}
	if page.Suppressions == nil {
		page.Suppressions = []Suppression{}
	}
	respond(w, http.StatusOK, page)
}

func (h *Handler) suppressionDelete(w http.ResponseWriter, r *http.Request, actor, appID string) {
	if h.Suppressions == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		method(w, "POST")
		return
	}
	if r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest)
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if decode(r, &req) != nil || len(req.IDs) < 1 || len(req.IDs) > maxSuppressionBatch {
		fail(w, http.StatusBadRequest)
		return
	}
	for _, id := range req.IDs {
		if !opaqueID(id) {
			fail(w, http.StatusBadRequest)
			return
		}
	}
	deleted, err := h.Suppressions.DeleteSuppressions(r.Context(), actor, appID, req.IDs)
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]int{"deleted": deleted})
}

// suppressionListQuery accepts only reason, limit and cursor.
func suppressionListQuery(r *http.Request) (SuppressionQuery, bool) {
	query := SuppressionQuery{Limit: 50}
	if len(r.URL.RawQuery) > 1024 {
		return query, false
	}
	values := r.URL.Query()
	for key, list := range values {
		if len(list) != 1 {
			return query, false
		}
		switch key {
		case "reason":
			query.Reason = list[0]
			if !ValidSuppressionReason(query.Reason) {
				return query, false
			}
		case "limit":
			n, err := strconv.Atoi(list[0])
			if err != nil || n < 1 || n > 100 || strconv.Itoa(n) != list[0] {
				return query, false
			}
			query.Limit = n
		case "cursor":
			query.Cursor = list[0]
			if len(query.Cursor) > 512 {
				return query, false
			}
		default:
			return query, false
		}
	}
	return query, true
}
