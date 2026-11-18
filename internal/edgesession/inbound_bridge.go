package edgesession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/bounce"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/journal"
)

// inboundBridge connects the SMTP receiver to the control stream for inbound
// mail. Like the bounce bridge it never opens a connection: only a
// trusted-initiated session that negotiated the capability and finished
// reconciliation can attach. It holds the routing snapshot control pushed and
// up to MaxInboundItems messages in memory, each until its acknowledgement.
type inboundBridge struct {
	mu      sync.Mutex
	enabled bool
	fence   journal.Fence
	active  *inboundConnection
	// routes is the last complete snapshot; haveSnapshot is false until one
	// arrives and again when a session without the capability attaches, so a
	// worker paired with an older control plane answers 550 as it always did.
	routes       map[string]snapshotRoute
	haveSnapshot bool
	// oldControl is set once a session without the capability has completed
	// reconciliation since this process started, and cleared when a capable
	// one does. Until it is set, a worker with no snapshot answers 451 for
	// non-bounce domains (a restart or reconnect must not bounce live mail);
	// afterwards it answers 550, as it always did.
	oldControl bool
	items      map[string]chan controlv1.InboundVerdict
}
type snapshotRoute struct {
	group  uint32
	accept bool
}
type inboundConnection struct {
	fence journal.Fence
	send  func(*controlv1.EdgeFrame) error
	valid func() bool
	done  chan struct{}
}

var _ bounce.Inbound = (*inboundBridge)(nil)

func (b *inboundBridge) isEnabled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.enabled
}

func (b *inboundBridge) activate(fence journal.Fence) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if fence.Epoch < b.fence.Epoch || fence.Epoch == b.fence.Epoch && b.fence.ControllerID != "" && fence.ControllerID != b.fence.ControllerID {
		return journal.ErrStaleController
	}
	if fence != b.fence {
		if b.active != nil {
			close(b.active.done)
			b.active = nil
		}
		b.fence = fence
	}
	return nil
}

// attach is called once a capable session finishes reconciliation.
func (b *inboundBridge) attach(fence journal.Fence, send func(*controlv1.EdgeFrame) error, valid func() bool) (*inboundConnection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fence != fence {
		return nil, journal.ErrStaleController
	}
	if b.active != nil {
		close(b.active.done)
	}
	c := &inboundConnection{fence: fence, send: send, valid: valid, done: make(chan struct{})}
	b.active = c
	b.items = make(map[string]chan controlv1.InboundVerdict)
	b.oldControl = false
	return c, nil
}

// withoutCapability is called when a session whose control plane lacks the
// capability finishes reconciliation: the old snapshot no longer describes what
// can be committed.
func (b *inboundBridge) withoutCapability() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.routes, b.haveSnapshot = nil, false
	b.oldControl = true
}

func (b *inboundBridge) detach(c *inboundConnection) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active == c {
		b.active = nil
		close(c.done)
	}
}

// setRoutes swaps the snapshot in atomically.
func (b *inboundBridge) setRoutes(c *inboundConnection, routes []*controlv1.InboundRoute) bool {
	next := make(map[string]snapshotRoute, len(routes))
	for _, route := range routes {
		next[route.Domain] = snapshotRoute{group: route.Group, accept: route.State == controlv1.InboundRouteState_INBOUND_ROUTE_STATE_ACCEPT}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != c || !c.valid() {
		return false
	}
	b.routes, b.haveSnapshot = next, true
	return true
}

// Lookup implements bounce.Inbound. A snapshot without a live, authorized
// session answers tempfail so senders queue, because nothing could be committed.
func (b *inboundBridge) Lookup(domain string) (uint32, bounce.RouteState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.enabled {
		return 0, bounce.RouteUnknown
	}
	if !b.haveSnapshot {
		if b.oldControl {
			return 0, bounce.RouteUnknown
		}
		// No snapshot yet (a fresh start or reconnect): which domains are live is
		// unknown, so queue the sender instead of refusing real mail.
		return 0, bounce.RouteTempfailNoSession
	}
	route, ok := b.routes[strings.ToLower(domain)]
	if !ok {
		return 0, bounce.RouteUnknown
	}
	if b.active == nil || !b.active.valid() {
		return route.group, bounce.RouteTempfailNoSession
	}
	if !route.accept {
		return route.group, bounce.RouteTempfailState
	}
	return route.group, bounce.RouteAccept
}

// acknowledge records control's verdict for an in-flight message. An unknown ID
// or a stale or unauthorized session is ignored.
func (b *inboundBridge) acknowledge(c *inboundConnection, ack *controlv1.InboundAcknowledgement) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != c || ack == nil || !c.valid() {
		return
	}
	ch := b.items[ack.InboundId]
	if ch == nil {
		return
	}
	select {
	case ch <- ack.Verdict:
	default:
	}
}

