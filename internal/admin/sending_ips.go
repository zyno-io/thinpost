package admin

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A sending IP names one outbound address of the Edge, with the HELO name the
// Edge greets with and the bounce domain whose SPF record must list the
// address. The Edge's first declared address (its primary) is every app's
// default; a row for it only names it. Names are logged, addresses never are.

// SendingIPNamePattern is the log-safe slug a sending IP name must match.
var SendingIPNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// SendingIP is one row plus what is known about it right now.
type SendingIP struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Address      string    `json:"address"`
	HeloName     string    `json:"helo_name"`
	BounceDomain string    `json:"bounce_domain"`
	CreatedAt    time.Time `json:"created_at"`
	// AppCount is the apps pinned to this IP. For the primary's row the apps
	// with no assignment send from it too (see SendingIPList.UnpinnedAppCount).
	AppCount int `json:"app_count"`
	// IsPrimary is true when the Edge's last report lists this address first.
	IsPrimary bool `json:"is_primary"`
	// OnEdge and BounceOnEdge are nil when the Edge's last Ready reported no
	// sources (an older Edge, or no report yet): they are unknown, not false.
	OnEdge       *bool `json:"on_edge"`
	BounceOnEdge *bool `json:"bounce_on_edge"`
}

// SendingIPList is the pool and what the Edge reported about it.
type SendingIPList struct {
	IPs []SendingIP `json:"ips"`
	// UnpinnedAppCount is the apps with no assignment: they use the primary.
	UnpinnedAppCount int `json:"unpinned_app_count"`
	// EdgeReportedAt is when the Edge last reported its sources, nil if never.
	EdgeReportedAt *time.Time `json:"edge_reported_at"`
	// EdgeReportsSources is false for an Edge that reports no sources.
	EdgeReportsSources bool `json:"edge_reports_sources"`
	// EdgePrimaryAddress is the address the Edge reported first (its primary,
	// every unassigned app's source); nil when it reported no sources.
	EdgePrimaryAddress *string `json:"edge_primary_address"`
	// UnnamedEdgeAddresses are reported addresses that have no row yet.
	UnnamedEdgeAddresses []string `json:"unnamed_edge_addresses"`
	// GlobalBounceDomain is the bounce domain the primary uses when it has no
	// row; empty when this replica does not know it.
	GlobalBounceDomain string `json:"global_bounce_domain,omitempty"`
}

// SendingIPInput creates a row; SendingIPPatch edits one. The address of a row
// never changes: a different address is a new row.
type SendingIPInput struct {
	Name, Address, HeloName, BounceDomain string
}
type SendingIPPatch struct {
	Name, HeloName, BounceDomain *string
}

// AppSendingIP is an app's assignment. A nil ID means no assignment: the app
// sends from the Edge's primary address.
type AppSendingIP struct {
	SendingIPID *string `json:"sending_ip_id"`
	Name        *string `json:"name"`
}

// SendingIPStore is optional on the Store. Delete returns ErrConflict while an
// app is assigned the IP. Every mutation is audited in its own transaction
// (system scope for the pool, app scope for an assignment) and an identical
// request changes and audits nothing.
type SendingIPStore interface {
	ListSendingIPs(ctx context.Context) (SendingIPList, error)
	GetSendingIP(ctx context.Context, id string) (SendingIP, error)
	CreateSendingIP(ctx context.Context, actor string, in SendingIPInput) (SendingIP, error)
	UpdateSendingIP(ctx context.Context, actor, id string, patch SendingIPPatch) (SendingIP, error)
	DeleteSendingIP(ctx context.Context, actor, id string) error
	GetAppSendingIP(ctx context.Context, appID string) (AppSendingIP, error)
	SetAppSendingIP(ctx context.Context, actor, appID string, sendingIPID *string) (AppSendingIP, error)
	CreateAppWithSendingIP(ctx context.Context, actor, name string, sendingIPID *string) (App, error)
}

