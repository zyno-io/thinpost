package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/submission"
)

// AppMessageStore is the part of Store that app-key reads use.
type AppMessageStore interface {
	GetMessage(ctx context.Context, actor, appID, messageID string) (MessageDetail, error)
}

// AppHoldStore releases or cancels a message submitted on hold. Each call
// rechecks, in its own transaction, that the app is active and the key
// unrevoked (submission.ErrInvalidKey otherwise), and serializes on the
// message: a repeat of the same action changes nothing and returns nil, and a
// message the action cannot apply to returns a HoldConflict. A message of
// another app is ErrNotFound. A change is audited with the actor key:<keyID>.
type AppHoldStore interface {
	ReleaseHeldMessage(ctx context.Context, appID, keyID, messageID string) error
	CancelHeldMessage(ctx context.Context, appID, keyID, messageID string) error
}

// AppReadHandler serves the message routes under /app/v1/ to the submission
// API keys of an app: message status, source body and attachments (GET), and
// release or cancel of a message submitted on hold (POST).
// Authentication is the submission service's (Authorization: Bearer <app key>),
// the key's app is the only app a request can read, and the audit actor is
// "key:<keyId>". Reads share the admin routes' store and content paths, so
// Enhanced Protection reads are audited identically. A message of another app
// is indistinguishable from an absent one (404).
type AppReadHandler struct {
	Service       submission.Service
	Store         AppMessageStore
	ContentReader ContentReader
	// Holds serves release and cancel; nil answers them 503.
	Holds AppHoldStore

	once   sync.Once
	budget *semaphore.Weighted
	// perApp counts each app's in-flight content reads so one key holder
	// cannot occupy the whole shared budget.
	perAppMu sync.Mutex
	perApp   map[string]int
	// Test seams; zero keeps the defaults below.
	readBudget, readWeight int64
	waitTimeout            time.Duration
}

// A body or attachment read decrypts and decodes a stored source of up to 16
// MiB, which costs several times that in memory (ciphertext, plaintext, parsed
// message, JSON encoding); the control pod has a 512Mi limit and also runs the
// operator API, the workers and submission. The size is unknown before the
// read, so every content read is charged the 16 MiB maximum source against a
// 48 MiB budget: at most three run at once (about 150 MiB worst case), and
// small messages are never charged less, which keeps the bound simple and safe.
// The budget is separate from submission's largeBodyBudget so a burst of reads
// cannot starve acceptance, and vice versa. Waiters queue in arrival order and
// give up after readWaitTimeout with 503 and Retry-After.
const (
	readBudgetBytes = 48 << 20
	readWeightBytes = 16 << 20
	readWaitTimeout = 5 * time.Second
	// readsPerApp caps one app's concurrent content reads below the global
	// budget, so a slow or abusive key cannot starve every other app.
	readsPerApp = 2
)

// acquire takes one of the app's content-read slots and the shared budget, or
// reports false: immediately when the app already has readsPerApp reads in
// flight, otherwise after the wait.
func (h *AppReadHandler) acquire(ctx context.Context, appID string) (release func(), ok bool) {
	h.perAppMu.Lock()
	if h.perApp == nil {
		h.perApp = map[string]int{}
	}
	if h.perApp[appID] >= readsPerApp {
		h.perAppMu.Unlock()
		return nil, false
	}
	h.perApp[appID]++
	h.perAppMu.Unlock()
	releaseApp := func() {
		h.perAppMu.Lock()
		if h.perApp[appID]--; h.perApp[appID] <= 0 {
			delete(h.perApp, appID)
		}
		h.perAppMu.Unlock()
	}
	h.once.Do(func() {
		if h.readBudget == 0 {
			h.readBudget = readBudgetBytes
		}
		if h.readWeight == 0 {
			h.readWeight = readWeightBytes
		}
		if h.waitTimeout == 0 {
			h.waitTimeout = readWaitTimeout
		}
		h.budget = semaphore.NewWeighted(h.readBudget)
	})
	waitCtx, cancel := context.WithTimeout(ctx, h.waitTimeout)
	defer cancel()
	if h.budget.Acquire(waitCtx, h.readWeight) != nil {
		releaseApp()
		return nil, false
	}
	return func() { h.budget.Release(h.readWeight); releaseApp() }, true
}

const appReadPrefix = "/app/v1/messages/"

