package edgeprotocol

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/netip"
	"slices"
	"strings"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
)

// CapInbound is the capability both sides advertise (Open.capabilities from
// control, Ready.capabilities from the worker) before any inbound frame is sent.
const CapInbound = "inbound/1"

const (
	// MaxInboundAssembleBytes is the largest body an assembler will hold. The
	// SMTP receiver limits inbound mail to MaxInboundBytes; control rejects
	// anything above that and the assembler's ceiling only bounds memory.
	MaxInboundAssembleBytes = 16 << 20
	// MaxInboundBytes is the inbound message size limit.
	MaxInboundBytes = 10 << 20
	// MaxInboundRecipients is the envelope recipient limit of one transaction.
	MaxInboundRecipients = 50
	// MaxInboundItems is how many inbound messages may be in flight per session.
	MaxInboundItems = 4
	// MaxRoutePageBytes bounds the estimated size of one routes page, below the
	// 128 KiB gRPC message limit.
	MaxRoutePageBytes = 96 << 10
	// MaxRoutes bounds one snapshot.
	MaxRoutes = 100000
	// MaxCapabilities and maxCapabilityLength bound a peer's capability list.
	MaxCapabilities = 8
)

// HasCapability reports whether a peer's list contains the capability.
func HasCapability(list []string, capability string) bool {
	return slices.Contains(list, capability)
}

// ValidCapabilities bounds a capability list received from a peer. Unknown
// capabilities are allowed (and ignored); malformed ones are not.
func ValidCapabilities(list []string) bool {
	if len(list) > MaxCapabilities {
		return false
	}
	for _, c := range list {
		if len(c) == 0 || len(c) > 32 {
			return false
		}
		for _, r := range c {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '/' || r == '-' || r == '_' || r == '.') {
				return false
			}
		}
	}
	return true
}

// ValidInboundID reports whether the value is a 32-character lowercase hex ID.
func ValidInboundID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ValidInboundFraming checks what the chunk protocol itself depends on: the ID,
// the size and the digest. A start that fails it is a protocol violation and
// ends the session; one that only fails ValidInboundStart (a bad envelope) is
// answered with a permanent rejection instead.
func ValidInboundFraming(start *controlv1.InboundStart) bool {
	return start != nil && ValidInboundID(start.InboundId) && start.ExpectedSize != 0 && start.ExpectedSize <= MaxInboundAssembleBytes && len(start.Sha256) == sha256.Size
}

// ValidInboundStart checks the frame's envelope bounds as well. The envelope's
// meaning is validated again by control's inbound package.
func ValidInboundStart(start *controlv1.InboundStart) bool {
	if !ValidInboundFraming(start) {
		return false
	}
	if len(start.RcptTo) == 0 || len(start.RcptTo) > MaxInboundRecipients || len(start.MailFrom) > 320 || len(start.Helo) > 512 || len(start.TlsVersion) > 64 || len(start.TlsCipher) > 128 || len(start.Receiver) > 255 {
		return false
	}
	for _, rcpt := range start.RcptTo {
		if len(rcpt) == 0 || len(rcpt) > 320 {
			return false
		}
	}
	if start.ClientIp != "" {
		if _, err := netip.ParseAddr(start.ClientIp); err != nil {
			return false
		}
	}
	return start.ReceivedUnixMs >= 0 && start.ReceivedUnixMs <= 253402300799999
}

// InboundAssembler collects one inbound message from ordered chunks and checks
// its size and SHA-256. A stream owner must discard it on any error.
type InboundAssembler struct {
	start *controlv1.InboundStart
	body  []byte
}

func NewInboundAssembler(start *controlv1.InboundStart) (*InboundAssembler, error) {
	if !ValidInboundFraming(start) {
		return nil, errors.New("invalid inbound start")
	}
	return &InboundAssembler{start: start, body: make([]byte, 0, int(start.ExpectedSize))}, nil
}

func (a *InboundAssembler) Start() *controlv1.InboundStart { return a.start }

// Add returns the assembled bytes once the final, hash-verified chunk arrives.
func (a *InboundAssembler) Add(chunk *controlv1.DataChunk) ([]byte, error) {
	size := a.start.ExpectedSize
	if a.body == nil || chunk == nil || chunk.Id != a.start.InboundId || chunk.Offset != uint64(len(a.body)) || len(chunk.Data) == 0 || len(chunk.Data) > MaxChunkBytes || uint64(len(chunk.Data)) > size-uint64(len(a.body)) {
		a.body = nil
		return nil, errors.New("invalid inbound chunk")
	}
	a.body = append(a.body, chunk.Data...)
	if chunk.Last != (uint64(len(a.body)) == size) {
		a.body = nil
		return nil, errors.New("invalid final inbound chunk")
	}
	if !chunk.Last {
		return nil, nil
	}
	digest := sha256.Sum256(a.body)
	if subtle.ConstantTimeCompare(digest[:], a.start.Sha256) != 1 {
		a.body = nil
		return nil, errors.New("inbound digest mismatch")
	}
	result := a.body
	a.body = nil
	return result, nil
}

// RoutePageSize estimates the encoded size of one route.
func RoutePageSize(route *controlv1.InboundRoute) int { return len(route.GetDomain()) + 12 }

// ValidRoute checks one snapshot entry: a normalized lowercase DNS name and a
// known state.
func ValidRoute(route *controlv1.InboundRoute) bool {
	if route == nil || route.Group == 0 || len(route.Domain) < 3 || len(route.Domain) > 253 || route.Domain != strings.ToLower(route.Domain) {
		return false
	}
	switch route.State {
	case controlv1.InboundRouteState_INBOUND_ROUTE_STATE_ACCEPT, controlv1.InboundRouteState_INBOUND_ROUTE_STATE_TEMPFAIL:
	default:
		return false
	}
	for _, label := range strings.Split(route.Domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// RoutesAssembler accumulates snapshot pages. A page with offset zero starts a
// new snapshot, discarding any partial one; later pages must continue exactly
// where the previous stopped. Add returns the complete snapshot on the last page.
type RoutesAssembler struct {
	routes []*controlv1.InboundRoute
	active bool
}

func (a *RoutesAssembler) Add(page *controlv1.InboundRoutes) ([]*controlv1.InboundRoute, bool, error) {
	if page == nil {
		return nil, false, errors.New("empty routes page")
	}
	if page.Offset == 0 {
		a.routes, a.active = nil, true
	}
	if !a.active || page.Offset != uint32(len(a.routes)) || len(a.routes)+len(page.Routes) > MaxRoutes {
		a.routes, a.active = nil, false
		return nil, false, errors.New("invalid routes page")
	}
	seen := make(map[string]struct{}, len(page.Routes))
	for _, route := range page.Routes {
		if !ValidRoute(route) {
			a.routes, a.active = nil, false
			return nil, false, errors.New("invalid route")
		}
		seen[route.Domain] = struct{}{}
	}
	if len(seen) != len(page.Routes) {
		a.routes, a.active = nil, false
		return nil, false, errors.New("duplicate route")
	}
	a.routes = append(a.routes, page.Routes...)
	if !page.Last {
		return nil, false, nil
	}
	complete := a.routes
	a.routes, a.active = nil, false
	// Duplicates across pages would let a later entry shadow an earlier one.
	all := make(map[string]struct{}, len(complete))
	for _, route := range complete {
		if _, dup := all[route.Domain]; dup {
			return nil, false, errors.New("duplicate route")
		}
		all[route.Domain] = struct{}{}
	}
	return complete, true, nil
}
