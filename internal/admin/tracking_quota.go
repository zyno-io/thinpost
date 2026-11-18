package admin

import (
	"context"
	"net/http"
)

// TrackingQuota is an aggregate UTC-day counter, with no request metadata.
// Suppressed saturates at one billion. A zero limit records no events.
type TrackingQuota struct {
	DailyLimit int    `json:"daily_limit"`
	Used       int    `json:"used"`
	Suppressed int    `json:"suppressed"`
	Day        string `json:"day"`
}

type TrackingQuotaStore interface {
	GetTrackingQuota(context.Context, string) (TrackingQuota, error)
	SetTrackingQuota(context.Context, string, string, int) (TrackingQuota, error)
}

func (h *Handler) trackingQuota(w http.ResponseWriter, r *http.Request, actor, appID string) {
	store, ok := h.Store.(TrackingQuotaStore)
	if !ok {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		quota, err := store.GetTrackingQuota(r.Context(), appID)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, quota)
	case http.MethodPut:
		var req struct {
			DailyLimit *int `json:"daily_limit"`
		}
		if decode(r, &req) != nil || req.DailyLimit == nil || *req.DailyLimit < 0 || *req.DailyLimit > 1000000 {
			fail(w, http.StatusBadRequest)
			return
		}
		quota, err := store.SetTrackingQuota(r.Context(), actor, appID, *req.DailyLimit)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, quota)
	default:
		method(w, "GET, PUT")
	}
}
