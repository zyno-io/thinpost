package bounce

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/emersion/go-smtp"
)

// Inbound listener limits (the Edge's per-class abuse controls).
const (
	defaultInboundMaxBytes       = 10 << 20
	defaultInboundConcurrent     = 4
	defaultInboundForwardTimeout = 60 * time.Second
	maxRecipients                = 50
	maxInvalidRecipients         = 10
	maxTransactions              = 20
	maxDataWallClock             = 10 * time.Minute
	maxAddressBytes              = 320
	maxLocalPartBytes            = 64
)

// RouteState is what the routing snapshot says about a recipient domain.
type RouteState uint8

const (
	// RouteUnknown means the domain is not a receiving domain: 550 5.7.1.
	RouteUnknown RouteState = iota
	// RouteAccept means recipients at the domain are accepted.
	RouteAccept
	// RouteTempfailState means the domain exists but cannot receive now (paused
	// app, disabled domain, no webhook endpoint, backlog, closed gate): 451.
	RouteTempfailState
	// RouteTempfailNoSession means a snapshot exists but no control session is
	// live to commit mail: 451.
	RouteTempfailNoSession
)

// Verdict is control's answer for one inbound message.
type Verdict uint8

const (
	// VerdictAccepted means the message is durably committed (or an exact
	// duplicate of a committed one).
	VerdictAccepted Verdict = iota + 1
	// VerdictTempfail means it was not committed; the sender retries.
	VerdictTempfail
	// VerdictRejected means control refused it permanently.
	VerdictRejected
)

// Message is one inbound message with the facts the receiver observed. It is
// held in memory only and handed to the trusted plane over the control stream.
type Message struct {
	ID       string
	MailFrom string
	Rcpts    []string
	ClientIP netip.Addr
	Helo     string
	TLSVer   string
	TLSCiph  string
	Received time.Time
	Receiver string
	Group    uint32
	Body     []byte
}

// Inbound supplies the routing snapshot and the handoff to the trusted plane.
// Forward must return VerdictAccepted only after trusted has durably committed
// the message, and an error for anything it cannot confirm.
type Inbound interface {
	Lookup(domain string) (group uint32, state RouteState)
	Forward(context.Context, Message) (Verdict, error)
}

// Stats receives fixed counter names (no addresses or content), for the Edge's
// per-minute log.
type Stats interface{ Count(name string) }

type class uint8

const (
	classNone class = iota
	classBounce
	classInbound
)

type transaction struct {
	class    class
	mailFrom string
	size     int64
	group    uint32
	rcpts    []string
}

// printableASCII reports whether every byte is a visible ASCII character.
func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f {
			return false
		}
	}
	return true
}

// sanitizeHelo bounds a client-supplied EHLO name before it is forwarded:
// visible ASCII only (anything else is dropped), at most 255 bytes.
func sanitizeHelo(helo string) string {
	var b strings.Builder
	for i := 0; i < len(helo) && b.Len() < 255; i++ {
		if c := helo[i]; c > ' ' && c < 0x7f {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (s *session) count(name string) {
	if s.backend.config.Stats != nil {
		s.backend.config.Stats.Count(name)
	}
}

// splitAddress validates the shape of an SMTP forward path: printable ASCII
// only (SMTPUTF8 is not advertised), bounded, one @, a non-empty local part and
// no % or ! (source-routing relics). It returns the lowercased domain.
func splitAddress(address string) (local, domain string, ok bool) {
	if len(address) < 3 || len(address) > maxAddressBytes {
		return "", "", false
	}
	for i := 0; i < len(address); i++ {
		if c := address[i]; c <= ' ' || c >= 0x7f {
			return "", "", false
		}
	}
	at := strings.LastIndexByte(address, '@')
	if at <= 0 || at == len(address)-1 {
		return "", "", false
	}
	local, domain = address[:at], strings.ToLower(address[at+1:])
	if len(local) > maxLocalPartBytes || strings.ContainsAny(local, "%!") || strings.HasPrefix(local, "@") || strings.ContainsAny(domain, "@,:[]") {
		return "", "", false
	}
	return local, domain, true
}

func (s *session) invalidRecipient(err error) error {
	s.invalid++
	if s.invalid > maxInvalidRecipients {
		return s.closeConnection()
	}
	return err
}

// closeConnection answers 421 and closes the connection shortly after the
// reply is written. go-smtp does not close on a 421 returned by a session.
func (s *session) closeConnection() error {
	s.closed = true
	s.count("connection_closed_abuse")
	if conn := s.conn; conn != nil {
		time.AfterFunc(250*time.Millisecond, func() { _ = conn.Close() })
	}
	return tooManyErrors()
}

func tooManyErrors() error {
	return &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Too many errors on this connection"}
}
func relayDenied() error {
	return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Relay access denied"}
}
func recipientTempfail() error {
	return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Recipient temporarily unavailable, try again later"}
}
func tooLarge() error {
	return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: "Message size exceeds the limit"}
}
func mixedTransaction() error {
	return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 5, 3}, Message: "Too many recipients for this message, send them in a later transaction"}
}

