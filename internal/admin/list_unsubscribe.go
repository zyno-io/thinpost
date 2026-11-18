package admin

import (
	"context"
	"net/http"

	"github.com/zyno-io/thinpost/internal/tracking"
)

// ListUnsubscribe is an app's Thinpost-managed RFC 8058 one-click unsubscribe
// opt-in. It is off by default.
type ListUnsubscribe struct {
	Enabled bool `json:"enabled"`
}

type ListUnsubscribeStore interface {
	GetListUnsubscribe(context.Context, string) (ListUnsubscribe, error)
	// SetListUnsubscribe is idempotent: an identical value changes and audits
	// nothing.
	SetListUnsubscribe(ctx context.Context, actor, appID string, enabled bool) (ListUnsubscribe, error)
}

func (h *Handler) listUnsubscribe(w http.ResponseWriter, r *http.Request, actor, appID string) {
	store, ok := h.Store.(ListUnsubscribeStore)
	if !ok {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		config, err := store.GetListUnsubscribe(r.Context(), appID)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, config)
	case http.MethodPut:
		var request struct {
			Enabled *bool `json:"enabled"`
		}
		if decode(r, &request) != nil || request.Enabled == nil {
			fail(w, http.StatusBadRequest)
			return
		}
		// Enabling needs the tracking keys and origin on the serving replica.
		// Disabling is always allowed.
		if *request.Enabled && (h.Runtime == nil || !h.Runtime.Features.Tracking || h.Runtime.Tracking == nil) {
			fail(w, http.StatusConflict)
			return
		}
		// The unsubscribe link must be a valid, ASCII List-Unsubscribe value on
		// the tracking origin, or every message would fail preparation.
		if *request.Enabled && tracking.ValidateUnsubscribeOrigin(h.Runtime.Tracking.Origin) != nil {
			respond(w, http.StatusConflict, map[string]string{"error": "The tracking origin is not ASCII, so it cannot carry a one-click unsubscribe link."})
			return
		}
		config, err := store.SetListUnsubscribe(r.Context(), actor, appID, *request.Enabled)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, config)
	default:
		method(w, "GET, PUT")
	}
}
