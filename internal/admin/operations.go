package admin

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// RuntimeFeatures reports which submission features the serving replica was
// configured with. Another replica could differ during a rollout.
type RuntimeFeatures struct {
	Submission bool `json:"submission"`
	Tracking   bool `json:"tracking"`
	// RecipientPolicy is "allowlist" when this deployment restricts recipients
	// to an allowlist and "unrestricted" when it does not. It is empty when the
	// replica does not enforce a recipient policy (no control bridge), in which
	// case the field is omitted.
	RecipientPolicy string `json:"recipient_policy,omitempty"`
}

// RuntimeTracking is the tracking deployment configuration, present only when
// tracking is enabled. LinkEventLimit is the per-link recorded event cap.
type RuntimeTracking struct {
	Origin         string `json:"origin"`
	LinkEventLimit int    `json:"link_event_limit"`
}

// RuntimeInfo is set once at startup from configuration, never from a request.
type RuntimeInfo struct {
	Features RuntimeFeatures  `json:"features"`
	Tracking *RuntimeTracking `json:"tracking,omitempty"`
}

// OperationsGate is the deployment gate. The stored generation is never
// returned; GenerationMatches compares it with this replica's.
type OperationsGate struct {
	Mode               string    `json:"mode"`
	Revision           int64     `json:"revision"`
	ChangedAt          time.Time `json:"changed_at"`
	ChangedBy          *string   `json:"changed_by,omitempty"`
	GenerationMatches  bool      `json:"generation_matches"`
	QuarantineComplete bool      `json:"quarantine_complete"`
}

// OperationsWorker is one Edge worker lease. Live is the database's judgment of
// the lease expiry; it does not prove SMTP reachability.
type OperationsWorker struct {
	WorkerID     string    `json:"worker_id"`
	ControllerID string    `json:"controller_id"`
	Epoch        int64     `json:"epoch"`
	ExpiresAt    time.Time `json:"expires_at"`
	Live         bool      `json:"live"`
}

// MessageBacklog counts messages that are not finished preparing.
type MessageBacklog struct {
	QueuedDue         int `json:"queued_due"`
	QueuedWaiting     int `json:"queued_waiting"`
	Preparing         int `json:"preparing"`
	PreparationFailed int `json:"preparation_failed"`
	RecoveryHold      int `json:"recovery_hold"`
	// OnHold counts messages submitted on hold that wait for their app.
	OnHold      int        `json:"on_hold"`
	OldestDueAt *time.Time `json:"oldest_due_at,omitempty"`
}

type DeliveryBacklog struct {
	QueuedDue           int        `json:"queued_due"`
	QueuedWaiting       int        `json:"queued_waiting"`
	InFlight            int        `json:"in_flight"`
	Uncertain           int        `json:"uncertain"`
	RecipientPolicyHeld int        `json:"recipient_policy_held"`
	TrackingHeld        int        `json:"tracking_held"`
	RecoveryHold        int        `json:"recovery_hold"`
	OldestDueAt         *time.Time `json:"oldest_due_at,omitempty"`
}

type WebhookBacklog struct {
	QueuedDue     int        `json:"queued_due"`
	QueuedWaiting int        `json:"queued_waiting"`
	InFlight      int        `json:"in_flight"`
	RecoveryHold  int        `json:"recovery_hold"`
	OldestDueAt   *time.Time `json:"oldest_due_at,omitempty"`
}

// InboundOperations counts received mail for the current UTC day across all
// apps, from inbound_daily (the same counters and day the per-app daily cap
// uses). Correlated is a subset of Accepted. Counts only: no address or content.
type InboundOperations struct {
	Day              string `json:"day"`
	Accepted         int    `json:"accepted"`
	Correlated       int    `json:"correlated"`
	Duplicate        int    `json:"duplicate"`
	TempfailRouting  int    `json:"tempfail_routing"`
	TempfailLimit    int    `json:"tempfail_limit"`
	TempfailInternal int    `json:"tempfail_internal"`
}

// RetryPolicy is the delivery retry policy with its revision.
type RetryPolicy struct {
	MaxAttempts    int       `json:"max_attempts"`
	InitialDelayMS int64     `json:"initial_delay_ms"`
	MaxDelayMS     int64     `json:"max_delay_ms"`
	MaxAgeMS       int64     `json:"max_age_ms"`
	Revision       int64     `json:"revision"`
	ChangedAt      time.Time `json:"changed_at"`
	ChangedBy      string    `json:"changed_by,omitempty"`
}

// RetryPolicyUpdate replaces the policy if the stored revision still equals
// ExpectedRevision.
type RetryPolicyUpdate struct {
	MaxAttempts      int   `json:"max_attempts"`
	InitialDelayMS   int64 `json:"initial_delay_ms"`
	MaxDelayMS       int64 `json:"max_delay_ms"`
	MaxAgeMS         int64 `json:"max_age_ms"`
	ExpectedRevision int64 `json:"expected_revision"`
}

// Operations is a read-only snapshot. It carries counts and identifiers only:
// no address, subject or message content.
type Operations struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Gate        OperationsGate     `json:"gate"`
	Workers     []OperationsWorker `json:"workers"`
	Messages    MessageBacklog     `json:"messages"`
	Deliveries  DeliveryBacklog    `json:"deliveries"`
	Webhooks    WebhookBacklog     `json:"webhooks"`
	Inbound     InboundOperations  `json:"inbound"`
	RetryPolicy RetryPolicy        `json:"retry_policy"`
}

