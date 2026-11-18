package admin

import (
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/zyno-io/thinpost/internal/webhook/format"
)

// InternalEndpointPermitted reports whether an endpoint that matched the
// internal destination list may be used by an endpoint of this format and
// authentication: only native outbound events (identifiers and outcomes, no
// message content) and no credentials, since the exception is plain in-cluster
// HTTP. Inbound formats carry message content and provider formats and any
// Basic or custom-header credential are outside the exception.
func InternalEndpointPermitted(formatName string, hasAuth bool) bool {
	return formatName == format.Thinpost && !hasAuth
}

// hasCredentials reports whether an auth value would send anything.
func (a *WebhookAuth) hasCredentials() bool {
	return a != nil && !a.IsZero() && (a.Username() != "" || a.Password() != "" || len(a.Headers()) > 0)
}

// internalHostSuffix is the cluster-internal DNS suffix every listed host must
// end in, after at least a service and a namespace label.
const internalHostSuffix = ".svc.cluster.local"

// Service CIDR bounds: wide enough for a cluster's Service network, too narrow
// to cover a whole private block or anything else.
const (
	minServiceCIDRBitsV4 = 12
	minServiceCIDRBitsV6 = 108
)

// metadataAddresses are cloud instance-metadata endpoints, never Service IPs.
var metadataAddresses = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("fd00:ec2::254"), netip.MustParseAddr("100.100.100.200"),
}

// WebhookDestinations is the operator's narrow exception to the public-HTTPS
// webhook destination rules (THINPOST_WEBHOOK_INTERNAL_DESTINATIONS and
// THINPOST_SERVICE_CIDRS). A URL on the exception list, matched exactly after
// normalization, may use http or https, any port and a cluster-internal name;
// its resolved address must then lie inside a configured Service CIDR. The
// nil value means no exceptions, which is the default and leaves
// NormalizeWebhookEndpoint's behavior unchanged. It is immutable after
// ParseWebhookDestinations and safe for concurrent use.
type WebhookDestinations struct {
	exact map[string]struct{}
	cidrs []netip.Prefix
}