func (h *AppReadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
	header.Set("Referrer-Policy", "no-referrer")
	// The routes have one canonical spelling. Matching the decoded path alone
	// would let /%61pp/... or <id>%2Fbody through, so any percent-encoding is 404.
	if strings.Contains(r.URL.EscapedPath(), "%") || !strings.HasPrefix(r.URL.Path, appReadPrefix) {
		fail(w, http.StatusNotFound)
		return
	}
	// Shapes: {id}, {id}/body, {id}/attachments/{index}, {id}/release, {id}/cancel.
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, appReadPrefix), "/")
	id, ok := providerid.Normalize(parts[0])
	if !ok || !opaqueID(id) {
		fail(w, http.StatusNotFound)
		return
	}
	kind, index := "", 0
	switch {
	case len(parts) == 1:
		kind = "message"
	case len(parts) == 2 && parts[1] == "body":
		kind = "body"
	case len(parts) == 2 && (parts[1] == "release" || parts[1] == "cancel"):
		kind = parts[1]
	case len(parts) == 3 && parts[1] == "attachments":
		kind = "attachment"
		if index, ok = attachmentIndex(parts[2]); !ok {
			fail(w, http.StatusNotFound)
			return
		}
	default:
		fail(w, http.StatusNotFound)
		return
	}
	if want := holdMethod(kind); r.Method != want {
		method(w, want)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		fail(w, http.StatusBadRequest)
		return
	}
	if h.Service == nil || h.Store == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	key, ok := appKey(r)
	if !ok {
		fail(w, http.StatusUnauthorized)
		return
	}
	app, err := h.Service.Authenticate(r.Context(), key)
	if err != nil || app.ID == "" || app.KeyID == "" {
		if err == nil || errors.Is(err, submission.ErrInvalidKey) {
			fail(w, http.StatusUnauthorized)
		} else {
			fail(w, http.StatusServiceUnavailable)
		}
		return
	}
	actor := "key:" + app.KeyID
	switch kind {
	case "message":
		detail, err := h.Store.GetMessage(r.Context(), actor, app.ID, id)
		if err != nil {
			storeError(w, err)
			return
		}
		if detail.Deliveries == nil {
			detail.Deliveries = []DeliverySummary{}
		}
		respond(w, http.StatusOK, detail)
	case "body":
		if h.ContentReader == nil {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		release, ok := h.acquire(r.Context(), app.ID)
		if !ok {
			header.Set("Retry-After", "5")
			fail(w, http.StatusServiceUnavailable)
			return
		}
		defer release()
		body, err := h.ContentReader.ReadMessageBody(r.Context(), actor, app.ID, id)
		if err != nil {
			storeError(w, err)
			return
		}
		if body.Attachments == nil {
			body.Attachments = []AttachmentMetadata{}
		}
		respond(w, http.StatusOK, body)
	case "release", "cancel":
		if h.Holds == nil {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		if kind == "release" {
			err = h.Holds.ReleaseHeldMessage(r.Context(), app.ID, app.KeyID, id)
		} else {
			err = h.Holds.CancelHeldMessage(r.Context(), app.ID, app.KeyID, id)
		}
		var conflict HoldConflict
		switch {
		case errors.As(err, &conflict):
			respond(w, http.StatusConflict, map[string]string{"error": string(conflict)})
			return
		case errors.Is(err, submission.ErrInvalidKey):
			fail(w, http.StatusUnauthorized)
			return
		case err != nil:
			storeError(w, err)
			return
		}
		// The action committed; report the message as it is now, which may
		// already have moved on (a released message being prepared).
		detail, err := h.Store.GetMessage(r.Context(), actor, app.ID, id)
		if err != nil {
			storeError(w, err)
			return
		}
		if detail.Deliveries == nil {
			detail.Deliveries = []DeliverySummary{}
		}
		respond(w, http.StatusOK, detail)
	case "attachment":
		if h.ContentReader == nil {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		release, ok := h.acquire(r.Context(), app.ID)
		if !ok {
			header.Set("Retry-After", "5")
			fail(w, http.StatusServiceUnavailable)
			return
		}
		defer release()
		attachment, err := h.ContentReader.ReadAttachment(r.Context(), actor, app.ID, id, index)
		if err != nil {
			storeError(w, err)
			return
		}
		writeAttachment(w, index, attachment)
	}
}

// holdMethod is the one method each route serves: POST for the hold actions,
// GET for reads.
func holdMethod(kind string) string {
	if kind == "release" || kind == "cancel" {
		return http.MethodPost
	}
	return http.MethodGet
}

// appKey extracts the single Bearer credential, as the submission API does.
func appKey(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 512 {
		return "", false
	}
	return parts[1], true
}
