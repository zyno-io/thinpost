package admin

import (
	"context"
	"net/http"
)

// A zero pair disables uploads. The burst counts messages, not recipients.
type SubmissionRate struct {
	PerMinute int `json:"per_minute"`
	Burst     int `json:"burst"`
}

type SubmissionRateStore interface {
	GetSubmissionRate(context.Context, string) (SubmissionRate, error)
	SetSubmissionRate(context.Context, string, string, int, int) (SubmissionRate, error)
}

func (h *Handler) submissionRate(w http.ResponseWriter, r *http.Request, actor, appID string) {
	store, ok := h.Store.(SubmissionRateStore)
	if !ok {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		config, err := store.GetSubmissionRate(r.Context(), appID)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, config)
	case http.MethodPut:
		var request struct {
			PerMinute *int `json:"per_minute"`
			Burst     *int `json:"burst"`
		}
		if decode(r, &request) != nil || request.PerMinute == nil || request.Burst == nil ||
			!((*request.PerMinute == 0 && *request.Burst == 0) || (*request.PerMinute >= 1 && *request.PerMinute <= 60000 && *request.Burst >= 1 && *request.Burst <= 10000)) {
			fail(w, http.StatusBadRequest)
			return
		}
		config, err := store.SetSubmissionRate(r.Context(), actor, appID, *request.PerMinute, *request.Burst)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, config)
	default:
		method(w, "GET, PUT")
	}
}
