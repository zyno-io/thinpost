// Package inbound receives mail for an app's receiving domains in the trusted
// plane. The Edge hands over the raw message and the SMTP envelope; this package
// resolves the recipients' domains to one app, de-duplicates retries, parses
// the message within bounds, reports SPF, DKIM and DMARC results (it never
// enforces them), correlates replies to the app's outbound mail, stores the raw
// MIME encrypted and commits one database transaction. The Edge answers 250
// only after the commit.
package inbound

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/oplog"
)

// Verdict is control's answer to the Edge for one message.
type Verdict uint8

const (
	// Tempfail: nothing was committed; the sender should retry (451).
	Tempfail Verdict = iota
	// Accepted: committed, or an exact duplicate of a committed message (250).
	Accepted
	// Rejected: refused permanently (554).
	Rejected
)

// RouteStatus is the routing decision for a message's recipient domains.
type RouteStatus uint8

const (
	// Unroutable: a domain is unknown or not active, the domains belong to more
	// than one app, the app is paused or the app has no enabled inbound endpoint.
	Unroutable RouteStatus = iota
	// Routed: every domain belongs to one app that can receive.
	Routed
	// Backlog: routed, but the app's pending inbound webhook deliveries are at
	// their limit.
	Backlog
)

// Resolution is the routing decision for a set of recipient domains.
type Resolution struct {
	AppID  string
	Status RouteStatus
}

// Check is the cheap pre-upload look at a message's fingerprint and limits. It
// is only a hint: the commit decides.
type Check uint8

const (
	CheckNew Check = iota
	CheckDuplicate
	CheckFull
)

// CommitStatus is the outcome of the commit transaction.
type CommitStatus uint8

const (
	// CommitTempfailRouting: routing changed since the check (a domain is gone or
	// disabled, the app is paused or has no endpoint, or domains now span apps).
	CommitTempfailRouting CommitStatus = iota
	// CommitAccepted: the message and its events were committed.
	CommitAccepted
	// CommitDuplicate: an identical message was already committed; nothing new.
	CommitDuplicate
	// CommitTempfailLimit: the app's daily or pending limit is reached.
	CommitTempfailLimit
	// CommitRejected: the database refused the record deterministically (a value
	// out of range, a constraint that can never hold). Retrying cannot help, so
	// the sender gets a permanent answer instead of a 451 loop.
	CommitRejected
)

// Route is one receiving domain in the routing snapshot.
type Route struct {
	Domain string
	AppID  string
	Accept bool
}

// RouteSet is what the Edge is told. Open is false while the runtime gate is
// closed; every route must then be reported as tempfail.
type RouteSet struct {
	Open   bool
	Routes []Route
}

// Record is everything CommitInbound writes. The commit re-resolves Domains and
// ignores AppID if they no longer map to it.
type Record struct {
	ID, ObjectKey string
	AppID         string
	// Domains is every distinct recipient domain; Domain is the first recipient's.
	Domains      []string
	Domain       string
	Fingerprints [][32]byte
	Size         int
	MailFrom     string
	HeaderFrom   string
	Subject      string
	MessageID    string
	Auth         Authentication
	Correlation  *Correlation
	ClientIP     netip.Addr
	Helo         string
	TLSVersion   string
	TLSCipher    string
	Receiver     string
	Recipients   []string
}

// Repository is the durable side. Every method runs under the open runtime gate
// except InboundRoutes, and returns an error while it is closed (the caller
// answers tempfail).
type Repository interface {
	// ResolveInbound maps recipient domains to the one app that owns them.
	ResolveInbound(ctx context.Context, domains []string) (Resolution, error)
	// CheckInbound reports whether a message with any of the fingerprints was
	// already committed for the app, or the daily cap would refuse it now (the
	// base cap, or twice that when correlated is set).
	CheckInbound(ctx context.Context, appID string, fingerprints [][32]byte, correlated bool) (Check, error)
	// CountInbound adds one to the app's daily counter for a refusal or duplicate
	// decided before the commit. Best effort.
	CountInbound(ctx context.Context, appID, result string) error
	// ReplyCandidates returns, for the app only, the deliveries each identifier
	// can refer to (see Candidate).
	ReplyCandidates(ctx context.Context, appID string, ids []string) ([]Candidate, error)
	// CommitInbound runs the commit transaction described on Record.
	CommitInbound(ctx context.Context, record Record) (CommitStatus, error)
	// InboundRoutes builds the routing snapshot.
	InboundRoutes(ctx context.Context) (RouteSet, error)
}

// Objects stores the raw message encrypted. PutMIME must not overwrite.
type Objects interface {
	PutMIME(ctx context.Context, key string, data []byte) error
}

type Service struct {
	Repository Repository
	Objects    Objects
	// Keys are the fingerprint keys; the first signs new records and all are
	// consulted for duplicates.
	Keys [][]byte
	// Resolver is the DNS for authentication; nil uses the system resolver.
	Resolver Resolver
	// AuthBudget overrides AuthBudget (tests).
	AuthBudget time.Duration
	// Now overrides the clock used for object keys (tests).
	Now func() time.Time
	// ReceiveTimeout overrides ReceiveTimeout (tests).
	ReceiveTimeout time.Duration
}

func (s *Service) valid() bool {
	return s.Repository != nil && s.Objects != nil && len(s.Keys) > 0
}

