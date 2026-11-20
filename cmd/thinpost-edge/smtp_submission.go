package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log"
	"net"
	"os"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/zyno-io/thinpost/internal/bounce"
	"github.com/zyno-io/thinpost/internal/edgeapp"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/edgesession"
	smtp "github.com/zyno-io/thinpost/internal/thirdparty/gosmtp"
)

// submissionRuntime holds the submission servers and the listeners each one
// serves. It is empty when submission is off.
type submissionRuntime struct {
	servers   []*smtp.Server
	serve     []net.Listener // what each server serves (465 is wrapped in TLS)
	raw       []net.Listener // closed on shutdown
	stats     *inboundStats
	admission []*smtpAdmissionStats
}

func (r *submissionRuntime) closeServers() {
	for _, server := range r.servers {
		_ = server.Close()
	}
}

func (r *submissionRuntime) closeListeners() {
	for _, l := range r.raw {
		_ = l.Close()
	}
}

func (r *submissionRuntime) logStats(ctx context.Context) {
	if len(r.servers) == 0 {
		return
	}
	go logInboundStats(ctx, r.stats, time.Minute, log.Printf)
	for _, a := range r.admission {
		go logSMTPAdmissionStats(ctx, a, time.Minute, log.Printf)
	}
}

// startSubmission binds the submission listeners when THINPOST_EDGE_SUBMISSION
// is true and/or the internal listener is configured, and enables the
// submission/1 capability. Every listener reuses its sending IP's TLS
// configuration (the IP's own certificate); the internal listener presents a
// certificate verified against the cluster-local CA.
func startSubmission(c edgeapp.Config, edge *edgesession.Server, listeners []edgeapp.Listener, bounceListeners []bounce.Listener) (*submissionRuntime, error) {
	runtime := &submissionRuntime{}
	if !c.Submission && !c.SubmissionInternal.Enabled() {
		return runtime, nil
	}
	// The public ports are reported to control only when the public listeners
	// exist; the internal listener is never reported.
	var ports []uint32
	if c.Submission {
		ports = edgeapp.SubmissionPublicPorts
	}
	if err := edge.EnableSubmission(ports...); err != nil {
		return nil, errors.New("invalid submission configuration")
	}
	rt, err := bindSubmission(c, edge.SMTPSubmission(), listeners, bounceListeners, submissionBind{starttls: "587", implicit: "465"}, semaphore.NewWeighted(edgeprotocol.EdgeSubmissionBudget))
	if err != nil {
		return nil, err
	}
	log.Printf("smtp submission listeners=%d", len(rt.servers))
	return rt, nil
}

// submissionBind names the public submission ports (tests use ephemeral ones).
type submissionBind struct{ starttls, implicit string }

// bindSubmission builds and binds the submission servers. The budget is the
// process-wide byte budget that message buffering across every submission
// listener draws from.
func bindSubmission(c edgeapp.Config, client bounce.Submission, listeners []edgeapp.Listener, bounceListeners []bounce.Listener, ports submissionBind, budget *semaphore.Weighted) (*submissionRuntime, error) {
	runtime := &submissionRuntime{}
	runtime.stats = &inboundStats{name: "smtp_submission"}
	limiter, err := newSMTPAuthLimiter(time.Minute, 10, 4096)
	if err != nil {
		return nil, errors.New("invalid SMTP authentication limits")
	}
	var identities []bounce.SubmissionListener
	var binds []string
	if c.Submission {
		for i, l := range listeners {
			identities = append(identities, bounce.SubmissionListener{Hostname: l.Hostname, TLS: bounceListeners[i].TLS, Port: 587}, bounce.SubmissionListener{Hostname: l.Hostname, TLS: bounceListeners[i].TLS, Port: 465})
			binds = append(binds, net.JoinHostPort(l.Address.String(), ports.starttls), net.JoinHostPort(l.Address.String(), ports.implicit))
		}
	}
	if internal := c.SubmissionInternal; internal.Enabled() {
		internalTLS, err := internalSubmissionTLS(c, internal)
		if err != nil {
			return nil, err
		}
		identities = append(identities, bounce.SubmissionListener{Hostname: internal.Hostname, TLS: internalTLS, Internal: true})
		binds = append(binds, internal.Listen.String())
	}
	servers, err := bounce.NewSubmissionServers(bounce.SubmissionConfig{Submission: client, AuthLimiter: limiter, Stats: runtime.stats,
		Budget: budget}, identities)
	if err != nil {
		return nil, errors.New("invalid SMTP submission configuration")
	}
	// 587 and 465 on every IP share one admission (64 total, 16 per source) and
	// one rate limiter (20 ms/200 global, 200 ms/30 per source); the internal
	// listener has its own admission (64/32).
	publicStats, internalStats := &smtpAdmissionStats{name: "smtp_submission_admission"}, &smtpAdmissionStats{name: "smtp_submission_internal_admission"}
	publicAdmission, err := newSMTPAdmission(64, 16, publicStats)
	if err != nil {
		return nil, errors.New("invalid SMTP connection limits")
	}
	internalAdmission, err := newSMTPAdmission(64, 32, internalStats)
	if err != nil {
		return nil, errors.New("invalid SMTP connection limits")
	}
	rateLimit, err := newSMTPRateLimiter(20*time.Millisecond, 200*time.Millisecond, 200, 30, 4096)
	if err != nil {
		return nil, errors.New("invalid SMTP connection rate limits")
	}
	rateLimit.stats = publicStats
	runtime.admission = []*smtpAdmissionStats{publicStats}
	for i, server := range servers {
		raw, err := net.Listen("tcp", binds[i])
		if err != nil {
			runtime.closeListeners()
			return nil, errors.New("cannot bind SMTP submission address and port")
		}
		var wrapped net.Listener
		if identities[i].Internal {
			wrapped = internalAdmission.Wrap(raw)
		} else {
			wrapped = &smtpRateListener{Listener: publicAdmission.Wrap(raw), limiter: rateLimit}
		}
		runtime.raw = append(runtime.raw, wrapped)
		// The clamp is the outermost non-TLS wrapper; implicit TLS wraps it.
		serve := bounce.ClampListener(wrapped)
		if identities[i].Port == 465 {
			serve = tls.NewListener(serve, server.TLSConfig)
		}
		runtime.servers = append(runtime.servers, server)
		runtime.serve = append(runtime.serve, serve)
	}
	if c.SubmissionInternal.Enabled() {
		runtime.admission = append(runtime.admission, internalStats)
	}
	return runtime, nil
}

func internalSubmissionTLS(c edgeapp.Config, internal edgeapp.SubmissionInternal) (*tls.Config, error) {
	caPEM, err := os.ReadFile(internal.CA)
	if err != nil {
		return nil, errors.New("cannot load internal submission CA")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid internal submission CA")
	}
	return publicSMTPTLS(internal.Cert, internal.Key, internal.Hostname, c.ControlCert, pool)
}
