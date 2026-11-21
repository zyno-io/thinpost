// Package controlbridge composes one trusted-initiated worker session. It does
// not enable submission, prepare MIME, configure networks, or deploy services.
package controlbridge

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/bounceaddress"
	"github.com/zyno-io/thinpost/internal/bounceingest"
	"github.com/zyno-io/thinpost/internal/controllease"
	"github.com/zyno-io/thinpost/internal/controlsession"
	"github.com/zyno-io/thinpost/internal/controltls"
	"github.com/zyno-io/thinpost/internal/dispatch"
	"github.com/zyno-io/thinpost/internal/egress"
	"github.com/zyno-io/thinpost/internal/feedback"
	"github.com/zyno-io/thinpost/internal/inbound"
	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/mailroute"
	"github.com/zyno-io/thinpost/internal/postgres"
	"google.golang.org/grpc"
)

type Objects interface {
	Get(context.Context, string, int64) ([]byte, error)
	PutMIME(context.Context, string, []byte) error
}
type Config struct {
	WorkerID, ControllerID, ControllerIdentity string
	JournalID                                  string
	Target, WorkerDNS, WorkerIdentity          string
	PrivateCA                                  *x509.CertPool
	ClientCertificate                          tls.Certificate
	Source                                     netip.Addr
	Infrastructure                             []netip.Prefix
	RouteExceptions                            []egress.Exception
	BounceDomain, ActiveBounceKey              string
	BounceKeys                                 map[string][]byte
	Recipients                                 dispatch.RecipientPolicy
	// Submission is optional: when set, the session advertises submission/1
	// and serves SMTP submissions from an edge that enables it.
	Submission                          controlsession.Submission
	LeaseTTL, ClockMargin, PollInterval time.Duration
}
type Bridge struct {
	runner     *controlsession.Runner
	connection *grpc.ClientConn
}

// New pins the worker transport to a literal private address while separately
// verifying its DNS and URI certificate identities. The controller private
// key signs only database-issued lease proofs. Call Close after Run exits.
func New(c Config, repository *postgres.Repository, objects Objects) (*Bridge, error) {
	if repository == nil || repository.DB == nil || objects == nil || c.Recipients == nil || c.WorkerID == "" || c.ControllerID == "" || c.ControllerIdentity == "" || !journal.ValidIdentity(c.JournalID) {
		return nil, errors.New("incomplete control bridge configuration")
	}
	host, port, err := net.SplitHostPort(c.Target)
	if err != nil || port == "" {
		return nil, errors.New("invalid edge target")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber <= 0 || portNumber > 65535 {
		return nil, errors.New("invalid edge target port")
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.Zone() != "" || !(address.IsPrivate() || address.IsLoopback()) {
		return nil, errors.New("edge target must be a literal private address")
	}
	signer, ok := c.ClientCertificate.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("control certificate key cannot sign leases")
	}
	if len(c.ClientCertificate.Certificate) == 0 {
		return nil, errors.New("missing control certificate")
	}
	leaf, err := x509.ParseCertificate(c.ClientCertificate.Certificate[0])
	if err != nil {
		return nil, errors.New("invalid control certificate")
	}
	matches := 0
	for _, identity := range leaf.URIs {
		if identity.String() == c.ControllerIdentity {
			matches++
		}
	}
	if matches != 1 {
		return nil, errors.New("controller identity does not match its certificate")
	}
	if c.LeaseTTL < 5*time.Second || c.LeaseTTL > 5*time.Minute || c.ClockMargin <= 0 || c.ClockMargin >= c.LeaseTTL/2 || c.PollInterval <= 0 {
		return nil, errors.New("invalid control session timing")
	}
	tlsConfig, err := controltls.Client(c.PrivateCA, c.ClientCertificate, c.WorkerDNS, c.WorkerIdentity)
	if err != nil {
		return nil, errors.New("invalid edge TLS configuration")
	}
	transport, err := controltls.ExpiringClientCredentials(tlsConfig, c.ClockMargin)
	if err != nil {
		return nil, errors.New("invalid edge TLS lifetime")
	}
	policy, err := egress.NewPolicy(c.Infrastructure, []netip.Addr{c.Source}, c.RouteExceptions...)
	if err != nil {
		return nil, errors.New("invalid destination policy")
	}
	keys := make(map[string][]byte, len(c.BounceKeys))
	for version, key := range c.BounceKeys {
		keys[version] = append([]byte(nil), key...)
	}
	envelopes := &bounceaddress.Service{Repository: repository, ActiveVersion: c.ActiveBounceKey, Keys: keys}
	if err = envelopes.Validate(); err != nil {
		return nil, errors.New("invalid bounce address configuration")
	}
	conn, err := grpc.NewClient("passthrough:///"+c.Target, grpc.WithTransportCredentials(transport), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(128<<10), grpc.MaxCallSendMsgSize(128<<10)))
	if err != nil {
		return nil, errors.New("edge client configuration failed")
	}
	work := &dispatch.Provider{Repository: repository, Objects: objects, Routes: &mailroute.Resolver{DNS: &mailroute.SystemDNS{}, Filter: policy}, Envelopes: envelopes, Recipients: c.Recipients}
	issuer := &controllease.Issuer{Repository: repository, Signer: signer, Identity: c.ControllerIdentity, JournalID: c.JournalID}
	runner := &controlsession.Runner{WorkerID: c.WorkerID, ControllerID: c.ControllerID, JournalID: c.JournalID, Issuer: issuer, Results: repository, Starts: repository, Reconciler: repository, Work: work, Bounces: &bounceingest.Service{Repository: repository, Objects: objects, Feedback: &feedback.Service{Repository: repository}},
		Inbound: &inbound.Service{Repository: repository, Objects: objects, Keys: inbound.FingerprintKeys(c.ActiveBounceKey, keys)}, LeaseTTL: c.LeaseTTL, ClockMargin: c.ClockMargin, PollInterval: c.PollInterval, Reports: repository, Sources: policy, Submission: c.Submission,
		Connect: func(ctx context.Context) (controlsession.Stream, error) {
			return controlv1.NewEdgeControlClient(conn).Connect(ctx)
		}}
	return &Bridge{runner: runner, connection: conn}, nil
}
func (b *Bridge) Run(ctx context.Context) error { return b.runner.Run(ctx) }
func (b *Bridge) RunDraining(ctx context.Context, drain <-chan struct{}) error {
	return b.runner.RunDraining(ctx, drain)
}
func (b *Bridge) Close() error { return b.connection.Close() }
