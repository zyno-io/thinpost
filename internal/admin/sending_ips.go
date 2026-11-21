package admin

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// A sending IP is one outbound address of the Edge. Its identity (address and
// HELO name, which is also its bounce domain) is set in the Edge's own
// configuration and mirrored here from what the Edge reports when it connects;
// nothing in the console or API creates, edits or deletes one. The only thing
// a user chooses is which IP an app sends from. IPs are shown by address and
// HELO name. Logs name an IP by its HELO name, never its address.

// SendingIP is one row plus what is known about it right now.
type SendingIP struct {
	ID        string    `json:"id"`
	Address   string    `json:"address"`
	HeloName  string    `json:"helo_name"`
	CreatedAt time.Time `json:"created_at"`
	// AppCount is the apps pinned to this IP. For the main IP, apps with no
	// choice send from it too (see SendingIPList.UnpinnedAppCount).
	AppCount int `json:"app_count"`
	// IsPrimary is true when the Edge's last report lists this address first.
	IsPrimary bool `json:"is_primary"`
	// OnEdge is nil when the Edge has not reported its addresses (no report
	// yet, or a worker that reported none): unknown, not false. False means
	// the Edge reported addresses and this one is not among them.
	OnEdge *bool `json:"on_edge"`
}

// SendingIPList is the pool and what the Edge reported about it.
type SendingIPList struct {
	IPs []SendingIP `json:"ips"`
	// UnpinnedAppCount is the apps with no choice: they use the main IP.
	UnpinnedAppCount int `json:"unpinned_app_count"`
	// EdgeReportedAt is when the Edge last reported, nil if never.
	EdgeReportedAt *time.Time `json:"edge_reported_at"`
	// EdgeReportsIdentity is true when the last report listed addresses with
	// their names. Without it the Edge is given no mail to send.
	EdgeReportsIdentity bool `json:"edge_reports_identity"`
	// EdgePrimaryAddress is the address the Edge reported first (the main IP);
	// nil when it reported none.
	EdgePrimaryAddress *string `json:"edge_primary_address"`
}

// AppSendingIP is an app's choice. A nil ID means no choice: the app sends
// from the Edge's main IP.
type AppSendingIP struct {
	SendingIPID *string `json:"sending_ip_id"`
	Address     *string `json:"address"`
	HeloName    *string `json:"helo_name"`
}

// SendingIPStore is optional on the Store. The pool is read-only here (the
// Edge's report writes it). SetAppSendingIP and CreateAppWithSendingIP return
// ErrConflict for an IP the Edge is not reporting right now; choosing none is
// always allowed. A change is audited in the same transaction and an identical
// request changes and audits nothing.
type SendingIPStore interface {
	ListSendingIPs(ctx context.Context) (SendingIPList, error)
	GetSendingIP(ctx context.Context, id string) (SendingIP, error)
	GetAppSendingIP(ctx context.Context, appID string) (AppSendingIP, error)
	SetAppSendingIP(ctx context.Context, actor, appID string, sendingIPID *string) (AppSendingIP, error)
	CreateAppWithSendingIP(ctx context.Context, actor, name string, sendingIPID *string) (App, error)
}

// ReverseResolver looks up reverse, forward and mail-exchanger DNS for the
// advisory check.
type ReverseResolver interface {
	LookupAddr(ctx context.Context, addr string) ([]string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

// DNSCheckStatus values.
const (
	DNSOK          = "ok"
	DNSMismatch    = "mismatch"
	DNSMissing     = "missing"
	DNSUnavailable = "unavailable"
)

// DNSCheck is advisory. It reads public DNS only and stores and logs nothing.
// Every check is about the HELO name, which is also the bounce domain.
type DNSCheck struct {
	// Reverse: the address's PTR records include the HELO name.
	Reverse      string   `json:"reverse"`
	ReverseNames []string `json:"reverse_names"`
	// Forward: the HELO name resolves back to the address.
	Forward string `json:"forward"`
	// MX: the HELO name's mail exchangers reach the address, so bounces for
	// mail sent from it arrive. A name with no MX record counts as its own
	// exchanger (RFC 5321).
	MX string `json:"mx"`
	// SPF: the HELO name's SPF record covers the address through an ip4:
	// entry or an a mechanism. Includes and redirects are not followed.
	SPF string `json:"spf"`
}

const (
	maxReverseNames = 8
	maxMXHosts      = 4
)

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

// resolvesTo reports whether any of hosts is address.
func resolvesTo(hosts []string, address netip.Addr) bool {
	for _, host := range hosts {
		if parsed, err := netip.ParseAddr(host); err == nil && parsed.Unmap() == address {
			return true
		}
	}
	return false
}

// checkSendingIPDNS runs the lookups under one deadline.
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
	hosts, hostErr := reverse.LookupHost(ctx, ip.HeloName)
	out.Forward = dnsStatus(hostErr, len(hosts))
	if out.Forward == DNSOK && !resolvesTo(hosts, address) {
		out.Forward = DNSMismatch
	}
	out.MX = checkMX(ctx, reverse, ip.HeloName, address, hosts, hostErr)
	records, err := txt.LookupTXT(ctx, ip.HeloName)
	out.SPF = dnsStatus(err, countSPF(records))
	if out.SPF == DNSOK {
		out.SPF = DNSMismatch
		for _, record := range records {
			if spfListsAddress(record, address) || spfAMatches(ctx, reverse, record, ip.HeloName, address) {
				out.SPF = DNSOK
			}
		}
	}
	return out
}

// checkMX resolves the name's mail exchangers (at most maxMXHosts) and reports
// whether one is the address. With no MX record the name itself is the
// exchanger, so its own A lookup (already done as the forward check) decides.
func checkMX(ctx context.Context, resolver ReverseResolver, name string, address netip.Addr, ownHosts []string, ownErr error) string {
	records, err := resolver.LookupMX(ctx, name)
	if dnsErr, ok := err.(*net.DNSError); err != nil && !(ok && dnsErr.IsNotFound) {
		return DNSUnavailable
	}
	if len(records) == 0 {
		status := dnsStatus(ownErr, len(ownHosts))
		if status == DNSOK && !resolvesTo(ownHosts, address) {
			return DNSMismatch
		}
		return status
	}
	unavailable := false
	for i, record := range records {
		if i == maxMXHosts {
			break
		}
		target := strings.TrimSuffix(strings.ToLower(record.Host), ".")
		if target == "" || target == "." {
			continue
		}
		hosts, err := resolver.LookupHost(ctx, target)
		if err != nil {
			if dnsErr, ok := err.(*net.DNSError); !ok || !dnsErr.IsNotFound {
				unavailable = true
			}
			continue
		}
		if resolvesTo(hosts, address) {
			return DNSOK
		}
	}
	if unavailable {
		return DNSUnavailable
	}
	return DNSMismatch
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

// sendingIPRoute serves /api/v1/sending-ips and its sub-resources. The pool is
// read-only: only listing and the DNS check exist. It reports false for any
// other path.
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
		if r.Method != http.MethodGet {
			method(w, "GET")
			return true
		}
		list, err := store.ListSendingIPs(r.Context())
		if err != nil {
			storeError(w, err)
			return true
		}
		if list.IPs == nil {
			list.IPs = []SendingIP{}
		}
		respond(w, http.StatusOK, list)
	case len(parts) == 1 && opaqueID(parts[0]):
		// A row cannot be read singly, edited or deleted.
		method(w, "")
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
		// The field is required: an explicit null means "back to the main IP".
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
