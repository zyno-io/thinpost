package admin

import (
	"net/url"
	"slices"
	"strings"

	"github.com/zyno-io/thinpost/internal/webhook/format"
)

const maxWebhookEndpointBytes = 2048

// WebhookOutcomes lists every outcome a webhook event can carry.
var WebhookOutcomes = format.Outcomes

// ValidWebhookEventNames accepts a non-empty, unique list of at most 32 names
// that some format defines. The Store narrows it to the endpoint's own format.
func ValidWebhookEventNames(events []string) bool {
	if len(events) == 0 || len(events) > 32 {
		return false
	}
	for i, event := range events {
		known := false
		for _, name := range format.Names() {
			if f, _ := format.Lookup(name); slices.Contains(f.EventTypes(), event) {
				known = true
			}
		}
		if !known || slices.Contains(events[:i], event) {
			return false
		}
	}
	return true
}

// internalSuffixes are names that resolve inside a cluster or host, never on
// the public internet. The dial-time address check and egress policy also
// block these; rejecting them here gives the admin a clear error.
var internalSuffixes = []string{"localhost", "local", "internal", "svc", "home.arpa"}

func internalHost(host string) bool {
	for _, suffix := range internalSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// numericLabel reports a final label that some resolvers parse as an IPv4
// shorthand (127.1, 0x7f.1), per the WHATWG "ends in a number" rule.
func numericLabel(label string) bool {
	digits := strings.TrimPrefix(strings.TrimPrefix(label, "0x"), "0X")
	if digits == "" {
		return label != ""
	}
	hex := digits != label
	for _, c := range digits {
		if !('0' <= c && c <= '9' || hex && ('a' <= c && c <= 'f' || 'A' <= c && c <= 'F')) {
			return false
		}
	}
	return true
}

func validEscapes(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '%' {
			if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
				return false
			}
			i += 2
		}
	}
	return true
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// NormalizeWebhookEndpoint returns the canonical HTTPS endpoint: lowercase DNS
// host, port 443 implied, query preserved. It rejects userinfo, fragments,
// non-ASCII input, IP literals and shorthands, internal names, single-label
// hosts and other ports.
func NormalizeWebhookEndpoint(raw string) (string, bool) {
	if len(raw) == 0 || len(raw) > maxWebhookEndpointBytes || strings.ContainsRune(raw, '\\') || strings.ContainsRune(raw, '#') {
		return "", false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= ' ' || raw[i] >= 0x7f {
			return "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" || (u.ForceQuery && u.RawQuery == "") {
		return "", false
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", false
	}
	host, ok := NormalizeDomain(u.Hostname())
	if !ok || numericLabel(host[strings.LastIndexByte(host, '.')+1:]) || internalHost(host) || !validEscapes(u.RawQuery) {
		return "", false
	}
	endpoint := "https://" + host + u.EscapedPath()
	if u.RawQuery != "" {
		endpoint += "?" + u.RawQuery
	}
	return endpoint, len(endpoint) <= maxWebhookEndpointBytes
}