func (s *Service) resolver() Resolver {
	if s.Resolver != nil {
		return s.Resolver
	}
	return net.DefaultResolver
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Receive handles one message. It returns Accepted only after the commit, and
// never returns an error: anything it cannot confirm is Tempfail, so the Edge
// answers 451 and the sender retries. Logs carry IDs and fixed values only.
func (s *Service) Receive(ctx context.Context, m Message) Verdict {
	verdict, result, appID := s.receive(ctx, m)
	oplog.Inbound(result, appID, m.ID)
	return verdict
}

// ReceiveTimeout bounds one message from routing to commit, covering the
// upload and the database. It is shorter than the worker's 60 second wait, so
// control has given up (and rolled back) before the worker answers 451.
const ReceiveTimeout = 45 * time.Second

// count records a refusal that happens before the commit transaction, best
// effort and bounded: the counters are for operations, never for correctness.
func (s *Service) count(appID, result string) {
	if appID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Repository.CountInbound(ctx, appID, result)
}

func (s *Service) receive(ctx context.Context, m Message) (Verdict, string, string) {
	if !s.valid() {
		return Tempfail, "tempfail_internal", ""
	}
	if len(m.Body) == 0 || len(m.Body) > MaxBytes {
		return Rejected, "rejected", ""
	}
	e, err := normalize(m)
	if err != nil {
		return Rejected, "rejected", ""
	}
	timeout := s.ReceiveTimeout
	if timeout <= 0 {
		timeout = ReceiveTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resolution, err := s.Repository.ResolveInbound(ctx, e.domains)
	if err != nil {
		return Tempfail, "tempfail_internal", ""
	}
	appID := resolution.AppID
	if resolution.Status != Routed {
		result := "tempfail_routing"
		if resolution.Status == Backlog {
			result = "tempfail_limit"
		}
		if appID == "" {
			return Tempfail, result, ""
		}
		// A retry of a message that was already committed is still a duplicate,
		// whatever the app's state is now: answer 250 so the sender stops.
		check, err := s.Repository.CheckInbound(ctx, appID, Fingerprints(s.Keys, appID, e, m.Body), false)
		if err == nil && check == CheckDuplicate {
			s.count(appID, "duplicate")
			return Accepted, "duplicate", appID
		}
		s.count(appID, result)
		return Tempfail, result, appID
	}
	fingerprints := Fingerprints(s.Keys, appID, e, m.Body)

	// Parse and correlate before uploading anything, so the pre-upload check
	// applies the real daily cap (base for uncorrelated mail, twice that for
	// replies) and an over-cap message is never stored.
	parsed := Parse(m.Body)
	correlation, err := Correlate(ctx, s.Repository, appID, parsed)
	if err != nil {
		// A lookup failure must not silently drop the correlation that exempts
		// the message from the daily cap: retry.
		return Tempfail, "tempfail_internal", appID
	}
	check, err := s.Repository.CheckInbound(ctx, appID, fingerprints, correlation != nil)
	if err != nil {
		return Tempfail, "tempfail_internal", appID
	}
	switch check {
	case CheckDuplicate:
		s.count(appID, "duplicate")
		return Accepted, "duplicate", appID
	case CheckFull:
		s.count(appID, "tempfail_limit")
		return Tempfail, "tempfail_limit", appID
	}

	// The stored ID is the worker's transfer ID, so logs on both sides join.
	id := e.id
	key, err := objectkey.New("inbound", id, s.now())
	if err != nil {
		return Tempfail, "tempfail_internal", appID
	}
	// The upload and the authentication checks run in parallel; the commit needs
	// both. Authentication is bounded by the DNS budget and never fails the message.
	var wg sync.WaitGroup
	var putErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		putErr = s.Objects.PutMIME(ctx, key, m.Body)
	}()
	auth := Authenticate(ctx, s.resolver(), s.AuthBudget, e, m.Body, parsed)
	wg.Wait()
	if putErr != nil || ctx.Err() != nil {
		s.count(appID, "tempfail_internal")
		return Tempfail, "tempfail_internal", appID
	}

	record := Record{
		ID: id, ObjectKey: key, AppID: appID, Domains: e.domains, Domain: addressDomain(e.rcpts[0]), Fingerprints: fingerprints,
		Size: len(m.Body), MailFrom: e.mailFrom, HeaderFrom: parsed.FromAddress, Subject: parsed.Subject, MessageID: parsed.MessageID,
		Auth: auth, Correlation: correlation, ClientIP: e.clientIP, Helo: e.helo, TLSVersion: e.tlsVer, TLSCipher: e.tlsCiph,
		Receiver: e.receiver, Recipients: e.rcpts,
	}
	status, err := s.Repository.CommitInbound(ctx, record)
	if err != nil {
		return Tempfail, "tempfail_internal", appID
	}
	switch status {
	case CommitRejected:
		return Rejected, "rejected", appID
	case CommitAccepted:
		if correlation != nil {
			return Accepted, "correlated", appID
		}
		return Accepted, "accepted", appID
	case CommitDuplicate:
		return Accepted, "duplicate", appID
	case CommitTempfailLimit:
		return Tempfail, "tempfail_limit", appID
	}
	return Tempfail, "tempfail_routing", appID
}

// Routes builds the routing snapshot the Edge uses at RCPT. While the runtime
// gate is closed every domain is tempfail.
func (s *Service) Routes(ctx context.Context) ([]Route, error) {
	if s.Repository == nil {
		return nil, errors.New("inbound unavailable")
	}
	set, err := s.Repository.InboundRoutes(ctx)
	if err != nil {
		return nil, err
	}
	routes := set.Routes
	if !set.Open {
		routes = append([]Route(nil), routes...)
		for i := range routes {
			routes[i].Accept = false
		}
	}
	return routes, nil
}
