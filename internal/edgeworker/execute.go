// Package edgeworker executes one delivery attempt. Stream authentication,
// lease expiry, scheduling, bounce handling and process lifecycle are pending.
package edgeworker

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/smtptransport"
)

var ErrJournal = errors.New("attempt journal failure; stop accepting work")

type Journal interface {
	Begin(string, string, journal.Fence, time.Time) error
	CompleteReply(id, outcome string, code int, stage, enhanced string, now time.Time) error
}
type Dialer interface {
	// Dial connects from source (the zero address means the primary) to the
	// numeric destination; it must never use any other local address.
	Dial(ctx context.Context, source, destination netip.Addr, recipient, mx string) (net.Conn, error)
}
type Worker struct {
	Journal  Journal
	Dialer   Dialer
	HeloName string
	// Sources are the outbound addresses this worker owns, the primary first.
	// A delivery may name one of them; anything else is refused before a permit
	// is requested.
	Sources []netip.Addr
	// SourceHostnames holds each source's configured hostname (its HELO name),
	// in Sources order. A delivery that names a source must carry that name.
	SourceHostnames []string
	Roots           *x509.CertPool
	SMTPTimeouts    *smtptransport.Timeouts
	// smtpTimeout is private so callers cannot accidentally disable the bound.
	// Tests shorten it; production uses the fixed deadline below.
	smtpTimeout time.Duration
}

// Execute assumes the stream has already authenticated the controller and
// activated its database-issued fence. It checks the final MIME again, commits
// the attempt before connecting, and commits the result before returning it.
// A journal error is fatal to claiming more work until recovery. The worker
// never stores recipients, message contents, or SMTP error text in its journal.
func (w *Worker) Execute(ctx context.Context, fence journal.Fence, start *controlv1.DeliveryStart, mime []byte, authorized func() bool, permit func(context.Context, *controlv1.DeliveryStart) error) (*controlv1.DeliveryResult, error) {
	if ctx.Err() != nil || w.Journal == nil || w.Dialer == nil || w.HeloName == "" || authorized == nil || permit == nil || !authorized() || !edgeprotocol.VerifyMessage(start, mime) || start.Epoch != fence.Epoch {
		return nil, errors.New("invalid delivery execution request")
	}
	address, err := netip.ParseAddr(start.DestinationIp)
	if err != nil || address.Zone() != "" {
		return nil, errors.New("invalid delivery destination")
	}
	source, helo, sourceErr := w.chooseSource(start)
	timeouts := smtptransport.DefaultTimeouts()
	if w.SMTPTimeouts != nil {
		timeouts = *w.SMTPTimeouts
	}
	if err = timeouts.Validate(); err != nil {
		return nil, err
	}
	if err = w.Journal.Begin(start.AttemptId, start.DeliveryId, fence, time.Now()); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJournal, err)
	}
	outcome := smtptransport.NotAccepted
	var attemptErr error
	var dialErr error
	if ctx.Err() != nil || !authorized() {
		dialErr = errors.New("controller lease expired")
	} else if start.TrackingSendCutoffUnixMs != 0 && time.Until(time.UnixMilli(start.TrackingSendCutoffUnixMs)) <= delivery.TrackingClockMargin {
		dialErr = errors.New("tracking lifetime is insufficient")
	} else if sourceErr != nil {
		// Checked before the permit: nothing is authorized, and nothing dials,
		// for a source this worker did not report.
		dialErr = sourceErr
	} else if err = permit(ctx, start); err != nil {
		dialErr = errors.New("SMTP start permit unavailable")
	} else if ctx.Err() != nil || !authorized() {
		dialErr = errors.New("controller lease expired")
	} else {
		timeout := delivery.MaxSMTPNetworkDuration
		if w.smtpTimeout > 0 && w.smtpTimeout < timeout {
			timeout = w.smtpTimeout
		}
		if start.TrackingSendCutoffUnixMs != 0 {
			remaining := time.Until(time.UnixMilli(start.TrackingSendCutoffUnixMs)) - delivery.TrackingClockMargin
			if remaining <= 0 {
				dialErr = errors.New("tracking lifetime is insufficient")
			} else if remaining < timeout {
				timeout = remaining
			}
		}
		if dialErr != nil {
			attemptErr = dialErr
		} else {
			smtpCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			var conn net.Conn
			dialCtx, stopDial := context.WithTimeout(smtpCtx, timeouts.Connect)
			conn, dialErr = w.Dialer.Dial(dialCtx, source, address, start.Recipient, start.MxName)
			if dialErr == nil && dialCtx.Err() != nil {
				dialErr = dialCtx.Err()
			}
			stopDial()
			if dialErr != nil && conn != nil {
				_ = conn.Close()
			}
			if dialErr == nil {
				if conn == nil {
					dialErr = errors.New("SMTP dial returned no connection")
				} else if smtpCtx.Err() != nil || !authorized() {
					_ = conn.Close()
					dialErr = errors.New("controller lease expired or SMTP attempt timed out")
				} else {
					outcome, attemptErr = smtptransport.Send(smtpCtx, conn, smtptransport.Request{MXName: start.MxName, HeloName: helo, EnvelopeFrom: start.EnvelopeFrom, Recipient: start.Recipient, RequireTLS: start.EnhancedProtection, Roots: w.Roots, MIME: bytes.NewReader(mime), Timeouts: &timeouts})
				}
			}
		}
	}
	if dialErr != nil {
		attemptErr = dialErr
	}
	kind := journal.Rejected
	result := controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED
	code := 0
	var stage, enhanced string
	var protoStage controlv1.SmtpStage
	switch outcome {
	case smtptransport.Accepted:
		kind = journal.Accepted
		result = controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED
		code = 250
	case smtptransport.Uncertain:
		kind = journal.Uncertain
		result = controlv1.DeliveryOutcome_DELIVERY_OUTCOME_UNCERTAIN
	case smtptransport.NotAccepted:
		var smtpErr *smtptransport.ReplyError
		if errors.As(attemptErr, &smtpErr) && smtpErr.Code >= 400 && smtpErr.Code <= 599 {
			code = smtpErr.Code
			// Only an envelope or DATA reply carries a stage; setup replies
			// (greeting, EHLO, STARTTLS) are route-level and stay unlabeled.
			switch smtpErr.Stage {
			case "MAIL FROM":
				stage, protoStage = "MAIL", controlv1.SmtpStage_SMTP_STAGE_MAIL
			case "RCPT TO":
				stage, protoStage = "RCPT", controlv1.SmtpStage_SMTP_STAGE_RCPT
			case "DATA":
				stage, protoStage = "DATA", controlv1.SmtpStage_SMTP_STAGE_DATA
			}
			if stage != "" && journal.ValidEnhanced(smtpErr.Enhanced) {
				enhanced = smtpErr.Enhanced
			}
			if errors.Is(attemptErr, smtptransport.ErrRouteRejected) {
				kind = journal.RouteRejected
				result = controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ROUTE_REJECTED
			}
		}
	default:
		return nil, fmt.Errorf("%w: invalid SMTP outcome", ErrJournal)
	}
	if err = w.Journal.CompleteReply(start.AttemptId, kind, code, stage, enhanced, time.Now()); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJournal, err)
	}
	return &controlv1.DeliveryResult{AttemptId: start.AttemptId, DeliveryId: start.DeliveryId, Epoch: start.Epoch, Outcome: result, SmtpCode: uint32(code), SmtpStage: protoStage, SmtpEnhanced: enhanced}, nil
}

