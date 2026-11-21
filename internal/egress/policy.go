// Package egress holds the destination policy and the SMTP source dialer.
// Policy is the control plane's destination filter: it is applied when a
// recipient's route is resolved, and it is the only place destinations are
// checked. SourceDialer is the Edge's dialer: it connects to the numeric
// address control authorized, from a source address the worker owns, and does
// not judge the destination. DNS resolution and MX authentication are separate
// responsibilities.
package egress

import (
	"errors"
	"net/netip"
)

// This deliberately conservative set denies special-purpose ranges, including
// globally reachable special anycasts and IPv6 translation/transition ranges.
// Review it against the IANA IPv4/IPv6 special-purpose registries at release.
var denied = prefixes([]string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.168.0.0/16", "192.175.48.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"2001::/23", "2001:db8::/32", "2002::/16", "2620:4f:8000::/48", "3fff::/20",
})
var globalV6 = netip.MustParsePrefix("2000::/3")

// MaxSources bounds the source addresses an Edge may report.
const MaxSources = 16

// Policy is the destination filter. It holds no source addresses: the
// Edge reports its own, and CheckSources vets them when it does. IPv4 is
// the only dialable family because every reported source is IPv4.
type Policy struct {
	local      []netip.Prefix
	exceptions map[exceptionKey]struct{}
}

// PublicSource reports whether address is a literal, unzoned public IPv4
// address, the only kind a worker may advertise as a source.
func PublicSource(address netip.Addr) bool {
	return address.IsValid() && address.Is4() && address.Zone() == "" && publicAddress(address)
}

// NewPolicy requires explicit infrastructure ranges, including public IPs
// owned by the operator. Populate these from deployment inventory; the static
// special-use list cannot identify organization-specific infrastructure.
// A route exception that names an Edge source is caught at Ready, by
// CheckSources, because sources are only known once the Edge reports them.
func NewPolicy(infrastructure []netip.Prefix, exceptions ...Exception) (*Policy, error) {
	if len(infrastructure) == 0 {
		return nil, errors.New("infrastructure deny ranges are required")
	}
	local := make([]netip.Prefix, 0, len(infrastructure))
	for _, prefix := range infrastructure {
		if !prefix.IsValid() || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" {
			return nil, errors.New("invalid infrastructure prefix")
		}
		local = append(local, prefix.Masked())
	}
	policy := &Policy{local: local}
	if len(exceptions) > maxExceptions {
		return nil, errors.New("too many SMTP route exceptions")
	}
	policy.exceptions = make(map[exceptionKey]struct{}, len(exceptions))
	for _, exception := range exceptions {
		key, ok := exception.key()
		if !ok || !policy.isExceptionAddress(key.address) {
			return nil, errors.New("invalid SMTP route exception")
		}
		if _, duplicate := policy.exceptions[key]; duplicate {
			return nil, errors.New("duplicate SMTP route exception")
		}
		policy.exceptions[key] = struct{}{}
	}
	return policy, nil
}
func prefixes(values []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		out = append(out, netip.MustParsePrefix(value))
	}
	return out
}

// Allowed requires a permitted IPv4 destination: IPv6 is never dialable
// because sources are IPv4 only, so routing never selects it.
func (p *Policy) Allowed(address netip.Addr) bool {
	if p == nil || len(p.local) == 0 || !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if !address.Is4() || !publicAddress(address) {
		return false
	}
	for _, prefix := range p.local {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

// AllowedFor permits a normally denied infrastructure destination only for a
// preapproved exact recipient, MX hostname and public IP. Special-use, source and unsupported-family addresses remain
// denied even when configured. Call this independently on Trusted and Edge.
func (p *Policy) AllowedFor(address netip.Addr, recipient, mx string) bool {
	if p.Allowed(address) {
		return true
	}
	if p == nil || !p.isExceptionAddress(address) {
		return false
	}
	key, ok := (Exception{Recipient: recipient, MX: mx, Address: address}).key()
	if !ok {
		return false
	}
	_, ok = p.exceptions[key]
	return ok
}

// CheckSources reports whether every address an Edge reports as a sending
// source is acceptable to this policy: each must lie inside an infrastructure
// range, so mail is never routed to it as a destination, and none may be named
// by a route exception. The Edge applies no destination policy, so this is the only check.
func (p *Policy) CheckSources(sources []netip.Addr) error {
	if p == nil || len(p.local) == 0 || len(sources) == 0 {
		return errors.New("SMTP source policy is not configured")
	}
	for _, source := range sources {
		if !PublicSource(source) {
			return errors.New("invalid SMTP source address")
		}
		inside := false
		for _, prefix := range p.local {
			if prefix.Contains(source) {
				inside = true
				break
			}
		}
		if !inside {
			return errors.New("SMTP source is outside the infrastructure ranges")
		}
		for key := range p.exceptions {
			if key.address == source {
				return errors.New("a route exception names an SMTP source")
			}
		}
	}
	return nil
}

func (p *Policy) isExceptionAddress(address netip.Addr) bool {
	if p == nil || !address.IsValid() || address.Zone() != "" || address.Is4In6() || !address.Is4() || !publicAddress(address) {
		return false
	}
	for _, prefix := range p.local {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func publicAddress(address netip.Addr) bool {
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	// Global unicast currently allocated for public IPv6 is in 2000::/3. This
	// also excludes NAT64, ULA, link-local, multicast and reserved ranges.
	if address.Is6() && !globalV6.Contains(address) {
		return false
	}
	for _, prefix := range denied {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
