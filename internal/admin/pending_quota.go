package admin

import (
	"context"
	"net/http"
)

// Pending is a snapshot. Truncated indicates at least Pending messages exist.
type PendingQuota struct {
	Limit     int  `json:"limit"`
	Pending   int  `json:"pending"`
	Truncated bool `json:"truncated"`
}

type PendingQuotaStore interface {
	GetPendingQuota(context.Context, string) (PendingQuota, error)
	SetPendingQuota(context.Context, string, string, int) (PendingQuota, error)
}

func (h *Handler) pendingQuota(w http.ResponseWriter, r *http.Request, actor, appID string) {
	store, ok := h.Store.(PendingQuotaStore)
	if !ok {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		quota, err := store.GetPendingQuota(r.Context(), appID)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, quota)
	case http.MethodPut:
		var request struct {
			Limit *int `json:"limit"`
		}
		if decode(r, &request) != nil || request.Limit == nil || *request.Limit < 0 || *request.Limit > 100000 {
			fail(w, http.StatusBadRequest)
			return
		}
		quota, err := store.SetPendingQuota(r.Context(), actor, appID, *request.Limit)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, quota)
	default:
		method(w, "GET, PUT")
	}
}
