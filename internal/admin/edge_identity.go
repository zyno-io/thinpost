package admin

import (
	"context"
	"net/netip"
	"strings"
	"time"
)

// EdgeIdentity is what the sending worker (the Edge) last reported about
// itself: the HELO name of each of its IPs, main IP first. The Edge's own
// configuration is the single source of truth. Control keeps no copy in its
// settings; it only reads the report. The zero value means the worker has
// never reported.
type EdgeIdentity struct {
	// MainHostname and MainAddress are the HELO name and IPv4 address of the
	// main (first) IP.
	MainHostname, MainAddress string
	// Hostnames is every reported HELO name, main first.
	Hostnames []string
}

// EdgeIdentityStore reads the Edge's last report. EdgeIdentity returns the zero
// value, not an error, before the Edge has ever reported.
type EdgeIdentityStore interface {
	EdgeIdentity(ctx context.Context) (EdgeIdentity, error)
}

// Host name statuses shown with names that clients are told to use (the SMTP
// submission host and the inbound mail host).
const (
	// HostNameOK: the name is shown.
	HostNameOK = "ok"
	// HostNameNotReported: the sending worker has not reported its IPs yet.
	HostNameNotReported = "not_reported"
	// HostNameDNSUnconfirmed: public DNS does not (or could not be shown to)
	// map the name to the reported address, so the name is withheld.
	HostNameDNSUnconfirmed = "dns_unconfirmed"
)

const hostConfirmTimeout = 3 * time.Second

// edgeIdentity reads the report. A store without it reports no identity.
func (h *Handler) edgeIdentity(ctx context.Context) (EdgeIdentity, error) {
	store, ok := h.Store.(EdgeIdentityStore)
	if !ok {
		return EdgeIdentity{}, nil
	}
	return store.EdgeIdentity(ctx)
}

// confirmHostName reports whether public DNS ties host and address together in
// both directions. The Edge is not trusted: a name it reports becomes
// client-facing (told to people as the place to send mail), so it is only shown
// when the address's reverse DNS names the host and the host's own A/AAAA
// records include the address. Reverse DNS is set by whoever runs the address
// block, not by the Edge, so a compromised Edge can't get a domain it owns shown
// (and later repoint it). Anything short of a clear match (NXDOMAIN, SERVFAIL,
// timeout, no resolver) withholds the name. Lookups use the fully qualified
// name so the resolver's search domains never apply.
func (h *Handler) confirmHostName(ctx context.Context, host, address string) bool {
	want, err := netip.ParseAddr(address)
	if h.ReverseDNS == nil || host == "" || err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, hostConfirmTimeout)
	defer cancel()
	names, err := h.ReverseDNS.LookupAddr(ctx, want.Unmap().String())
	if err != nil {
		return false
	}
	named := false
	for _, name := range names {
		if strings.EqualFold(strings.TrimSuffix(name, "."), host) {
			named = true
			break
		}
	}
	if !named {
		return false
	}
	found, err := h.ReverseDNS.LookupHost(ctx, strings.TrimSuffix(host, ".")+".")
	if err != nil {
		return false
	}
	for _, value := range found {
		if got, perr := netip.ParseAddr(value); perr == nil && got.Unmap() == want.Unmap() {
			return true
		}
	}
	return false
}

// inboundMXHost is the host receiving domains point their MX record at: the
// main IP's HELO name, when public DNS confirms it. The status says why there
// is none.
func (h *Handler) inboundMXHost(ctx context.Context) (host, status string, err error) {
	identity, err := h.edgeIdentity(ctx)
	if err != nil {
		return "", "", err
	}
	if identity.MainHostname == "" {
		return "", HostNameNotReported, nil
	}
	if !h.confirmHostName(ctx, identity.MainHostname, identity.MainAddress) {
		return "", HostNameDNSUnconfirmed, nil
	}
	return identity.MainHostname, HostNameOK, nil
}
