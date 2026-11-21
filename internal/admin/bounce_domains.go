package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/customdomain"
)

// Errors of the bounce-domain store beyond the generic ones. Each wraps the
// generic error that gives its HTTP status.
var (
	// ErrInboundDomainClash: the name is a receiving domain that can take mail
	// (active or disabled). 409.
	ErrInboundDomainClash = errors.Join(ErrConflict, errors.New("domain is a receiving domain"))
	// ErrBounceDomainClash: a receiving domain was requested or verified for a
	// name an app uses as a bounce domain. 400.
	ErrBounceDomainClash = errors.Join(ErrInvalidInput, errors.New("domain is a bounce domain"))
	// ErrAppPaused: the app is paused, so its domains cannot be changed. 409.
	ErrAppPaused = errors.Join(ErrConflict, errors.New("app is paused"))
	// ErrSenderDomainUnverified: the sending domain has no verified DKIM key
	// yet. 400.
	ErrSenderDomainUnverified = errors.Join(ErrInvalidInput, errors.New("sending domain is not verified"))
)

// Bounce domain states, as the API reports them.
const (
	BounceDomainPending = customdomain.StatePending
	BounceDomainActive  = customdomain.StateActive
	BounceDomainFailing = customdomain.StateFailing
)

// BounceDomain is an app's own Return-Path domain for one sender domain. Mail
// uses it only while State is "active" and the domain still points at the
// HELO name of the app's sending IP; otherwise Thinpost's standard bounce
// address is used and nothing is lost.
type BounceDomain struct {
	Name  string `json:"name"`
	State string `json:"state"`
	// Checking is true while a check is in flight.
	Checking bool `json:"checking"`
	// Result is the last check's outcome: ok, missing, not_cname, other_ip,
	// wrong_target, lookup_failed or no_sending_ip. Absent before the first.
	Result *string `json:"result,omitempty"`
	// ObservedTarget is the name the CNAME pointed at on the last check.
	ObservedTarget *string    `json:"observed_target,omitempty"`
	CheckedAt      *time.Time `json:"checked_at,omitempty"`
	LastOKAt       *time.Time `json:"last_ok_at,omitempty"`
	VerifiedAt     *time.Time `json:"verified_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// BounceDomainView is a bounce domain with what its CNAME must point at.
type BounceDomainView struct {
	BounceDomain
	CNAMETarget       *string `json:"cname_target,omitempty"`
	CNAMETargetStatus string  `json:"cname_target_status"`
}

// BounceDomainStore keeps bounce domains. Mutations audit in the same
// transaction (bounce_domain.added, activated, failing, removed). The check
// functions implement the lease: BeginBounceCheck takes a fresh token, so
// the later claimant wins, and FinishBounceCheck writes nothing when the
// token is no longer current.
type BounceDomainStore interface {
	// ListBounceDomains returns the app's bounce domains by sender domain.
	ListBounceDomains(ctx context.Context, appID string) (map[string]BounceDomain, error)
	// GetBounceDomain returns the bounce domain of a sender domain; ErrNotFound
	// when it has none.
	GetBounceDomain(ctx context.Context, appID, senderDomain string) (BounceDomain, error)
	// SetBounceDomain creates the sender domain's bounce domain, or replaces
	// it in the same transaction. The same name again changes nothing.
	SetBounceDomain(ctx context.Context, actor, appID, senderDomain, name string) (BounceDomain, error)
	// DeleteBounceDomain removes it; ErrNotFound when there is none.
	DeleteBounceDomain(ctx context.Context, actor, appID, senderDomain string) error
	// BounceTarget is what the app's bounce domains must point at.
	BounceTarget(ctx context.Context, appID string) (customdomain.Target, error)
	// BeginBounceCheck leases the row to a manual check under a fresh token.
	BeginBounceCheck(ctx context.Context, appID, senderDomain string) (customdomain.Check, error)
	// FinishBounceCheck records a result and returns the row. written is false
	// when the token was no longer current (a newer check or an IP change took
	// the row) and nothing was recorded.
	FinishBounceCheck(ctx context.Context, actor string, check customdomain.Check, result customdomain.Result, observed string) (BounceDomain, bool, error)
}

// maxBounceDomainBytes matches the Return-Path limit a delivery enforces.
const maxBounceDomainBytes = 219

// The check is bounded so the console's button never hangs on slow DNS.
const bounceCheckTimeout = 8 * time.Second

// bounceDomainRoute serves /apps/{appId}/domains/{domain}/bounce-domain:
//
//	GET, PUT, DELETE   the sender domain's bounce domain
//	POST check         look at its DNS now
//
// rest is the path after "domains": the domain and what follows. It reports
// false for a path it does not own.
func (h *Handler) bounceDomainRoute(w http.ResponseWriter, r *http.Request, actor, appID string, rest []string) bool {
	if len(rest) < 2 || rest[1] != "bounce-domain" || len(rest) > 3 || len(rest) == 3 && rest[2] != "check" {
		return false
	}
	store, ok := h.Store.(BounceDomainStore)
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
		h.checkBounceDomain(w, r, store, actor, appID, sender)
		return true
	}
	switch r.Method {
	case http.MethodGet:
		current, err := store.GetBounceDomain(ctx, appID, sender)
		if err != nil {
			storeError(w, err)
			return true
		}
		h.respondBounceDomain(w, r, store, appID, current, http.StatusOK)
	case http.MethodPut:
		var req struct {
			Name string `json:"name"`
		}
		if decode(r, &req) != nil {
			fail(w, http.StatusBadRequest)
			return true
		}
		name, ok := NormalizeDomain(strings.TrimSpace(req.Name))
		if !ok || len(name) > maxBounceDomainBytes {
			respond(w, http.StatusBadRequest, map[string]string{"error": "That is not a valid domain name."})
			return true
		}
		if !customdomain.StrictSubdomain(name, sender) {
			respond(w, http.StatusBadRequest, map[string]string{"error": "The bounce domain must be a subdomain of " + sender + ", such as bounces." + sender + "."})
			return true
		}
		reserved, err := h.bounceDomainReserved(ctx, name)
		if err != nil {
			storeError(w, err)
			return true
		}
		if reserved {
			respond(w, http.StatusBadRequest, map[string]string{"error": "This name is used by Thinpost itself and can't be a bounce domain."})
			return true
		}
		created, err := store.SetBounceDomain(ctx, actor, appID, sender, name)
		if err != nil {
			h.bounceDomainError(w, err)
			return true
		}
		h.respondBounceDomain(w, r, store, appID, created, http.StatusOK)
	case http.MethodDelete:
		if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			fail(w, http.StatusBadRequest)
			return true
		}
		if err := store.DeleteBounceDomain(ctx, actor, appID, sender); err != nil {
			h.bounceDomainError(w, err)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		method(w, "GET, PUT, DELETE")
	}
	return true
}

// bounceDomainError answers the errors that have a friendlier message than
// their status.
func (h *Handler) bounceDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInboundDomainClash):
		respond(w, http.StatusConflict, map[string]string{"error": "This domain already receives mail through Thinpost, so it can't also be a bounce domain."})
	case errors.Is(err, ErrAppPaused):
		respond(w, http.StatusConflict, map[string]string{"error": "This app is paused. Resume it to change its bounce domain."})
	case errors.Is(err, ErrSenderDomainUnverified):
		respond(w, http.StatusBadRequest, map[string]string{"error": "Verify the sending domain first: a bounce domain belongs to a verified sending domain."})
	default:
		storeError(w, err)
	}
}

// checkBounceDomain looks at the CNAME now. The check is bounded, runs under a
// fresh lease token (so it beats any background check already in flight) and
// answers with the row as recorded.
func (h *Handler) checkBounceDomain(w http.ResponseWriter, r *http.Request, store BounceDomainStore, actor, appID, sender string) {
	if h.CNAME == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	check, err := store.BeginBounceCheck(r.Context(), appID, sender)
	if err != nil {
		storeError(w, err)
		return
	}
	// A client that disconnects must not turn the lookup into a recorded
	// failure: the check runs to its own deadline either way.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), bounceCheckTimeout)
	result, observed := customdomain.CheckBounce(ctx, h.CNAME, check.Domain, check.Target)
	cancel()
	// The result is recorded even if the request's own deadline has passed.
	recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer recordCancel()
	updated, written, err := store.FinishBounceCheck(recordCtx, actor, check, result, observed)
	if err != nil {
		storeError(w, err)
		return
	}
	if !written {
		// A newer check (or an IP change) took the row while this one ran.
		fail(w, http.StatusConflict)
		return
	}
	h.respondBounceDomain(w, r, store, appID, updated, http.StatusOK)
}

func (h *Handler) respondBounceDomain(w http.ResponseWriter, r *http.Request, store BounceDomainStore, appID string, d BounceDomain, status int) {
	view := BounceDomainView{BounceDomain: d}
	target, state, err := h.cnameTarget(r.Context(), store, appID)
	if err != nil {
		storeError(w, err)
		return
	}
	if state == HostNameOK {
		view.CNAMETarget = &target
	}
	view.CNAMETargetStatus = state
	respond(w, status, view)
}

// cnameTarget is the name a bounce domain's CNAME must point at: the HELO name
// of the app's current sending IP. The edge's report is not trusted, so the
// name is shown only once DNS ties it to its address both ways.
func (h *Handler) cnameTarget(ctx context.Context, store BounceDomainStore, appID string) (name, status string, err error) {
	target, err := store.BounceTarget(ctx, appID)
	if err != nil {
		return "", "", err
	}
	if target.Name == "" {
		return "", HostNameNotReported, nil
	}
	if !h.confirmHostName(ctx, target.Name, target.Address) {
		return "", HostNameDNSUnconfirmed, nil
	}
	return target.Name, HostNameOK, nil
}

// bounceDomainReserved reports whether name is exactly a HELO name the edge
// reported: those names carry Thinpost's own mail.
func (h *Handler) bounceDomainReserved(ctx context.Context, name string) (bool, error) {
	identity, err := h.edgeIdentity(ctx)
	if err != nil {
		return false, err
	}
	for _, reserved := range identity.Hostnames {
		if reserved == name {
			return true, nil
		}
	}
	return false, nil
}

// attachBounceDomains adds each sender domain's bounce domain, and the name
// its CNAME must use, to the list the console shows. It is a no-op for a store
// without bounce domains.
func (h *Handler) attachBounceDomains(ctx context.Context, appID string, domains []Domain) error {
	store, ok := h.Store.(BounceDomainStore)
	if !ok || len(domains) == 0 {
		return nil
	}
	existing, err := store.ListBounceDomains(ctx, appID)
	if err != nil {
		return err
	}
	verified := false
	for i := range domains {
		if b, ok := existing[domains[i].Name]; ok {
			b := b
			domains[i].BounceDomain = &b
		}
		verified = verified || domains[i].Active
	}
	if !verified {
		return nil
	}
	target, state, err := h.cnameTarget(ctx, store, appID)
	if err != nil {
		return err
	}
	for i := range domains {
		if !domains[i].Active {
			continue
		}
		domains[i].CNAMETargetStatus = state
		if state == HostNameOK {
			t := target
			domains[i].CNAMETarget = &t
		}
	}
	return nil
}
