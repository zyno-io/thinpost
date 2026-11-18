package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/submission"
)

// resendBatch validates the complete request before invoking the atomic
// batch service. It never falls back to individual message submissions.
func (s *Server) resendBatch(w http.ResponseWriter, r *http.Request, app submission.App) {
	defer r.Body.Close()
	service, ok := s.service.(submission.BatchService)
	if !ok {
		fail(w, "resend", http.StatusServiceUnavailable, "batch submission unavailable")
		return
	}
	key, err := resendIdempotencyKey(r)
	if err != nil {
		fail(w, "resend", http.StatusBadRequest, "invalid idempotency key")
		return
	}
	flag, err := protectionFlag(r.Header)
	if err != nil {
		fail(w, "resend", http.StatusBadRequest, "invalid X-Thinpost-Enhanced-Protection header")
		return
	}
	opens, clicks, err := resendTrackingHeaders(r)
	if err != nil || (opens != nil && *opens || clicks != nil && *clicks) && !s.tracking {
		fail(w, "resend", http.StatusBadRequest, "invalid or unsupported tracking")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(data) > maxBody || !utf8.Valid(data) {
		fail(w, "resend", http.StatusBadRequest, "invalid batch")
		return
	}
	var entries []json.RawMessage
	if err = json.Unmarshal(data, &entries); err != nil || len(entries) == 0 || len(entries) > 100 {
		fail(w, "resend", http.StatusBadRequest, "invalid batch")
		return
	}
	messages := make([]submission.Message, 0, len(entries))
	for _, entry := range entries {
		item := r.Clone(r.Context())
		item.Body = io.NopCloser(bytes.NewReader(entry))
		message, err := decodeResend(item, s.batchLimits())
		if err != nil {
			fail(w, "resend", http.StatusBadRequest, "invalid or unsupported batch message")
			return
		}
		message.EnhancedProtection = message.EnhancedProtection || flag
		// RequestTracking allocates per item, so items never share options.
		message.RequestTracking(opens, clicks)
		if !s.admits(message) || len(message.Envelope()) > 50 {
			fail(w, "resend", http.StatusBadRequest, "invalid or unsupported batch message")
			return
		}
		messages = append(messages, message)
	}
	receipt, err := service.AcceptBatch(r.Context(), app, submission.BatchRequest{Messages: messages, IdempotencyKey: key, Adapter: "resend"})
	if err != nil {
		switch {
		case errors.Is(err, submission.ErrInvalidKey):
			fail(w, "resend", http.StatusUnauthorized, "invalid API key")
		case errors.Is(err, submission.ErrInvalidMessage):
			fail(w, "resend", http.StatusBadRequest, "invalid or unauthorized batch")
		case errors.Is(err, submission.ErrIdempotencyConflict):
			fail(w, "resend", http.StatusConflict, "idempotency conflict")
		case errors.Is(err, submission.ErrQuotaExceeded), errors.Is(err, submission.ErrBacklogExceeded), errors.Is(err, submission.ErrRateLimited):
			fail(w, "resend", http.StatusTooManyRequests, "batch quota exceeded")
		default:
			fail(w, "resend", http.StatusServiceUnavailable, "batch submission unavailable")
		}
		return
	}
	if len(receipt.IDs) != len(messages) {
		fail(w, "resend", http.StatusServiceUnavailable, "batch submission unavailable")
		return
	}
	results := make([]map[string]string, 0, len(receipt.IDs))
	seen := make(map[string]bool, len(receipt.IDs))
	for _, id := range receipt.IDs {
		if id == "" || seen[id] {
			fail(w, "resend", http.StatusServiceUnavailable, "batch submission unavailable")
			return
		}
		seen[id] = true
		results = append(results, map[string]string{"id": providerid.Resend(id)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": results})
}
