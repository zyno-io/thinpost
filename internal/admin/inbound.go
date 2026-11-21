package admin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

// Inbound domain states.
const (
	InboundDomainPending  = "pending"
	InboundDomainActive   = "active"
	InboundDomainDisabled = "disabled"
)

// MX check results. They are informational: a domain can be verified and
// active whatever its MX records say.
const (
	MXThinpost = "thinpost" // every MX record names the Thinpost mail host
	MXMixed    = "mixed"    // some do, some name another host
	MXOther    = "other"    // none do
	MXNone     = "none"     // the domain has no MX record
	MXNull     = "null_mx"  // a single "." record (RFC 7505): the domain takes no mail
	MXError    = "error"    // the lookup failed; the previous answer is unknown
)

// Inbound limit bounds. They match the migration's CHECK constraints.
const (
	MinInboundLimit        = 1
	MaxInboundDailyLimit   = 1000000
	MaxInboundPendingLimit = 100000
)

// InboundDomain is a receiving domain of one app. MXStatus and MXCheckedAt are
// absent until a check has run.
type InboundDomain struct {
	Name        string     `json:"name"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"created_at"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
	MXStatus    *string    `json:"mx_status,omitempty"`
	MXCheckedAt *time.Time `json:"mx_checked_at,omitempty"`
}

// InboundDomainList carries the MX host the console tells people to point
// their records at; it is empty when this deployment has none configured.
type InboundDomainList struct {
	Domains []InboundDomain `json:"domains"`
	MXHost  string          `json:"mx_host,omitempty"`
	// MXHostStatus is ok when MXHost is set; otherwise not_reported (the sending
	// worker has not reported its IPs yet) or dns_unconfirmed (public DNS does
	// not map the name to the address the worker reported).
	MXHostStatus string `json:"mx_host_status"`
}

// InboundDomainChallenge is the TXT record that proves control of a domain and
// the MX record that routes its mail. The value is shown only once.
type InboundDomainChallenge struct {
	Domain   InboundDomain `json:"domain"`
	DNSName  string        `json:"dns_name"`
	DNSValue string        `json:"dns_value"`
	MXHost   string        `json:"mx_host,omitempty"`
	// MXHostStatus is as in InboundDomainList.
	MXHostStatus string `json:"mx_host_status"`
}

// InboundLimits are an app's inbound caps: messages accepted per UTC day and
// inbound webhook deliveries waiting at once.
type InboundLimits struct {
	DailyLimit   int `json:"daily_limit"`
	PendingLimit int `json:"pending_limit"`
}

// MXResolver is the slice of net.Resolver the MX check needs.
type MXResolver interface {
	LookupMX(context.Context, string) ([]*net.MX, error)
}

// InboundStore persists receiving domains and limits. Every mutation records
// its audit event in the same transaction. A store that finds the domain
// already verified by another app returns ErrConflict from VerifyInboundDomain.
type InboundStore interface {
	ListInboundDomains(ctx context.Context, appID string) ([]InboundDomain, error)
	RequestInboundDomain(ctx context.Context, actor, appID, domain string) (InboundDomainChallenge, error)
	InboundDomainChallenge(ctx context.Context, appID, domain string) (InboundDomain, [32]byte, error)
	VerifyInboundDomain(ctx context.Context, actor, appID, domain string, digest [32]byte) (InboundDomain, error)
	RecordInboundMX(ctx context.Context, actor, appID, domain, status string) (InboundDomain, error)
	SetInboundDomainEnabled(ctx context.Context, actor, appID, domain string, enabled bool) (InboundDomain, error)
	DeleteInboundDomain(ctx context.Context, actor, appID, domain string) error
	GetInboundLimits(ctx context.Context, appID string) (InboundLimits, error)
	SetInboundLimits(ctx context.Context, actor, appID string, limits InboundLimits) (InboundLimits, error)
}

// ClassifyMX folds a domain's MX records into one status against the Thinpost
// mail host. The records of a failed lookup are not passed here.
func ClassifyMX(records []*net.MX, host string) string {
	host = normalizeHost(host)
	var targets []string
	for _, record := range records {
		if record != nil {
			targets = append(targets, normalizeHost(record.Host))
		}
	}
	if len(targets) == 0 {
		return MXNone
	}
	if len(targets) == 1 && targets[0] == "" {
		return MXNull
	}
	ours := 0
	for _, target := range targets {
		if host != "" && target == host {
			ours++
		}
	}
	switch {
	case ours == len(targets):
		return MXThinpost
	case ours > 0:
		return MXMixed
	}
	return MXOther
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

// CheckMX looks up the MX records of a domain. A missing domain or an empty
// answer is MXNone; any other failure is MXError.
func CheckMX(ctx context.Context, resolver MXResolver, domain, host string) string {
	records, err := resolver.LookupMX(ctx, domain)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return MXNone
		}
		// net.Resolver returns the records it could parse next to a non-fatal
		// error for a null MX or an invalid target; treat records as the answer.
		if len(records) == 0 {
			return MXError
		}
	}
	return ClassifyMX(records, host)
}

// inboundDomainReserved reports whether domain is a HELO name the sending
// worker reported (which is also its bounce domain and the inbound mail host),
// or a subdomain of one. Those names carry Thinpost's own mail and can never be
// a customer receiving domain. Names are reserved whether or not DNS confirms
// them.
func (h *Handler) inboundDomainReserved(ctx context.Context, domain string) (bool, error) {
	identity, err := h.edgeIdentity(ctx)
	if err != nil {
		return false, err
	}
	for _, reserved := range identity.Hostnames {
		if reserved != "" && (domain == reserved || strings.HasSuffix(domain, "."+reserved)) {
			return true, nil
		}
	}
	return false, nil
}

func (h *Handler) inboundRoute(w http.ResponseWriter, r *http.Request, actor, appID string, path []string) bool {
	if len(path) >= 6 && path[5] == "inbound-messages" {
		h.inboundMessageRoute(w, r, actor, appID, path)
		return true
	}
	if len(path) < 6 || (path[5] != "inbound-domains" && path[5] != "inbound-limits") {
		return false
	}
	store, ok := h.Store.(InboundStore)
	if !ok {
		fail(w, http.StatusServiceUnavailable)
		return true
	}
	switch {
	case len(path) == 6 && path[5] == "inbound-limits":
		h.inboundLimits(w, r, store, actor, appID)
	case len(path) == 6:
		h.inboundDomains(w, r, store, actor, appID)
	case len(path) == 7 && path[5] == "inbound-domains":
		if r.Method != http.MethodDelete {
			method(w, "DELETE")
			return true
		}
		domain, ok := NormalizeDomain(path[6])
		if !ok || r.URL.RawQuery != "" || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			fail(w, http.StatusBadRequest)
			return true
		}
		if err := store.DeleteInboundDomain(r.Context(), actor, appID, domain); err != nil {
			storeError(w, err)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	case len(path) == 8 && path[5] == "inbound-domains":
		h.inboundDomainAction(w, r, store, actor, appID, path[6], path[7])
	default:
		http.NotFound(w, r)
	}
	return true
}

func (h *Handler) inboundDomains(w http.ResponseWriter, r *http.Request, store InboundStore, actor, appID string) {
	switch r.Method {
	case http.MethodGet:
		domains, err := store.ListInboundDomains(r.Context(), appID)
		if err != nil {
			storeError(w, err)
			return
		}
		if domains == nil {
			domains = []InboundDomain{}
		}
		mxHost, mxStatus, err := h.inboundMXHost(r.Context())
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, InboundDomainList{Domains: domains, MXHost: mxHost, MXHostStatus: mxStatus})
	case http.MethodPost:
		var req struct {
			Domain string `json:"domain"`
		}
		if decode(r, &req) != nil {
			fail(w, http.StatusBadRequest)
			return
		}
		domain, ok := NormalizeDomain(req.Domain)
		if !ok {
			fail(w, http.StatusBadRequest)
			return
		}
		reserved, err := h.inboundDomainReserved(r.Context(), domain)
		if err != nil {
			storeError(w, err)
			return
		}
		if reserved {
			respond(w, http.StatusBadRequest, map[string]string{"error": "This domain is used by Thinpost itself and can't receive mail for an app."})
			return
		}
		// Read the MX host first: the claim commits a one-time challenge, which a
		// failure after it would lose.
		mxHost, mxStatus, err := h.inboundMXHost(r.Context())
		if err != nil {
			storeError(w, err)
			return
		}
		challenge, err := store.RequestInboundDomain(r.Context(), actor, appID, domain)
		if err != nil {
			storeError(w, err)
			return
		}
		challenge.MXHost, challenge.MXHostStatus = mxHost, mxStatus
		respond(w, http.StatusCreated, challenge)
	default:
		method(w, "GET, POST")
	}
}

func (h *Handler) inboundDomainAction(w http.ResponseWriter, r *http.Request, store InboundStore, actor, appID, rawDomain, action string) {
	if action != "verify" && action != "check-mx" && action != "disable" && action != "enable" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		method(w, "POST")
		return
	}
	if r.URL.RawQuery != "" || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		fail(w, http.StatusBadRequest)
		return
	}
	domain, ok := NormalizeDomain(rawDomain)
	if !ok {
		fail(w, http.StatusBadRequest)
		return
	}
	var result InboundDomain
	var err error
	switch action {
	case "verify":
		result, err = h.verifyInboundDomain(r.Context(), store, actor, appID, domain)
	case "check-mx":
		result, err = h.checkInboundMX(r.Context(), store, actor, appID, domain)
	default:
		result, err = store.SetInboundDomainEnabled(r.Context(), actor, appID, domain, action == "enable")
	}
	if err != nil {
		var status *statusError
		if errors.As(err, &status) {
			fail(w, status.code)
			return
		}
		storeError(w, err)
		return
	}
	respond(w, http.StatusOK, result)
}

// statusError carries an HTTP status chosen by a handler step that is not a
// store error (an unavailable resolver, a failed lookup, a challenge mismatch).
type statusError struct{ code int }

func (e *statusError) Error() string { return http.StatusText(e.code) }

func (h *Handler) verifyInboundDomain(ctx context.Context, store InboundStore, actor, appID, domain string) (InboundDomain, error) {
	current, digest, err := store.InboundDomainChallenge(ctx, appID, domain)
	if err != nil {
		return InboundDomain{}, err
	}
	if current.State != InboundDomainPending {
		return current, nil
	}
	if reserved, err := h.inboundDomainReserved(ctx, domain); err != nil {
		return InboundDomain{}, err
	} else if reserved {
		return InboundDomain{}, ErrInvalidInput
	}
	if h.DNS == nil {
		return InboundDomain{}, &statusError{http.StatusServiceUnavailable}
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	records, err := h.DNS.LookupTXT(lookupCtx, "_thinpost."+domain)
	cancel()
	if err != nil {
		return InboundDomain{}, &statusError{http.StatusServiceUnavailable}
	}
	matched := false
	for _, record := range records {
		hash := sha256.Sum256([]byte(record))
		if subtle.ConstantTimeCompare(hash[:], digest[:]) == 1 {
			matched = true
		}
	}
	if !matched {
		return InboundDomain{}, ErrConflict
	}
	return store.VerifyInboundDomain(ctx, actor, appID, domain, digest)
}

func (h *Handler) checkInboundMX(ctx context.Context, store InboundStore, actor, appID, domain string) (InboundDomain, error) {
	host, _, err := h.inboundMXHost(ctx)
	if err != nil {
		return InboundDomain{}, err
	}
	if h.MX == nil || host == "" {
		return InboundDomain{}, &statusError{http.StatusServiceUnavailable}
	}
	// Check that the domain belongs to this app before spending a lookup on it.
	if _, _, err := store.InboundDomainChallenge(ctx, appID, domain); err != nil {
		return InboundDomain{}, err
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	status := CheckMX(lookupCtx, h.MX, domain, host)
	cancel()
	return store.RecordInboundMX(ctx, actor, appID, domain, status)
}

func validInboundLimits(l InboundLimits) bool {
	return l.DailyLimit >= MinInboundLimit && l.DailyLimit <= MaxInboundDailyLimit &&
		l.PendingLimit >= MinInboundLimit && l.PendingLimit <= MaxInboundPendingLimit
}

func (h *Handler) inboundLimits(w http.ResponseWriter, r *http.Request, store InboundStore, actor, appID string) {
	switch r.Method {
	case http.MethodGet:
		limits, err := store.GetInboundLimits(r.Context(), appID)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, limits)
	case http.MethodPut:
		var request struct {
			DailyLimit   *int `json:"daily_limit"`
			PendingLimit *int `json:"pending_limit"`
		}
		if decode(r, &request) != nil || request.DailyLimit == nil || request.PendingLimit == nil {
			fail(w, http.StatusBadRequest)
			return
		}
		limits := InboundLimits{DailyLimit: *request.DailyLimit, PendingLimit: *request.PendingLimit}
		if !validInboundLimits(limits) {
			fail(w, http.StatusBadRequest)
			return
		}
		saved, err := store.SetInboundLimits(r.Context(), actor, appID, limits)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, saved)
	default:
		method(w, "GET, PUT")
	}
}