// Forward implements bounce.Inbound. It has no disk spool: a timeout, lost
// stream or unauthorized session returns an error so the SMTP peer retries (the
// keyed fingerprint makes a late commit and its retry one message).
func (b *inboundBridge) Forward(ctx context.Context, m bounce.Message) (bounce.Verdict, error) {
	if !edgeprotocol.ValidInboundID(m.ID) || len(m.Body) == 0 || len(m.Body) > edgeprotocol.MaxInboundBytes || len(m.Rcpts) == 0 || len(m.Rcpts) > edgeprotocol.MaxInboundRecipients {
		return 0, errors.New("invalid inbound message")
	}
	b.mu.Lock()
	c := b.active
	if !b.enabled || c == nil || !c.valid() || len(b.items) >= edgeprotocol.MaxInboundItems {
		b.mu.Unlock()
		return 0, errors.New("inbound stream unavailable")
	}
	if _, exists := b.items[m.ID]; exists {
		b.mu.Unlock()
		return 0, errors.New("duplicate inbound id")
	}
	ack := make(chan controlv1.InboundVerdict, 1)
	b.items[m.ID] = ack
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.items[m.ID] == ack {
			delete(b.items, m.ID)
		}
		b.mu.Unlock()
	}()
	if err := sendInbound(ctx, c, m); err != nil {
		return 0, err
	}
	select {
	case verdict := <-ack:
		if !c.valid() {
			return 0, errors.New("controller lease expired")
		}
		switch verdict {
		case controlv1.InboundVerdict_INBOUND_VERDICT_ACCEPTED:
			return bounce.VerdictAccepted, nil
		case controlv1.InboundVerdict_INBOUND_VERDICT_TEMPFAIL:
			return bounce.VerdictTempfail, nil
		case controlv1.InboundVerdict_INBOUND_VERDICT_REJECTED:
			return bounce.VerdictRejected, nil
		}
		return 0, errors.New("invalid inbound verdict")
	case <-c.done:
		return 0, errors.New("inbound session closed")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func sendInbound(ctx context.Context, c *inboundConnection, m bounce.Message) error {
	if !c.valid() {
		return errors.New("controller lease expired")
	}
	digest := sha256.Sum256(m.Body)
	start := &controlv1.InboundStart{InboundId: m.ID, ExpectedSize: uint64(len(m.Body)), Sha256: digest[:], MailFrom: m.MailFrom, RcptTo: m.Rcpts, Helo: m.Helo, TlsVersion: m.TLSVer, TlsCipher: m.TLSCiph, ReceivedUnixMs: m.Received.UnixMilli(), Receiver: m.Receiver, Group: m.Group}
	if m.ClientIP.IsValid() {
		start.ClientIp = m.ClientIP.String()
	}
	if err := c.send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Inbound{Inbound: start}}); err != nil {
		return err
	}
	for offset := 0; offset < len(m.Body); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return errors.New("inbound session closed")
		default:
		}
		if !c.valid() {
			return errors.New("controller lease expired")
		}
		end := min(offset+edgeprotocol.MaxChunkBytes, len(m.Body))
		chunk := &controlv1.DataChunk{Id: m.ID, Offset: uint64(offset), Data: bytes.Clone(m.Body[offset:end]), Last: end == len(m.Body)}
		if err := c.send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_InboundChunk{InboundChunk: chunk}}); err != nil {
			return err
		}
		offset = end
	}
	return nil
}