// ParseWebhookDestinations builds the policy from the two comma-separated
// settings. Both empty yields nil (no exceptions). Destinations without
// Service CIDRs, an invalid entry or an over-broad CIDR is an error, so a
// misconfigured control process fails closed at startup rather than running
// with an exception that cannot be constrained.
func ParseWebhookDestinations(destinations, serviceCIDRs string) (*WebhookDestinations, error) {
	var p WebhookDestinations
	for _, raw := range splitList(serviceCIDRs) {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix != prefix.Masked() || prefix.Addr().Zone() != "" || prefix.Addr().Is4In6() {
			return nil, errors.New("THINPOST_SERVICE_CIDRS must be a comma-separated list of canonical CIDRs")
		}
		if !privatePrefix(prefix) {
			return nil, errors.New("THINPOST_SERVICE_CIDRS entries must be wholly private address ranges")
		}
		if prefix.Addr().Is6() && prefix.Bits() < minServiceCIDRBitsV6 || prefix.Addr().Is4() && prefix.Bits() < minServiceCIDRBitsV4 {
			return nil, errors.New("THINPOST_SERVICE_CIDRS entries must be at least /12 (IPv4) or /108 (IPv6)")
		}
		for _, metadata := range metadataAddresses {
			if prefix.Contains(metadata) {
				return nil, errors.New("THINPOST_SERVICE_CIDRS entries must not contain a cloud metadata address")
			}
		}
		p.cidrs = append(p.cidrs, prefix)
	}
	if len(p.cidrs) > 8 {
		return nil, errors.New("THINPOST_SERVICE_CIDRS lists too many ranges")
	}
	urls := splitList(destinations)
	if len(urls) == 0 {
		if len(p.cidrs) == 0 {
			return nil, nil
		}
		return &p, nil
	}
	if len(p.cidrs) == 0 {
		return nil, errors.New("THINPOST_WEBHOOK_INTERNAL_DESTINATIONS requires THINPOST_SERVICE_CIDRS")
	}
	if len(urls) > 16 {
		return nil, errors.New("THINPOST_WEBHOOK_INTERNAL_DESTINATIONS lists too many URLs")
	}
	p.exact = make(map[string]struct{}, len(urls))
	for _, raw := range urls {
		canonical, ok := normalizeInternalWebhookURL(raw)
		if !ok || canonical != raw {
			return nil, errors.New("THINPOST_WEBHOOK_INTERNAL_DESTINATIONS entries must be canonical http(s) URLs with a DNS host name, no credentials or fragment, and no default port")
		}
		if host := hostOf(canonical); !strings.HasSuffix(host, internalHostSuffix) || !strings.Contains(strings.TrimSuffix(host, internalHostSuffix), ".") {
			return nil, errors.New("THINPOST_WEBHOOK_INTERNAL_DESTINATIONS hosts must be <service>.<namespace>.svc.cluster.local")
		}
		p.exact[canonical] = struct{}{}
	}
	return &p, nil
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// carrierGradeNAT is shared address space some clusters use for Services.
var carrierGradeNAT = netip.MustParsePrefix("100.64.0.0/10")

// privatePrefix reports whether every address of the prefix is private (RFC
// 1918, unique local or 100.64.0.0/10), by checking its first and last address;
// a prefix is contiguous and these ranges are aligned blocks. Loopback,
// link-local, multicast and public space are therefore never Service CIDRs.
func privatePrefix(prefix netip.Prefix) bool {
	if prefix.Bits() == 0 {
		return false
	}
	first := prefix.Addr()
	last := lastAddr(prefix)
	for _, a := range []netip.Addr{first, last} {
		if !a.IsPrivate() && !carrierGradeNAT.Contains(a) {
			return false
		}
	}
	return true
}

func lastAddr(prefix netip.Prefix) netip.Addr {
	b := prefix.Addr().AsSlice()
	for bit := prefix.Bits(); bit < len(b)*8; bit++ {
		b[bit/8] |= 0x80 >> (bit % 8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// AllowAddress reports whether a resolved address of an internal destination
// is inside a configured Service CIDR. Always false for the nil policy.
func (p *WebhookDestinations) AllowAddress(addr netip.Addr) bool {
	if p == nil || !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range p.cidrs {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// NormalizeEndpoint is NormalizeWebhookEndpoint extended with the exception
// list. internal is true when the endpoint matched an exception exactly (the
// whole normalized URL: scheme, host, port, path and query), which obliges the
// caller to dial under AllowAddress and the URL's own port; otherwise the public
// rules apply. A URL that is neither public-valid nor listed is rejected.
func (p *WebhookDestinations) NormalizeEndpoint(raw string) (endpoint string, internal, ok bool) {
	if p != nil && len(p.exact) > 0 {
		if canonical, valid := normalizeInternalWebhookURL(raw); valid {
			if _, listed := p.exact[canonical]; listed {
				return canonical, true, true
			}
		}
	}
	endpoint, ok = NormalizeWebhookEndpoint(raw)
	return endpoint, false, ok
}

// normalizeInternalWebhookURL canonicalizes a candidate exception URL: http or
// https, lowercase DNS host of two or more labels (never an IP literal), default
// port dropped, path and query preserved. Userinfo and fragments are rejected.
func normalizeInternalWebhookURL(raw string) (string, bool) {
	if len(raw) == 0 || len(raw) > maxWebhookEndpointBytes || strings.ContainsRune(raw, '\\') || strings.ContainsRune(raw, '#') || strings.ContainsRune(raw, ',') {
		return "", false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= ' ' || raw[i] >= 0x7f {
			return "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" || (u.ForceQuery && u.RawQuery == "") || !validEscapes(u.RawQuery) {
		return "", false
	}
	host, ok := NormalizeDomain(u.Hostname())
	if !ok {
		return "", false
	}
	port := u.Port()
	if port != "" {
		n := 0
		for _, c := range port {
			if c < '0' || c > '9' || n > 65535 {
				return "", false
			}
			n = n*10 + int(c-'0')
		}
		if n < 1 || n > 65535 || port != strconv.Itoa(n) {
			return "", false
		}
	}
	if port == "80" && u.Scheme == "http" || port == "443" && u.Scheme == "https" {
		port = ""
	}
	endpoint := u.Scheme + "://" + host
	if port != "" {
		endpoint += ":" + port
	}
	endpoint += u.EscapedPath()
	if u.RawQuery != "" {
		endpoint += "?" + u.RawQuery
	}
	return endpoint, len(endpoint) <= maxWebhookEndpointBytes
}

func hostOf(canonical string) string {
	u, err := url.Parse(canonical)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