// ReverseResolver looks up reverse and forward DNS for the advisory check.
type ReverseResolver interface {
	LookupAddr(ctx context.Context, addr string) ([]string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// ValidSendingIPName, ValidSendingIPAddress and ValidSendingIPDomain are the
// shared input rules; the store applies them again.
func ValidSendingIPName(name string) bool {
	return SendingIPNamePattern.MatchString(name) && !ReservedSendingIPNames[name]
}

// ReservedSendingIPNames are the words logs use for "no named IP" ("primary"), an
// invalid name ("other") and a deleted row ("removed"); a real name must not
// impersonate them.
var ReservedSendingIPNames = map[string]bool{"primary": true, "other": true, "removed": true}

// ValidSendingIPAddress accepts a canonical IPv4 literal that is not private,
// loopback, link-local or unspecified. The store applies the stricter
// egress.PublicSource rule (the same one the Edge and the Ready check use);
// admin cannot import egress.
func ValidSendingIPAddress(value string) bool {
	address, err := netip.ParseAddr(value)
	return err == nil && address.Is4() && address.String() == value && address.IsGlobalUnicast() && !address.IsPrivate() && !address.IsLoopback() && !address.IsLinkLocalUnicast()
}

// ValidSendingIPDomain accepts a normalized DNS name of at most max bytes.
func ValidSendingIPDomain(value string, max int) bool {
	normalized, ok := NormalizeDomain(value)
	return ok && normalized == value && len(value) >= 3 && len(value) <= max
}

// HeloUnderBounceDomain reports whether the HELO name is the bounce domain or a
// name beneath it. The Edge accepts bounces for the name each of its addresses
// greets with, so an address's bounce domain must be that name or a parent of
// it. Both arguments are already normalized.
func HeloUnderBounceDomain(helo, bounce string) bool {
	return helo == bounce || strings.HasSuffix(helo, "."+bounce)
}

// ValidSendingIPInput applies every field rule; the store calls it too.
func ValidSendingIPInput(in SendingIPInput) bool {
	return ValidSendingIPName(in.Name) && ValidSendingIPAddress(in.Address) && ValidSendingIPDomain(in.HeloName, 253) && ValidSendingIPDomain(in.BounceDomain, 219) && HeloUnderBounceDomain(in.HeloName, in.BounceDomain)
}

// DNSCheckStatus values.
const (
	DNSOK          = "ok"
	DNSMismatch    = "mismatch"
	DNSMissing     = "missing"
	DNSUnavailable = "unavailable"
)

// DNSCheck is advisory. It reads public DNS only and stores and logs nothing.
type DNSCheck struct {
	// Reverse: the address's PTR records include the HELO name.
	Reverse      string   `json:"reverse"`
	ReverseNames []string `json:"reverse_names"`
	// Forward: the HELO name resolves back to the address.
	Forward string `json:"forward"`
	// HeloSPF: the HELO name publishes an SPF record.
	HeloSPF string `json:"helo_spf"`
	// BounceSPF: the bounce domain's SPF record covers the address through an
	// ip4: entry or an a mechanism. Includes and redirects are not followed.
	BounceSPF string `json:"bounce_spf"`
}

const maxReverseNames = 8

func dnsStatus(err error, found int) string {
	if err != nil {
		if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
			return DNSMissing
		}
		return DNSUnavailable
	}
	if found == 0 {
		return DNSMissing
	}
	return DNSOK
}

// checkSendingIPDNS runs the four lookups under one deadline.
func checkSendingIPDNS(ctx context.Context, reverse ReverseResolver, txt TXTResolver, ip SendingIP) DNSCheck {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out := DNSCheck{ReverseNames: []string{}}
	address, _ := netip.ParseAddr(ip.Address)
	names, err := reverse.LookupAddr(ctx, ip.Address)
	for _, name := range names {
		name = strings.TrimSuffix(strings.ToLower(name), ".")
		if len(out.ReverseNames) < maxReverseNames {
			out.ReverseNames = append(out.ReverseNames, name)
		}
	}
	out.Reverse = dnsStatus(err, len(names))
	if out.Reverse == DNSOK {
		out.Reverse = DNSMismatch
		for _, name := range out.ReverseNames {
			if name == ip.HeloName {
				out.Reverse = DNSOK
			}
		}
	}
	hosts, err := reverse.LookupHost(ctx, ip.HeloName)
	out.Forward = dnsStatus(err, len(hosts))
	if out.Forward == DNSOK {
		out.Forward = DNSMismatch
		for _, host := range hosts {
			if parsed, perr := netip.ParseAddr(host); perr == nil && parsed.Unmap() == address {
				out.Forward = DNSOK
			}
		}
	}
	records, err := txt.LookupTXT(ctx, ip.HeloName)
	out.HeloSPF = dnsStatus(err, countSPF(records))
	records, err = txt.LookupTXT(ctx, ip.BounceDomain)
	out.BounceSPF = dnsStatus(err, countSPF(records))
	if out.BounceSPF == DNSOK {
		out.BounceSPF = DNSMismatch
		for _, record := range records {
			if spfListsAddress(record, address) || spfAMatches(ctx, reverse, record, ip.BounceDomain, address) {
				out.BounceSPF = DNSOK
			}
		}
	}
	return out
}

// maxSPFALookups bounds the a-mechanism lookups one check makes.
const maxSPFALookups = 2

// spfAMatches reports whether an a mechanism in an SPF record (bare a, a/cidr,
// a:name or a:name/cidr) covers address, resolving at most maxSPFALookups
// names. A bare a refers to domain, the name the record was published at.
func spfAMatches(ctx context.Context, resolver ReverseResolver, record, domain string, address netip.Addr) bool {
	fields := strings.Fields(strings.ToLower(record))
	if len(fields) == 0 || fields[0] != "v=spf1" || !address.Is4() {
		return false
	}
	lookups := 0
	for _, field := range fields[1:] {
		field = strings.TrimLeft(field, "+")
		if field != "a" && !strings.HasPrefix(field, "a:") && !strings.HasPrefix(field, "a/") {
			continue
		}
		target, bits := domain, 32
		rest := strings.TrimPrefix(field, "a")
		if name, ok := strings.CutPrefix(rest, ":"); ok {
			rest = ""
			if i := strings.IndexByte(name, '/'); i >= 0 {
				name, rest = name[:i], name[i:]
			}
			if name == "" || strings.Contains(name, "%") {
				continue
			}
			target = strings.TrimSuffix(name, ".")
		}
		if rest != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(rest, "/"))
			if !strings.HasPrefix(rest, "/") || err != nil || n < 0 || n > 32 {
				continue
			}
			bits = n
		}
		if lookups == maxSPFALookups {
			return false
		}
		lookups++
		hosts, err := resolver.LookupHost(ctx, target)
		if err != nil {
			continue
		}
		for _, host := range hosts {
			parsed, err := netip.ParseAddr(host)
			if err != nil || !parsed.Unmap().Is4() {
				continue
			}
			if prefix, err := parsed.Unmap().Prefix(bits); err == nil && prefix.Contains(address) {
				return true
			}
		}
	}
	return false
}

