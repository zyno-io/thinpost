package admin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/zyno-io/thinpost/internal/dkimkey"
)

// domainKeyRoute serves the DKIM routes under /apps/{appId}/domains/{domain}:
//
//	POST   verify                      check the pending key's DNS record; first key verifies the domain
//	POST   dkim-keys                   create the next pending key (rotation, or a first key)
//	POST   dkim-keys/{selector}/activate  switch to the pending key once its record is in DNS
//	DELETE dkim-keys/{selector}        cancel a pending key
//
// (DELETE /apps/{appId}/domains/{domain}, with nothing after the domain, is
// domainRoute: it removes the domain.)
//
// rest is the path after "domains": the domain and what follows. It reports
// false for a path it does not own.
func (h *Handler) domainKeyRoute(w http.ResponseWriter, r *http.Request, actor, appID string, rest []string) bool {
	verify := len(rest) == 2 && rest[1] == "verify"
	create := len(rest) == 2 && rest[1] == "dkim-keys"
	activate := len(rest) == 4 && rest[1] == "dkim-keys" && rest[3] == "activate"
	cancel := len(rest) == 3 && rest[1] == "dkim-keys"
	if !verify && !create && !activate && !cancel {
		return false
	}
	domain, ok := NormalizeDomain(rest[0])
	if !ok {
		fail(w, http.StatusBadRequest)
		return true
	}
	selector := ""
	if activate || cancel {
		selector = rest[2]
		if !dkimkey.ValidSelector(selector) {
			fail(w, http.StatusBadRequest)
			return true
		}
	}
	want := http.MethodPost
	if cancel {
		want = http.MethodDelete
	}
	if r.Method != want {
		method(w, want)
		return true
	}
	if r.URL.RawQuery != "" || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		fail(w, http.StatusBadRequest)
		return true
	}
	ctx := r.Context()
	switch {
	case create:
		challenge, err := h.Store.CreateDKIMKey(ctx, actor, appID, domain)
		if err != nil {
			storeError(w, err)
			return true
		}
		respond(w, http.StatusCreated, challenge)
	case cancel:
		if err := h.Store.CancelDKIMKey(ctx, actor, appID, domain, selector); err != nil {
			storeError(w, err)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		h.activateKey(w, r, actor, appID, domain, selector, activate)
	}
	return true
}

// ErrDomainInSMTPPolicy is returned while the app's SMTP From policy sends as
// the domain: every SMTP message would then be refused as temporarily failing.
var ErrDomainInSMTPPolicy = errors.Join(ErrConflict, errors.New("domain is the SMTP From policy's target"))

// domainRoute serves DELETE /apps/{appId}/domains/{domain}: 200 with the
// removal receipt, 409 reason smtp_from_policy while the SMTP From policy
// sends as the domain.
func (h *Handler) domainRoute(w http.ResponseWriter, r *http.Request, actor, appID, name string) {
	domain, ok := NormalizeDomain(name)
	if !ok {
		fail(w, http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodDelete {
		method(w, http.MethodDelete)
		return
	}
	if r.URL.RawQuery != "" || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		fail(w, http.StatusBadRequest)
		return
	}
	removal, err := h.Store.DeleteDomain(r.Context(), actor, appID, domain)
	if errors.Is(err, ErrDomainInSMTPPolicy) {
		respond(w, http.StatusConflict, map[string]string{"error": http.StatusText(http.StatusConflict), "reason": "smtp_from_policy"})
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, http.StatusOK, removal)
}

// activateKey checks the pending key's record in DNS and, when it is there,
// activates the key. Verify and the rotation switch differ only in what a
// found record does to an existing active key.
func (h *Handler) activateKey(w http.ResponseWriter, r *http.Request, actor, appID, domain, selector string, switchActive bool) {
	key, err := h.Store.PendingDKIMKey(r.Context(), appID, domain, selector)
	if err != nil {
		storeError(w, err)
		return
	}
	if h.DNS == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	records, err := h.DNS.LookupTXT(ctx, dkimkey.RecordName(key.Selector, domain))
	cancel()
	if err != nil {
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		records = nil
	}
	if reason := dkimkey.CheckRecord(records, key.PublicKey); reason != dkimkey.ReasonOK {
		respond(w, http.StatusConflict, map[string]string{"error": http.StatusText(http.StatusConflict), "reason": string(reason)})
		return
	}
	updated, err := h.Store.ActivateDKIMKey(r.Context(), actor, appID, domain, key, switchActive)
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, http.StatusOK, updated)
}
