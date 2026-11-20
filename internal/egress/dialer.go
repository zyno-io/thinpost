package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
)

type dialFunc func(context.Context, netip.Addr, netip.Addr) (net.Conn, error)

// SourceDialer connects to the numeric destination control authorized, on SMTP
// port 25, from one of the worker's own source addresses. It does not decide
// whether a destination is acceptable: control is the sole authority for the
// egress policy and applies Policy when it resolves a route. Network-level
// controls on the Edge (for example a firewall that allows only outbound TCP 25
// to public addresses) are the boundary against a compromised control plane.
type SourceDialer struct {
	source4, source6 netip.Addr
	sources          map[netip.Addr]struct{}
}

// NewSourceDialer requires at least one literal, unzoned public source
// address, at most MaxSources, with no duplicates. The first source of each
// family is its primary and the default for that family.
func NewSourceDialer(sources []netip.Addr) (*SourceDialer, error) {
	if len(sources) > MaxSources {
		return nil, errors.New("too many SMTP source addresses")
	}
	d := &SourceDialer{sources: make(map[netip.Addr]struct{}, len(sources))}
	for _, source := range sources {
		if !source.IsValid() || source.Zone() != "" || source.Is4In6() || !publicAddress(source) {
			return nil, errors.New("invalid SMTP source address")
		}
		if _, duplicate := d.sources[source]; duplicate {
			return nil, errors.New("duplicate SMTP source")
		}
		d.sources[source] = struct{}{}
		if source.Is4() && !d.source4.IsValid() {
			d.source4 = source
		} else if source.Is6() && !d.source6.IsValid() {
			d.source6 = source
		}
	}
	if !d.source4.IsValid() && !d.source6.IsValid() {
		return nil, errors.New("SMTP source address is required")
	}
	return d, nil
}

// Dial connects only to the supplied numeric address on SMTP port 25. The MX
// name must be retained separately for TLS validation. A zero source means the
// primary address of the destination's family; otherwise source must be one of
// the dialer's sources, of the destination's family, and is never replaced by
// another address. recipient and mx are unused: they are kept so the dialer
// satisfies the worker's interface.
func (d *SourceDialer) Dial(ctx context.Context, source, address netip.Addr, recipient, mx string) (net.Conn, error) {
	return d.dial(ctx, source, address, func(ctx context.Context, source, destination netip.Addr) (net.Conn, error) {
		return dialTCP(ctx, source, destination, "25")
	})
}

// dialTCP binds the local end to source before connecting. A bind failure
// (for example an address the host does not own) fails the dial; the kernel
// never picks a different source.
func dialTCP(ctx context.Context, source, destination netip.Addr, port string) (net.Conn, error) {
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IP(source.AsSlice())}}
	return dialer.DialContext(ctx, "tcp", net.JoinHostPort(destination.String(), port))
}

func (d *SourceDialer) dial(ctx context.Context, source, address netip.Addr, dial dialFunc) (net.Conn, error) {
	if !address.IsValid() || address.Zone() != "" {
		return nil, errors.New("invalid SMTP destination")
	}
	address = address.Unmap()
	if !source.IsValid() {
		source = d.source6
		if address.Is4() {
			source = d.source4
		}
		if !source.IsValid() {
			return nil, errors.New("no SMTP source for destination address family")
		}
	} else if _, ok := d.sources[source]; !ok || source.Is4() != address.Is4() {
		return nil, errors.New("SMTP source is not allowed for destination")
	}
	return dial(ctx, source, address)
}