func countSPF(records []string) int {
	n := 0
	for _, record := range records {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(record)), "v=spf1") {
			n++
		}
	}
	return n
}

// spfListsAddress reports whether an SPF record has an ip4: entry (single
// address or CIDR) that contains address. It evaluates nothing else.
func spfListsAddress(record string, address netip.Addr) bool {
	fields := strings.Fields(strings.ToLower(record))
	if len(fields) == 0 || fields[0] != "v=spf1" {
		return false
	}
	for _, field := range fields[1:] {
		field = strings.TrimLeft(field, "+")
		value, ok := strings.CutPrefix(field, "ip4:")
		if !ok {
			continue
		}
		if prefix, err := netip.ParsePrefix(value); err == nil {
			if prefix.Contains(address) {
				return true
			}
		} else if single, err := netip.ParseAddr(value); err == nil && single == address {
			return true
		}
	}
	return false
}

// sendingIPRoute serves /api/v1/sending-ips and its sub-resources. It reports
// false for any other path.
func (h *Handler) sendingIPRoute(w http.ResponseWriter, r *http.Request, actor string) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/sending-ips")
	if !ok || (rest != "" && rest[0] != '/') {
		return false
	}
	store, ok := h.Store.(SendingIPStore)
	if !ok || r.URL.RawQuery != "" {
		fail(w, http.StatusServiceUnavailable)
		return true
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	switch {
	case rest == "" || rest == "/":
		switch r.Method {
		case http.MethodGet:
			list, err := store.ListSendingIPs(r.Context())
			if err != nil {
				storeError(w, err)
				return true
			}
			if list.IPs == nil {
				list.IPs = []SendingIP{}
			}
			if list.UnnamedEdgeAddresses == nil {
				list.UnnamedEdgeAddresses = []string{}
			}
			respond(w, http.StatusOK, list)
		case http.MethodPost:
			var req struct {
				Name         string `json:"name"`
				Address      string `json:"address"`
				HeloName     string `json:"helo_name"`
				BounceDomain string `json:"bounce_domain"`
			}
			in := SendingIPInput{}
			if decode(r, &req) != nil {
				fail(w, http.StatusBadRequest)
				return true
			}
			in = SendingIPInput{Name: req.Name, Address: req.Address, HeloName: req.HeloName, BounceDomain: req.BounceDomain}
			if !ValidSendingIPInput(in) {
				fail(w, http.StatusBadRequest)
				return true
			}
			created, err := store.CreateSendingIP(r.Context(), actor, in)
			if err != nil {
				storeError(w, err)
				return true
			}
			respond(w, http.StatusCreated, created)
		default:
			method(w, "GET, POST")
		}
	case len(parts) == 1 && opaqueID(parts[0]):
		switch r.Method {
		case http.MethodPatch:
			var req struct {
				Name         *string `json:"name"`
				HeloName     *string `json:"helo_name"`
				BounceDomain *string `json:"bounce_domain"`
			}
			if decode(r, &req) != nil || req.Name == nil && req.HeloName == nil && req.BounceDomain == nil ||
				req.Name != nil && !ValidSendingIPName(*req.Name) ||
				req.HeloName != nil && !ValidSendingIPDomain(*req.HeloName, 253) ||
				req.BounceDomain != nil && !ValidSendingIPDomain(*req.BounceDomain, 219) {
				fail(w, http.StatusBadRequest)
				return true
			}
			updated, err := store.UpdateSendingIP(r.Context(), actor, parts[0], SendingIPPatch{Name: req.Name, HeloName: req.HeloName, BounceDomain: req.BounceDomain})
			if err != nil {
				storeError(w, err)
				return true
			}
			respond(w, http.StatusOK, updated)
		case http.MethodDelete:
			if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
				fail(w, http.StatusBadRequest)
				return true
			}
			if err := store.DeleteSendingIP(r.Context(), actor, parts[0]); err != nil {
				storeError(w, err)
				return true
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			method(w, "PATCH, DELETE")
		}
	case len(parts) == 2 && opaqueID(parts[0]) && parts[1] == "check-dns":
		if r.Method != http.MethodPost {
			method(w, "POST")
			return true
		}
		if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			fail(w, http.StatusBadRequest)
			return true
		}
		if h.ReverseDNS == nil || h.DNS == nil {
			fail(w, http.StatusServiceUnavailable)
			return true
		}
		ip, err := store.GetSendingIP(r.Context(), parts[0])
		if err != nil {
			storeError(w, err)
			return true
		}
		respond(w, http.StatusOK, checkSendingIPDNS(r.Context(), h.ReverseDNS, h.DNS, ip))
	default:
		http.NotFound(w, r)
	}
	return true
}

// appSendingIP serves /api/v1/apps/{appId}/sending-ip.
func (h *Handler) appSendingIP(w http.ResponseWriter, r *http.Request, actor, appID string) {
	store, ok := h.Store.(SendingIPStore)
	if !ok || r.URL.RawQuery != "" {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		current, err := store.GetAppSendingIP(r.Context(), appID)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, current)
	case http.MethodPut:
		// The field is required: an explicit null means "back to the primary".
		var raw map[string]any
		if decode(r, &raw) != nil || len(raw) != 1 {
			fail(w, http.StatusBadRequest)
			return
		}
		value, present := raw["sending_ip_id"]
		if !present {
			fail(w, http.StatusBadRequest)
			return
		}
		var sendingIPID *string
		if value != nil {
			text, isString := value.(string)
			if !isString || !opaqueID(text) {
				fail(w, http.StatusBadRequest)
				return
			}
			sendingIPID = &text
		}
		updated, err := store.SetAppSendingIP(r.Context(), actor, appID, sendingIPID)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, updated)
	default:
		method(w, "GET, PUT")
	}
}