// inboundRecipient routes one recipient at a non-bounce domain.
func (s *session) inboundRecipient(local, domain string) error {
	inbound := s.backend.config.Inbound
	group, state := inbound.Lookup(domain)
	switch state {
	case RouteAccept:
	case RouteTempfailState:
		s.count("rcpt_tempfail_state")
		return recipientTempfail()
	case RouteTempfailNoSession:
		s.count("rcpt_tempfail_no_session")
		return recipientTempfail()
	default:
		s.count("rcpt_relay_denied")
		return s.invalidRecipient(relayDenied())
	}
	if s.txn.class == classBounce || s.txn.class == classInbound && s.txn.group != group || len(s.txn.rcpts) >= maxRecipients {
		return mixedTransaction()
	}
	if s.txn.size > s.backend.config.InboundMaxBytes {
		s.count("rcpt_too_large")
		return tooLarge()
	}
	address := local + "@" + domain
	for _, existing := range s.txn.rcpts {
		if existing == address {
			return nil
		}
	}
	s.txn.class, s.txn.group = classInbound, group
	s.txn.rcpts = append(s.txn.rcpts, address)
	s.count("rcpt_accept")
	return nil
}

// deadlineReader fails reads after the transaction's wall-clock allowance.
type deadlineReader struct {
	r        io.Reader
	deadline time.Time
}

var errDataWallClock = errors.New("DATA wall clock exceeded")

func (d deadlineReader) Read(p []byte) (int, error) {
	if time.Now().After(d.deadline) {
		return 0, errDataWallClock
	}
	return d.r.Read(p)
}

func (s *session) inboundData(reader io.Reader) error {
	cfg := s.backend.config
	select {
	case s.backend.inSlots <- struct{}{}:
		defer func() { <-s.backend.inSlots }()
	default:
		s.count("data_tempfail")
		return temporary()
	}
	body, err := io.ReadAll(io.LimitReader(deadlineReader{r: reader, deadline: time.Now().Add(maxDataWallClock)}, cfg.InboundMaxBytes+1))
	if errors.Is(err, smtp.ErrDataTooLarge) || int64(len(body)) > cfg.InboundMaxBytes {
		s.count("data_too_large")
		return tooLarge()
	}
	if err != nil || len(body) == 0 || len(s.txn.rcpts) == 0 {
		s.count("data_tempfail")
		return temporary()
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		s.count("data_tempfail")
		return temporary()
	}
	message := Message{ID: hex.EncodeToString(id[:]), MailFrom: s.txn.mailFrom, Rcpts: append([]string(nil), s.txn.rcpts...), Received: time.Now(), Receiver: cfg.Hostname, Group: s.txn.group, Body: body}
	if s.conn != nil {
		message.Helo = sanitizeHelo(s.conn.Hostname())
		if remote := s.conn.Conn(); remote != nil {
			if host, _, splitErr := net.SplitHostPort(remote.RemoteAddr().String()); splitErr == nil {
				if address, parseErr := netip.ParseAddr(host); parseErr == nil {
					message.ClientIP = address.Unmap()
				}
			}
		}
		if state, ok := s.conn.TLSConnectionState(); ok {
			message.TLSVer, message.TLSCiph = tls.VersionName(state.Version), tls.CipherSuiteName(state.CipherSuite)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.InboundForwardTimeout)
	defer cancel()
	verdict, err := cfg.Inbound.Forward(ctx, message)
	if err != nil {
		s.count("data_tempfail")
		return temporary()
	}
	switch verdict {
	case VerdictAccepted:
		s.count("data_accept")
		return nil
	case VerdictRejected:
		s.count("data_rejected")
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Message refused"}
	}
	s.count("data_tempfail")
	return temporary()
}
