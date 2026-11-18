package admin

import (
	"context"
	"net/http"
)

// TrackingDefaults are an app's open and click tracking defaults. Both are off
// by default. Acceptance applies a default only to an HTML message whose
// request left that kind unset.
type TrackingDefaults struct {
	Opens  bool `json:"opens"`
	Clicks bool `json:"clicks"`
}

type TrackingDefaultsStore interface {
	GetTrackingDefaults(ctx context.Context, appID string) (TrackingDefaults, error)
	// SetTrackingDefaults is idempotent: an identical value changes and audits
	// nothing. When canEnable is false, turning a default on (relative to the
	// stored value, read under the row lock) returns ErrConflict; turning one
	// off, or resending an unchanged value, is always allowed.
	SetTrackingDefaults(ctx context.Context, actor, appID string, v TrackingDefaults, canEnable bool) (TrackingDefaults, error)
}

func (h *Handler) trackingDefaults(w http.ResponseWriter, r *http.Request, actor, appID string) {
	store, ok := h.Store.(TrackingDefaultsStore)
	if !ok {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		defaults, err := store.GetTrackingDefaults(r.Context(), appID)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, defaults)
	case http.MethodPut:
		var request struct {
			Opens  *bool `json:"opens"`
			Clicks *bool `json:"clicks"`
		}
		if decode(r, &request) != nil || request.Opens == nil || request.Clicks == nil {
			fail(w, http.StatusBadRequest)
			return
		}
		// A check for the operator, not the safety guarantee: acceptance
		// ignores defaults whenever its own runtime lacks tracking.
		canEnable := h.Runtime != nil && h.Runtime.Features.Tracking && h.Runtime.Tracking != nil
		defaults, err := store.SetTrackingDefaults(r.Context(), actor, appID, TrackingDefaults{Opens: *request.Opens, Clicks: *request.Clicks}, canEnable)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, defaults)
	default:
		method(w, "GET, PUT")
	}
}
