package admin

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/zyno-io/thinpost/internal/submission"
)

// maxTestMessageBody is the largest accepted test-send request body.
const maxTestMessageBody = 64 << 10

// TestMessageSubmitter submits an operator's test message as an app through
// the same acceptance path as the app API. actor is the authenticated
// administrator subject; body is the raw native request body, which the
// submitter decodes and constrains. Implementations return submission errors
// that SubmissionFailure maps.
type TestMessageSubmitter interface {
	SubmitTestMessage(ctx context.Context, actor, appID string, body []byte) (submission.Receipt, error)
}

// SubmittedMessage is the response to an accepted submission.
type SubmittedMessage struct {
	ID         string    `json:"id"`
	AcceptedAt time.Time `json:"accepted_at"`
}

// testMessage handles POST /api/v1/apps/{appId}/messages/test. The message
// content stays in the body, out of access logs, like search terms.
func (h *Handler) testMessage(w http.ResponseWriter, r *http.Request, actor, appID string) {
	if r.Method != http.MethodPost {
		method(w, "POST")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		fail(w, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTestMessageBody+1))
	if err != nil || len(body) > maxTestMessageBody {
		fail(w, http.StatusBadRequest)
		return
	}
	if h.TestMessages == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	receipt, err := h.TestMessages.SubmitTestMessage(r.Context(), actor, appID, body)
	if err != nil {
		status, code := SubmissionFailure(err)
		respond(w, status, map[string]string{"error": code})
		return
	}
	if receipt.ID == "" || receipt.AcceptedAt.IsZero() {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Location", "/api/v1/apps/"+appID+"/messages/"+receipt.ID)
	respond(w, http.StatusCreated, SubmittedMessage{ID: receipt.ID, AcceptedAt: receipt.AcceptedAt.UTC()})
}
