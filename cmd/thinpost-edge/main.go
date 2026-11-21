// thinpost-edge is an experimental worker. Deployment, IP ownership fencing,
// certificate rotation, storage monitoring and operational readiness are not
// yet complete; do not use it for real mail until they are.
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/bounce"
	"github.com/zyno-io/thinpost/internal/edgeapp"
	"github.com/zyno-io/thinpost/internal/edgesession"
	"github.com/zyno-io/thinpost/internal/edgeworker"
	"github.com/zyno-io/thinpost/internal/egress"
	"github.com/zyno-io/thinpost/internal/envguard"
	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/leaseproof"
	"golang.org/x/net/netutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	if err := envguard.RefuseGRPCBinaryLog(os.Getenv); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return errors.New("edge worker must run as non-root")
	}
	if len(os.Args) > 1 {
		if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
			return probeHealth(healthAddress)
		}
		// Journal-only check for the liveness probe; see edgeHealthHandler.
		if len(os.Args) == 3 && os.Args[1] == "healthcheck" && os.Args[2] == "liveness" {
			return probeHealthPath(healthAddress, "/livez")
		}
		if len(os.Args) == 3 && os.Args[1] == "journal-id" && filepath.IsAbs(os.Args[2]) {
			return printJournalIdentity(os.Args[2], os.Stdout)
		}
		return errors.New("unknown edge command")
	}
	c, err := edgeapp.Load(os.Getenv)
	if err != nil {
		return err
	}
	caPEM, err := os.ReadFile(c.ControlCA)
	if err != nil {
		return errors.New("cannot load private control CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return errors.New("invalid private control CA")
	}
	// Every listener greets with, and presents a certificate for, its own name.
	// Each certificate is checked at startup and on every new handshake.
	listeners := c.Listeners()
	smtpCerts := make([]string, 0, len(listeners))
	bounceListeners := make([]bounce.Listener, 0, len(listeners))
	for _, l := range listeners {
		smtpCerts = append(smtpCerts, l.Cert)
		listenerTLS, tlsErr := publicSMTPTLS(l.Cert, l.Key, l.Hostname, c.ControlCert, nil)
		if tlsErr != nil {
			return tlsErr
		}
		bounceListeners = append(bounceListeners, bounce.Listener{Hostname: l.Hostname, TLS: listenerTLS})
	}
	controlTLS, err := privateControlTLS(c.ControlCert, c.ControlKey, smtpCerts, roots, c.ControllerIdentities)
	if err != nil {
		return err
	}
	// Control is the sole authority for the egress deny ranges: it filters
	// every route when it resolves the recipient. The Edge dials the numeric
	// address it was given, from a source it owns, and judges nothing.
	dialer, err := egress.NewSourceDialer(c.Sources())
	if err != nil {
		return errors.New("invalid SMTP source address")
	}
	// Refuse to start, as for an unbindable listener, when a configured source
	// is missing from this host; a worker must not advertise one it cannot bind.
	if err = requireLocalAddresses(c.Sources(), net.InterfaceAddrs); err != nil {
		return err
	}
	j, err := journal.OpenWithReserve(c.JournalPath, c.JournalCapacity, c.JournalReserveBytes)
	if err != nil {
		return errors.New("cannot open edge journal")
	}
	defer j.Close()
	// Before starting a replacement, operations must fence the previous worker
	// and its IP independently; a fresh journal cannot establish that fact.
	if err = j.Recover(time.Now()); err != nil {
		return errors.New("edge journal recovery failed")
	}
	authority, err := leaseproof.NewAuthority(c.ClockMargin)
	if err != nil {
		return errors.New("invalid lease configuration")
	}
	// The Edge enforces neither the recipient allowlist nor the egress deny
	// ranges: control is authoritative for both. THINPOST_RECIPIENT_POLICY,
	// THINPOST_RECIPIENT_ALLOWLIST_FILE, THINPOST_EDGE_INFRASTRUCTURE_CIDRS and
	// THINPOST_EDGE_ROUTE_EXCEPTIONS_FILE are not read here, so an Edge that
	// still has them set starts normally. Network controls on the Edge are the
	// boundary against a compromised control plane.
	worker := &edgeworker.Worker{Journal: j, Dialer: dialer, HeloName: c.Hostname, Sources: c.Sources(), SourceHostnames: sourceHostnames(listeners), SMTPTimeouts: &c.OutboundSMTPTimeouts}
	edge, err := edgesession.New(c.WorkerID, c.ControllerIdentities, j, worker, authority, 2)
	if err != nil {
		return errors.New("invalid edge session configuration")
	}
	sources := make([]string, 0, len(listeners))
	hostnames := make([]string, 0, len(listeners))
	for _, l := range listeners {
		sources = append(sources, l.Address.String())
		hostnames = append(hostnames, l.Hostname)
	}
	edge.Advertise(sources, hostnames, c.BounceDomains())
	// Inbound mail for app receiving domains: the routing snapshot and the
	// message handoff come from the control plane over the existing stream; the
	// worker holds no spool and logs only fixed counters.
	edge.EnableInbound()
	inboundCounters := &inboundStats{}
	smtpServers, err := bounce.NewServers(bounce.Config{Hostname: c.Hostname, Domains: c.BounceDomains(), MaxBytes: 16 << 20, MaxConcurrent: 1, ForwardTimeout: 20 * time.Second, Forwarder: edge,
		Inbound: edge.Inbound(), InboundMaxBytes: 10 << 20, InboundConcurrent: 4, InboundForwardTimeout: 60 * time.Second, Stats: inboundCounters}, bounceListeners)
	if err != nil {
		return errors.New("invalid SMTP listener configuration")
	}
	controlListener, err := net.Listen("tcp", c.ControlListen)
	if err != nil {
		return errors.New("cannot bind private control listener")
	}
	controlListener = netutil.LimitListener(controlListener, 8)
	defer controlListener.Close()
	// All listeners share one admission (64 total, 8 per source) and one rate
	// limiter, so the bounds hold for the process however many addresses it has.
	admissionStats := &smtpAdmissionStats{}
	admission, err := newSMTPAdmission(64, 8, admissionStats)
	if err != nil {
		return errors.New("invalid SMTP connection limits")
	}
	rateLimit, err := newSMTPRateLimiter(c.SMTPGlobalInterval, c.SMTPSourceInterval, c.SMTPGlobalBurst, c.SMTPSourceBurst, 4096)
	if err != nil {
		return errors.New("invalid SMTP connection rate limits")
	}
	rateLimit.stats = admissionStats
	smtpListeners := make([]net.Listener, 0, len(listeners))
	for _, l := range listeners {
		raw, listenErr := net.Listen("tcp", net.JoinHostPort(l.Address.String(), "25"))
		if listenErr != nil {
			return errors.New("cannot bind SMTP source address and port")
		}
		wrapped := &smtpRateListener{Listener: admission.Wrap(raw), limiter: rateLimit}
		defer wrapped.Close()
		smtpListeners = append(smtpListeners, wrapped)
	}
	log.Printf("smtp listeners=%d", len(smtpListeners))
	// Authenticated submission (587 STARTTLS, 465 implicit TLS per sending IP,
	// plus the optional in-cluster listener) has its own admission and rate
	// limits; the MX admission above is untouched. Port 25 keeps CHUNKING and
	// never offers AUTH.
	sub, err := startSubmission(c, edge, listeners, bounceListeners)
	if err != nil {
		return err
	}
	defer sub.closeListeners()
	healthListener, err := net.Listen("tcp4", healthAddress)
	if err != nil {
		return errors.New("cannot bind loopback health listener")
	}
	healthListener = netutil.LimitListener(healthListener, 4)
	defer healthListener.Close()
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(controlTLS)), grpc.MaxRecvMsgSize(128<<10), grpc.MaxSendMsgSize(128<<10), grpc.MaxConcurrentStreams(2))
	controlv1.RegisterEdgeControlServer(grpcServer, edge)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go logSMTPAdmissionStats(ctx, admissionStats, time.Minute, log.Printf)
	go logInboundStats(ctx, inboundCounters, time.Minute, log.Printf)
	sub.logStats(ctx)
	health := startJournalHealth(ctx, func() error {
		_, err := j.Identity()
		return err
	}, 5*time.Second, 15*time.Second)
	// Readiness also waits for every source's gateway to resolve, so a pod that
	// moved nodes is not ready before the routers learn its MAC. Liveness and
	// startup use the journal-only `healthcheck liveness` (/livez), so this only
	// delays readiness and never restarts the worker.
	var announcer *sourceAnnouncer
	readiness := health.ready
	if len(c.SourceGateways) > 0 {
		announcer, err = newSourceAnnouncer(c.Sources(), c.SourceGateways, log.Printf)
		if err != nil {
			return err
		}
		go announcer.run(ctx)
		readiness = allReady(health.ready, announcer.ready)
	}
	healthServer := &http.Server{Handler: edgeHealthHandler(readiness, health.ready), ReadHeaderTimeout: 2 * time.Second, IdleTimeout: 2 * time.Second, MaxHeaderBytes: 4096}
	defer healthServer.Close()
	closeSMTP := func() {
		for _, server := range smtpServers {
			_ = server.Close()
		}
		sub.closeServers()
	}
	results := make(chan error, 2+len(smtpServers)+len(sub.servers))
	go func() { results <- grpcServer.Serve(controlListener) }()
	for i, server := range smtpServers {
		go func() { results <- server.Serve(smtpListeners[i]) }()
	}
	for i, server := range sub.servers {
		go func() { results <- server.Serve(sub.serve[i]) }()
	}
	go func() { results <- healthServer.Serve(healthListener) }()
	select {
	case <-ctx.Done():
		closeSMTP()
		grpcServer.Stop()
		return nil
	case <-results:
		closeSMTP()
		grpcServer.Stop()
		return errors.New("edge listener stopped")
	}
}

// sourceHostnames lists each listener's name in listener (source) order.
func sourceHostnames(listeners []edgeapp.Listener) []string {
	out := make([]string, 0, len(listeners))
	for _, l := range listeners {
		out = append(out, l.Hostname)
	}
	return out
}
