package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/customdomain"
)

// ErrCustomTrackingUnavailable: this deployment does not offer custom tracking
// domains (tracking is off, or THINPOST_ADMIN_ORIGIN is not set, so the console
// could be reached on a customer's domain). 409.
var ErrCustomTrackingUnavailable = errors.Join(ErrConflict, errors.New("custom tracking domains are unavailable"))

// Tracking domain states, as the API reports them.
const (
	TrackingDomainPending  = customdomain.StatePending
	TrackingDomainActive   = customdomain.StateActive
	TrackingDomainFailing  = customdomain.StateFailing
	TrackingDomainRemoved  = customdomain.StateRemoved
	maxTrackingDomainBytes = 253
)

// TrackingDomain is an app's own tracking address for one sender domain. New
// links in the app's mail use it only while State is "active", a check passed
// within the last 3 hours and the deployment has custom domains enabled;
// otherwise they use Thinpost's standard tracking address.
type TrackingDomain struct {
	Name  string `json:"name"`
	State string `json:"state"`
	// Checking is true while a check is in flight.
	Checking bool `json:"checking"`
	// DNSResult is informational and never blocks use: ok, missing,
	// wrong_target or lookup_failed. A name behind a proxy reports
	// wrong_target and works.
	DNSResult *string `json:"dns_result,omitempty"`
	// HTTPSResult decides use: ok, unreachable, certificate, redirected or
	// not_thinpost.
	HTTPSResult *string `json:"https_result,omitempty"`
	// ObservedTarget is the name DNS pointed at when it was not the tracking
	// address.
	ObservedTarget       *string    `json:"observed_target,omitempty"`
	CertificateExpiresAt *time.Time `json:"certificate_expires_at,omitempty"`
	CheckedAt            *time.Time `json:"checked_at,omitempty"`
	LastOKAt             *time.Time `json:"last_ok_at,omitempty"`
	VerifiedAt           *time.Time `json:"verified_at,omitempty"`
	RemovedAt            *time.Time `json:"removed_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
}

// TrackingDomainListing is a tracking domain with the app and sender domain it
// belongs to, for the operator-wide list.
type TrackingDomainListing struct {
	AppID        string `json:"app_id"`
	SenderDomain string `json:"sender_domain"`
	TrackingDomain
}

// TrackingDomainStore keeps tracking domains. Mutations audit in the same
// transaction (tracking_domain.added, activated, failing, removed). The check
// functions implement the same lease as bounce domains: BeginTrackingCheck
// takes a fresh token, so the later claimant wins, and FinishTrackingCheck
// writes nothing when the token is no longer current.
type TrackingDomainStore interface {
	// ListTrackingDomains returns the app's live (not removed) tracking
	// domains by sender domain.
	ListTrackingDomains(ctx context.Context, appID string) (map[string]TrackingDomain, error)
	// GetTrackingDomain returns the sender domain's live tracking domain;
	// ErrNotFound when it has none.
	GetTrackingDomain(ctx context.Context, appID, senderDomain string) (TrackingDomain, error)
	// SetTrackingDomain creates the sender domain's tracking domain, or
	// replaces it (the old one becomes a tombstone). The same name again
	// changes nothing; a name that was removed is revived as pending.
	SetTrackingDomain(ctx context.Context, actor, appID, senderDomain, name string) (TrackingDomain, error)
	// DeleteTrackingDomain removes it (it stays as a tombstone that is still
	// checked daily); ErrNotFound when there is none.
	DeleteTrackingDomain(ctx context.Context, actor, appID, senderDomain string) error
	// BeginTrackingCheck leases the live row to a manual check under a fresh
	// token.
	BeginTrackingCheck(ctx context.Context, appID, senderDomain string) (customdomain.Check, error)
	// FinishTrackingCheck records a result and returns the row; written is
	// false when the token was no longer current.
	FinishTrackingCheck(ctx context.Context, actor string, check customdomain.Check, outcome customdomain.TrackingOutcome) (TrackingDomain, bool, error)
	// ListAllTrackingDomains lists every tracking domain of every app,
	// removed tombstones included.
	ListAllTrackingDomains(ctx context.Context) ([]TrackingDomainListing, error)
}

// TrackingDomainChecker runs one tracking check. customdomain.TrackingChecker
// is the production implementation.
type TrackingDomainChecker interface {
	Check(ctx context.Context, domain string) customdomain.TrackingOutcome
}

// The manual check is bounded: DNS lookups, then one HTTPS request.
const trackingCheckTimeout = 20 * time.Second

// customTrackingAvailable reports whether this replica offers custom tracking
// domains.
func (h *Handler) customTrackingAvailable() bool {
	return h.Runtime != nil && h.Runtime.Tracking != nil && h.Runtime.Tracking.CustomDomains
}

// trackingDomainReserved reports whether name is one of this deployment's own
// hosts: the tracking address or the console's.
func (h *Handler) trackingDomainReserved(name string) bool {
	if h.AdminHost != "" && name == h.AdminHost {
		return true
	}
	if h.Runtime != nil && h.Runtime.Tracking != nil && h.Runtime.Tracking.CNAMETarget != "" && name == h.Runtime.Tracking.CNAMETarget {
		return true
	}
	return false
}

// trackingDomainsRoute serves GET /api/v1/tracking-domains: every tracking
// domain of every app with its state, check results, certificate expiry and
// removed tombstones, for operators' certificate and routing automation. It
// reports false for any other path.
func (h *Handler) trackingDomainsRoute(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/api/v1/tracking-domains" {
		return false
	}
	store, ok := h.Store.(TrackingDomainStore)
	if !ok || r.URL.RawQuery != "" {
		fail(w, http.StatusServiceUnavailable)
		return true
	}
	if r.Method != http.MethodGet {
		method(w, "GET")
		return true
	}
	list, err := store.ListAllTrackingDomains(r.Context())
	if err != nil {
		storeError(w, err)
		return true
	}
	if list == nil {
		list = []TrackingDomainListing{}
	}
	respond(w, http.StatusOK, map[string]any{"tracking_domains": list})
	return true
}

// trackingDomainRoute serves /apps/{appId}/domains/{domain}/tracking-domain:
//
//	GET, PUT, DELETE   the sender domain's tracking domain
//	POST check         look at it now
//
// rest is the path after "domains". It reports false for a path it does not own.
func (h *Handler) trackingDomainRoute(w http.ResponseWriter, r *http.Request, actor, appID string, rest []string) bool {
	if len(rest) < 2 || rest[1] != "tracking-domain" || len(rest) > 3 || len(rest) == 3 && rest[2] != "check" {
		return false
	}
	store, ok := h.Store.(TrackingDomainStore)
	if !ok {
		fail(w, http.StatusServiceUnavailable)
		return true
	}
	sender, ok := NormalizeDomain(rest[0])
	if !ok || r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest)
		return true
	}
	ctx := r.Context()
	if len(rest) == 3 {
		if r.Method != http.MethodPost {
			method(w, "POST")
			return true
		}
		if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			fail(w, http.StatusBadRequest)
			return true
		}
		h.checkTrackingDomain(w, r, store, actor, appID, sender)
		return true
	}
	switch r.Method {
	case http.MethodGet:
		current, err := store.GetTrackingDomain(ctx, appID, sender)
		if err != nil {
			storeError(w, err)
			return true
		}
		respond(w, http.StatusOK, current)
	case http.MethodPut:
		var req struct {
			Name string `json:"name"`
		}
		if decode(r, &req) != nil {
			fail(w, http.StatusBadRequest)
			return true
		}
		if !h.customTrackingAvailable() {
			respond(w, http.StatusConflict, map[string]string{"error": "Your own tracking address isn't available on this Thinpost installation. Ask your Thinpost administrator."})
			return true
		}
		name, ok := NormalizeDomain(strings.TrimSpace(req.Name))
		if !ok || len(name) > maxTrackingDomainBytes {
			respond(w, http.StatusBadRequest, map[string]string{"error": "That is not a valid domain name."})
			return true
		}
		if !customdomain.StrictSubdomain(name, sender) {
			respond(w, http.StatusBadRequest, map[string]string{"error": "The tracking domain must be a subdomain of " + sender + ", such as links." + sender + "."})
			return true
		}
		if h.trackingDomainReserved(name) {
			respond(w, http.StatusBadRequest, map[string]string{"error": "This name is used by Thinpost itself and can't be a tracking domain."})
			return true
		}
		created, err := store.SetTrackingDomain(ctx, actor, appID, sender, name)
		if err != nil {
			trackingDomainError(w, err)
			return true
		}
		respond(w, http.StatusOK, created)
	case http.MethodDelete:
		if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			fail(w, http.StatusBadRequest)
			return true
		}
		if err := store.DeleteTrackingDomain(ctx, actor, appID, sender); err != nil {
			trackingDomainError(w, err)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		method(w, "GET, PUT, DELETE")
	}
	return true
}

func trackingDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrAppPaused):
		respond(w, http.StatusConflict, map[string]string{"error": "This app is paused. Resume it to change its tracking domain."})
	case errors.Is(err, ErrSenderDomainUnverified):
		respond(w, http.StatusBadRequest, map[string]string{"error": "Verify the sending domain first: a tracking domain belongs to a verified sending domain."})
	default:
		storeError(w, err)
	}
}

// checkTrackingDomain looks at the domain now. The check is bounded, runs
// under a fresh lease token (so it beats any background check in flight) and
// answers with the row as recorded.
func (h *Handler) checkTrackingDomain(w http.ResponseWriter, r *http.Request, store TrackingDomainStore, actor, appID, sender string) {
	if h.TrackingChecker == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	check, err := store.BeginTrackingCheck(r.Context(), appID, sender)
	if err != nil {
		storeError(w, err)
		return
	}
	// A client that disconnects must not turn the lookup into a recorded
	// failure: the check runs to its own deadline either way.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), trackingCheckTimeout)
	outcome := h.TrackingChecker.Check(ctx, check.Domain)
	cancel()
	recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer recordCancel()
	updated, written, err := store.FinishTrackingCheck(recordCtx, actor, check, outcome)
	if err != nil {
		storeError(w, err)
		return
	}
	if !written {
		// A newer check (or a removal) took the row while this one ran.
		fail(w, http.StatusConflict)
		return
	}
	respond(w, http.StatusOK, updated)
}

// attachTrackingDomains adds each sender domain's tracking domain to the list
// the console shows. It is a no-op for a store without tracking domains.
func (h *Handler) attachTrackingDomains(ctx context.Context, appID string, domains []Domain) error {
	store, ok := h.Store.(TrackingDomainStore)
	if !ok || len(domains) == 0 {
		return nil
	}
	existing, err := store.ListTrackingDomains(ctx, appID)
	if err != nil {
		return err
	}
	for i := range domains {
		if t, ok := existing[domains[i].Name]; ok {
			t := t
			domains[i].TrackingDomain = &t
		}
	}
	return nil
}
