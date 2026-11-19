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

// postmarkBatch deliberately supports a bounded subset: at most 100 messages
// and 2 MiB total. Every successful result follows its own durable commit.
// A disconnected caller can observe a partial batch; Postmark does not define
// an idempotency key for this endpoint, so retries may create duplicates.
func (s *Server) postmarkBatch(w http.ResponseWriter, r *http.Request, app submission.App, hold bool) {
	defer r.Body.Close()
	flag, err := protectionFlag(r.Header)
	if err != nil {
		fail(w, "postmark", http.StatusBadRequest, "invalid X-Thinpost-Enhanced-Protection header")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(data) > maxBody || !utf8.Valid(data) {
		fail(w, "postmark", http.StatusBadRequest, "invalid batch")
		return
	}
	var entries []json.RawMessage
	if err = json.Unmarshal(data, &entries); err != nil || len(entries) == 0 || len(entries) > 100 {
		fail(w, "postmark", http.StatusBadRequest, "invalid batch")
		return
	}
	results := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		item := r.Clone(r.Context())
		item.Body = io.NopCloser(bytes.NewReader(entry))
		message, to, err := decodePostmark(item, s.batchLimits())
		if err != nil {
			results = append(results, map[string]any{"ErrorCode": 300, "Message": "invalid or unsupported message"})
			continue
		}
		message.EnhancedProtection = flag
		if !s.admits(message) || message.Tracking != nil && !s.tracking {
			results = append(results, map[string]any{"ErrorCode": 300, "Message": "invalid or unsupported message"})
			continue
		}
		receipt, err := s.service.Accept(r.Context(), app, submission.Request{Message: message, Adapter: "postmark", RejectAllSuppressed: true, Hold: hold})
		if err != nil {
			code := 100
			message := "submission unavailable"
			if errors.Is(err, submission.ErrAllRecipientsSuppressed) {
				code = 406
				message = postmarkInactiveMessage
			}
			if errors.Is(err, submission.ErrInvalidMessage) {
				code = 300
				message = "invalid or unauthorized message"
			}
			if errors.Is(err, submission.ErrInvalidKey) {
				code = 10
				message = "invalid API key"
			}
			if errors.Is(err, submission.ErrBacklogExceeded) {
				message = "pending message limit exceeded"
			}
			if errors.Is(err, submission.ErrRateLimited) {
				message = "submission rate exceeded"
			}
			results = append(results, map[string]any{"ErrorCode": code, "Message": message})
			continue
		}
		if receipt.ID == "" || receipt.AcceptedAt.IsZero() {
			results = append(results, map[string]any{"ErrorCode": 100, "Message": "submission unavailable"})
			continue
		}
		results = append(results, map[string]any{"To": to, "SubmittedAt": receipt.AcceptedAt.UTC().Format("2006-01-02T15:04:05.0000000Z"), "MessageID": providerid.Postmark(receipt.ID), "ErrorCode": 0, "Message": "OK"})
	}
	// Every item that was accepted is held; failed items carry their own error.
	markHeld(w, hold)
	writeJSON(w, http.StatusOK, results)
}