// Attention kinds, in the order the list documents them.
const (
	AttentionUncertain           = "uncertain"
	AttentionRecipientPolicyHeld = "recipient_policy_held"
	AttentionTrackingHeld        = "tracking_held"
	AttentionPreparationFailed   = "preparation_failed"
	AttentionRecoveryHold        = "recovery_hold"
)

// AttentionItem points at a message that needs an operator. Since is the time
// the message was accepted.
type AttentionItem struct {
	AppID      string    `json:"app_id"`
	AppName    string    `json:"app_name"`
	MessageID  string    `json:"message_id"`
	DeliveryID string    `json:"delivery_id,omitempty"`
	Kind       string    `json:"kind"`
	Since      time.Time `json:"since"`
}

type AttentionPage struct {
	Items      []AttentionItem `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// OperationsStore reads the operations snapshot without the runtime gate, so it
// works while the gate is closed. SetRetryPolicy needs the open gate.
type OperationsStore interface {
	Operations(ctx context.Context) (Operations, error)
	Attention(ctx context.Context, cursor string, limit int) (AttentionPage, error)
	SetRetryPolicy(ctx context.Context, actor string, update RetryPolicyUpdate) (RetryPolicy, error)
}

// operationsRoute serves the global routes. It reports false for any other path.
func (h *Handler) operationsRoute(w http.ResponseWriter, r *http.Request, actor string) bool {
	switch r.URL.Path {
	case "/api/v1/runtime":
		if r.Method != http.MethodGet {
			method(w, "GET")
		} else if h.Runtime == nil || r.URL.RawQuery != "" {
			fail(w, http.StatusServiceUnavailable)
		} else {
			respond(w, http.StatusOK, h.Runtime)
		}
	case "/api/v1/operations":
		h.operations(w, r)
	case "/api/v1/operations/attention":
		h.attention(w, r)
	case "/api/v1/retry-policy":
		h.retryPolicy(w, r, actor)
	default:
		return false
	}
	return true
}

func (h *Handler) operations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		method(w, "GET")
		return
	}
	if h.Operations == nil || r.URL.RawQuery != "" {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	snapshot, err := h.Operations.Operations(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	if snapshot.Workers == nil {
		snapshot.Workers = []OperationsWorker{}
	}
	respond(w, http.StatusOK, snapshot)
}

func (h *Handler) attention(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		method(w, "GET")
		return
	}
	if h.Operations == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	limit := 50
	cursor := ""
	if err != nil {
		fail(w, http.StatusBadRequest)
		return
	}
	for key, values := range query {
		if len(values) != 1 {
			fail(w, http.StatusBadRequest)
			return
		}
		switch key {
		case "cursor":
			cursor = values[0]
		case "limit":
			n, convErr := strconv.Atoi(values[0])
			if convErr != nil || n < 1 || n > 100 {
				fail(w, http.StatusBadRequest)
				return
			}
			limit = n
		default:
			fail(w, http.StatusBadRequest)
			return
		}
	}
	if len(cursor) > 512 {
		fail(w, http.StatusBadRequest)
		return
	}
	page, err := h.Operations.Attention(r.Context(), cursor, limit)
	if err != nil {
		storeError(w, err)
		return
	}
	if page.Items == nil {
		page.Items = []AttentionItem{}
	}
	respond(w, http.StatusOK, page)
}

func (h *Handler) retryPolicy(w http.ResponseWriter, r *http.Request, actor string) {
	if r.Method != http.MethodPut {
		method(w, "PUT")
		return
	}
	if h.Operations == nil || r.URL.RawQuery != "" {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	var req struct {
		MaxAttempts      *int   `json:"max_attempts"`
		InitialDelayMS   *int64 `json:"initial_delay_ms"`
		MaxDelayMS       *int64 `json:"max_delay_ms"`
		MaxAgeMS         *int64 `json:"max_age_ms"`
		ExpectedRevision *int64 `json:"expected_revision"`
	}
	if decode(r, &req) != nil || req.MaxAttempts == nil || req.InitialDelayMS == nil || req.MaxDelayMS == nil || req.MaxAgeMS == nil || req.ExpectedRevision == nil {
		fail(w, http.StatusBadRequest)
		return
	}
	update := RetryPolicyUpdate{MaxAttempts: *req.MaxAttempts, InitialDelayMS: *req.InitialDelayMS, MaxDelayMS: *req.MaxDelayMS, MaxAgeMS: *req.MaxAgeMS, ExpectedRevision: *req.ExpectedRevision}
	if !ValidRetryPolicyUpdate(update) {
		fail(w, http.StatusBadRequest)
		return
	}
	policy, err := h.Operations.SetRetryPolicy(r.Context(), actor, update)
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, http.StatusOK, policy)
}

// ValidRetryPolicyUpdate mirrors delivery.RetryPolicy.Validate and the table
// CHECK constraints: 1 to 1000 attempts; delays of 1 minute to 24 hours with
// max delay at least the initial delay; age of 1 hour to 30 days, at least the
// initial delay.
func ValidRetryPolicyUpdate(u RetryPolicyUpdate) bool {
	const minute, hour = int64(60_000), int64(3_600_000)
	return u.ExpectedRevision > 0 && u.MaxAttempts >= 1 && u.MaxAttempts <= 1000 &&
		u.InitialDelayMS >= minute && u.InitialDelayMS <= 24*hour &&
		u.MaxDelayMS >= u.InitialDelayMS && u.MaxDelayMS <= 24*hour &&
		u.MaxAgeMS >= hour && u.MaxAgeMS <= 720*hour && u.MaxAgeMS >= u.InitialDelayMS
}
