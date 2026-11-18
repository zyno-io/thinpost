package admin

import (
	"context"
	"net/http"
)

// RecipientQuota counts accepted envelope recipients within a UTC day.
type RecipientQuota struct {
	DailyLimit int    `json:"daily_limit"`
	Used       int    `json:"used"`
	Day        string `json:"day"`
}

type QuotaStore interface {
	GetRecipientQuota(context.Context, string) (RecipientQuota, error)
	SetRecipientQuota(context.Context, string, string, int) (RecipientQuota, error)
}

func (h *Handler) recipientQuota(w http.ResponseWriter, r *http.Request, actor, appID string) {
	store, ok := h.Store.(QuotaStore)
	if !ok {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		quota, err := store.GetRecipientQuota(r.Context(), appID)
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
		quota, err := store.SetRecipientQuota(r.Context(), actor, appID, *req.DailyLimit)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, quota)
	default:
		method(w, "GET, PUT")
	}
}
