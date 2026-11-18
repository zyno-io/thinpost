package httpapi

import (
	"errors"
	"net/http"

	"github.com/zyno-io/thinpost/internal/submission"
)

// sendGridBatch never falls back to individual acceptance calls: partial
// success would make a provider retry silently duplicate earlier messages.
func (s *Server) sendGridBatch(w http.ResponseWriter, r *http.Request, app submission.App, messages []submission.Message) {
	flag, err := protectionFlag(r.Header)
	if err != nil {
		fail(w, "sendgrid", http.StatusBadRequest, "invalid X-Thinpost-Enhanced-Protection header")
		return
	}
	total := 0
	for i := range messages {
		messages[i].EnhancedProtection = flag
		message := messages[i]
		total += len(message.Envelope())
		if total > 1000 || !s.admits(message) || message.Tracking != nil && !s.tracking {
			fail(w, "sendgrid", http.StatusBadRequest, "invalid or unsupported message")
			return
		}
	}
	service, ok := s.service.(submission.BatchService)
	if !ok {
		fail(w, "sendgrid", http.StatusServiceUnavailable, "batch submission unavailable")
		return
	}
	receipt, err := service.AcceptBatch(r.Context(), app, submission.BatchRequest{Messages: messages, Adapter: "sendgrid"})
	if err != nil {
		switch {
		case errors.Is(err, submission.ErrInvalidKey):
			fail(w, "sendgrid", http.StatusUnauthorized, "invalid API key")
		case errors.Is(err, submission.ErrInvalidMessage):
			fail(w, "sendgrid", http.StatusBadRequest, "invalid or unauthorized message")
		case errors.Is(err, submission.ErrQuotaExceeded), errors.Is(err, submission.ErrBacklogExceeded), errors.Is(err, submission.ErrRateLimited):
			fail(w, "sendgrid", http.StatusTooManyRequests, "batch quota exceeded")
		default:
			fail(w, "sendgrid", http.StatusServiceUnavailable, "batch submission unavailable")
		}
		return
	}
	if len(receipt.IDs) != len(messages) {
		fail(w, "sendgrid", http.StatusServiceUnavailable, "batch submission unavailable")
		return
	}
	seen := make(map[string]bool, len(receipt.IDs))
	for _, id := range receipt.IDs {
		if id == "" || seen[id] {
			fail(w, "sendgrid", http.StatusServiceUnavailable, "batch submission unavailable")
			return
		}
		seen[id] = true
	}
	w.WriteHeader(http.StatusAccepted)
}