// chooseSource resolves the delivery's source address and HELO name. Both must
// be set, the source must be one this worker owns, and the HELO name must equal
// that source's configured hostname: control cannot make the worker greet a
// receiver under a name the address does not carry. A start without either
// field is refused, never defaulted. The returned error is generic: it never
// names the address or the name.
func (w *Worker) chooseSource(start *controlv1.DeliveryStart) (netip.Addr, string, error) {
	if start.SourceAddress == "" || start.HeloName == "" {
		return netip.Addr{}, "", errors.New("incomplete delivery source")
	}
	source, err := netip.ParseAddr(start.SourceAddress)
	if err != nil || source.Zone() != "" || source.Is4In6() || source.String() != start.SourceAddress {
		return netip.Addr{}, "", errors.New("invalid delivery source")
	}
	owned := false
	hostname := ""
	for i, candidate := range w.Sources {
		if candidate == source {
			owned = true
			if i < len(w.SourceHostnames) {
				hostname = w.SourceHostnames[i]
			}
			break
		}
	}
	if !owned {
		return netip.Addr{}, "", errors.New("delivery source is not available on this worker")
	}
	if name, ok := admin.NormalizeDomain(start.HeloName); !ok || name != start.HeloName {
		return netip.Addr{}, "", errors.New("invalid delivery HELO name")
	}
	if hostname == "" || start.HeloName != hostname {
		return netip.Addr{}, "", errors.New("delivery HELO name does not match the source")
	}
	return source, start.HeloName, nil
}
