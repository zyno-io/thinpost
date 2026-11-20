package admin

import (
	"errors"
	"net/http"

	"github.com/zyno-io/thinpost/internal/submission"
)

// SubmissionFailure maps a submission error to the HTTP status and error code
// shared by the native send route and the operator test-send route. Codes for
// refusals the caller can act on are stable snake_case identifiers; the rest
// are the status text. Nothing here depends on message content.
func SubmissionFailure(err error) (status int, code string) {
	switch {
	case errors.Is(err, submission.ErrInvalidRequest):
		return http.StatusBadRequest, http.StatusText(http.StatusBadRequest)
	case errors.Is(err, submission.ErrInvalidKey):
		return http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized)
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, http.StatusText(http.StatusNotFound)
	case errors.Is(err, submission.ErrAppPaused):
		return http.StatusConflict, "app_paused"
	case errors.Is(err, submission.ErrIdempotencyConflict):
		return http.StatusConflict, "idempotency_conflict"
	case errors.Is(err, submission.ErrInvalidMessage):
		return http.StatusUnprocessableEntity, "message_rejected"
	case errors.Is(err, submission.ErrQuotaExceeded):
		return http.StatusTooManyRequests, "recipient_quota_exceeded"
	case errors.Is(err, submission.ErrRateLimited):
		return http.StatusTooManyRequests, "submission_rate_exceeded"
	case errors.Is(err, submission.ErrBacklogExceeded):
		return http.StatusTooManyRequests, "pending_limit_exceeded"
	default:
		return http.StatusServiceUnavailable, http.StatusText(http.StatusServiceUnavailable)
	}
}
