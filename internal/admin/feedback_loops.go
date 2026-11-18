package admin

import (
	"context"
	"net/http"
	"time"
)

// FeedbackMailbox is the global spam-complaint feedback mailbox. The token is
// shown in the console and is not a credential: a report is trusted only through
// its reporter DKIM signature and its correlation to a delivery.
type FeedbackMailbox struct {
	Token     string
	CreatedAt time.Time
}

// FeedbackCount is one day's count of a result of inbound feedback.
type FeedbackCount struct {
	Day     string `json:"day"`
	Channel string `json:"channel"`
	Result  string `json:"result"`
	Count   int    `json:"count"`
}

// FeedbackLoops is the store's view: the mailbox (if created), the reporter
// DKIM domain allowlist and the last 14 days of counters.
type FeedbackLoops struct {
	Mailbox   *FeedbackMailbox
	Reporters []string
	Counts    []FeedbackCount
}

type FeedbackLoopStore interface {
	// FeedbackLoops reads a snapshot without the runtime gate.
	FeedbackLoops(ctx context.Context) (FeedbackLoops, error)
	// CreateFeedbackMailbox returns ErrConflict when a mailbox exists and rotate
	// is false.
	CreateFeedbackMailbox(ctx context.Context, actor string, rotate bool) (FeedbackMailbox, error)
	SetFeedbackReporters(ctx context.Context, actor string, domains []string) ([]string, error)
}

type feedbackMailboxResponse struct {
	Address   string    `json:"address,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type feedbackLoopsResponse struct {
	// Receiving is true when this replica knows the bounce domain, so it can
	// show the mailbox address.
	Receiving bool                     `json:"receiving"`
	Mailbox   *feedbackMailboxResponse `json:"mailbox,omitempty"`
	Reporters []string                 `json:"reporters"`
	Counts    []FeedbackCount          `json:"counts"`
}

// feedbackAddress forms b+<token>@<bounce domain>; empty without a domain.
func (h *Handler) feedbackAddress(token string) string {
	if h.BounceDomain == "" || token == "" {
		return ""
	}
	return "b+" + token + "@" + h.BounceDomain
}

// feedbackRoute serves /api/v1/feedback-loops and its sub-resources. It
// reports false for any other path.
func (h *Handler) feedbackRoute(w http.ResponseWriter, r *http.Request, actor string) bool {
	switch r.URL.Path {
	case "/api/v1/feedback-loops", "/api/v1/feedback-loops/mailbox", "/api/v1/feedback-loops/reporters":
	default:
		return false
	}
	store, ok := h.Store.(FeedbackLoopStore)
	if !ok || r.URL.RawQuery != "" {
		fail(w, http.StatusServiceUnavailable)
		return true
	}
	switch r.URL.Path {
	case "/api/v1/feedback-loops":
		if r.Method != http.MethodGet {
			method(w, "GET")
			return true
		}
		loops, err := store.FeedbackLoops(r.Context())
		if err != nil {
			storeError(w, err)
			return true
		}
		h.respondFeedbackLoops(w, loops)
	case "/api/v1/feedback-loops/mailbox":
		if r.Method != http.MethodPost {
			method(w, "POST")
			return true
		}
		var request struct {
			Rotate bool `json:"rotate"`
		}
		if decode(r, &request) != nil {
			fail(w, http.StatusBadRequest)
			return true
		}
		// Without the bounce domain the address cannot be formed, so do not
		// create a mailbox this replica cannot show.
		if h.BounceDomain == "" {
			fail(w, http.StatusConflict)
			return true
		}
		mailbox, err := store.CreateFeedbackMailbox(r.Context(), actor, request.Rotate)
		if err != nil {
			storeError(w, err)
			return true
		}
		respond(w, http.StatusOK, map[string]string{"address": h.feedbackAddress(mailbox.Token)})
	default:
		if r.Method != http.MethodPut {
			method(w, "PUT")
			return true
		}
		var request struct {
			Domains *[]string `json:"domains"`
		}
		if decode(r, &request) != nil || request.Domains == nil || len(*request.Domains) > 32 {
			fail(w, http.StatusBadRequest)
			return true
		}
		domains := make([]string, 0, len(*request.Domains))
		seen := map[string]bool{}
		for _, value := range *request.Domains {
			domain, ok := NormalizeDomain(value)
			if !ok || len(domain) < 3 {
				fail(w, http.StatusBadRequest)
				return true
			}
			if !seen[domain] {
				seen[domain] = true
				domains = append(domains, domain)
			}
		}
		saved, err := store.SetFeedbackReporters(r.Context(), actor, domains)
		if err != nil {
			storeError(w, err)
			return true
		}
		respond(w, http.StatusOK, map[string][]string{"domains": saved})
	}
	return true
}

func (h *Handler) respondFeedbackLoops(w http.ResponseWriter, loops FeedbackLoops) {
	out := feedbackLoopsResponse{Receiving: h.BounceDomain != "", Reporters: loops.Reporters, Counts: loops.Counts}
	if out.Reporters == nil {
		out.Reporters = []string{}
	}
	if out.Counts == nil {
		out.Counts = []FeedbackCount{}
	}
	if loops.Mailbox != nil {
		out.Mailbox = &feedbackMailboxResponse{Address: h.feedbackAddress(loops.Mailbox.Token), CreatedAt: loops.Mailbox.CreatedAt}
	}
	respond(w, http.StatusOK, out)
}
