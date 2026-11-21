// Package bounce provides the Edge SMTP receiver. It accepts opaque bounce
// tokens and, when an Inbound router is configured, mail for the receiving
// domains a trusted routing snapshot lists. It holds bodies only in bounded
// memory and requires a trusted durable commit before acknowledging SMTP DATA.
package bounce

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"strings"
	"time"

	smtp "github.com/zyno-io/thinpost/internal/thirdparty/gosmtp"
)

const maxAllowedBytes = 16 << 20

// Forwarder must return nil only after trusted has durably committed the body
// and its database record. It must honor context cancellation and must not log
// the token or message contents.
type Forwarder interface {
	Forward(context.Context, string, string, []byte) error
}

type Config struct {
	// Hostname is the SMTP greeting name. Domains are the bounce domains whose
	// b+<token> recipients are accepted: the Edge passes every listener's name,
	// which is also the HELO name and Return-Path domain control uses for it.
	Hostname       string
	Domains        []string
	TLS            *tls.Config
	MaxBytes       int64
	MaxConcurrent  int
	ForwardTimeout time.Duration
	Forwarder      Forwarder
	// Inbound is optional. Without it the bounce domains and bounce addresses
	// (b+<token>) at any domain are accepted and every other recipient is
	// refused. The limits below apply to inbound mail only; zero values take
	// the defaults.
	Inbound               Inbound
	InboundMaxBytes       int64
	InboundConcurrent     int
	InboundForwardTimeout time.Duration
	// Stats is optional and receives fixed counter names only.
	Stats Stats
}

// backend is the receiver core shared by every listener: configuration and the
// bounded DATA slots. Listener identity lives on each smtp.Server.
type backend struct {
	config  Config
	slots   chan struct{}
	inSlots chan struct{}
}
type session struct {
	backend *backend
	conn    *smtp.Conn
	token   string
	txn     transaction
	// Connection-wide abuse counters; Reset clears only the transaction.
	transactions, invalid int
	closed                bool
}

// MaxDomains bounds the bounce domains a receiver accepts: one per listener
// name, with headroom.
const MaxDomains = 24

// Listener is one SMTP listener's own identity: the name it greets with and the
// TLS configuration presenting that name's certificate.
type Listener struct {
	Hostname string
	TLS      *tls.Config
}

// NewServer returns the receiver for a single listener, named by config.Hostname
// and secured by config.TLS. It does not bind a socket. Caller must bind port 25
// non-root on edge, configure external firewalls and connection limits, and
// provide certificates.
func NewServer(config Config) (*smtp.Server, error) {
	servers, err := NewServers(config, []Listener{{Hostname: config.Hostname, TLS: config.TLS}})
	if err != nil {
		return nil, err
	}
	return servers[0], nil
}

// NewServers returns one SMTP server per listener, all sharing one receiver
// core: the DATA slots, the accepted domains and the forwarders. The memory
// bound therefore holds for the process, not per listener. Each server greets
// with its own name and presents its own certificate; config.Hostname stays the
// receiving host recorded on inbound mail, whichever listener accepted it.
// config.TLS is unused. It does not bind sockets.
func NewServers(config Config, listeners []Listener) ([]*smtp.Server, error) {
	if !dnsName(config.Hostname) || len(config.Domains) == 0 || len(config.Domains) > MaxDomains || len(listeners) == 0 || len(listeners) > 16 || config.Forwarder == nil || config.MaxBytes <= 0 || config.MaxBytes > maxAllowedBytes || config.MaxConcurrent <= 0 || config.MaxConcurrent > 8 || config.ForwardTimeout <= 0 || config.ForwardTimeout > time.Minute {
		return nil, errors.New("invalid bounce receiver configuration")
	}
	for _, domain := range config.Domains {
		if !dnsName(domain) {
			return nil, errors.New("invalid bounce receiver configuration")
		}
	}
	for _, l := range listeners {
		if !dnsName(l.Hostname) || l.TLS == nil || len(l.TLS.Certificates) == 0 && l.TLS.GetCertificate == nil {
			return nil, errors.New("invalid bounce receiver configuration")
		}
	}
	config.Domains = append([]string(nil), config.Domains...)
	if config.InboundMaxBytes == 0 {
		config.InboundMaxBytes = min(defaultInboundMaxBytes, config.MaxBytes)
	}
	if config.InboundConcurrent == 0 {
		config.InboundConcurrent = defaultInboundConcurrent
	}
	if config.InboundForwardTimeout == 0 {
		config.InboundForwardTimeout = defaultInboundForwardTimeout
	}
	if config.InboundMaxBytes < 0 || config.InboundMaxBytes > config.MaxBytes || config.InboundConcurrent < 0 || config.InboundConcurrent > 8 || config.InboundForwardTimeout < 0 || config.InboundForwardTimeout > time.Minute {
		return nil, errors.New("invalid bounce receiver configuration")
	}
	b := &backend{config: config, slots: make(chan struct{}, config.MaxConcurrent), inSlots: make(chan struct{}, config.InboundConcurrent)}
	servers := make([]*smtp.Server, 0, len(listeners))
	for _, l := range listeners {
		server := smtp.NewServer(b)
		server.Domain = l.Hostname
		server.TLSConfig = l.TLS
		server.MaxMessageBytes = config.MaxBytes
		server.MaxRecipients = maxRecipients
		server.ReadTimeout = 30 * time.Second
		server.WriteTimeout = 30 * time.Second
		// go-smtp's default logger prints peer addresses and panic values, which
		// could carry message content. Replace it; never set server.Debug.
		server.ErrorLog = redactedLogger{logf: log.Printf}
		servers = append(servers, server)
	}
	return servers, nil
}

// redactedLogger receives go-smtp's internal error reports and emits only a
// fixed class, dropping the peer address, error text, panic value and stack.
type redactedLogger struct{ logf func(string, ...any) }

func (l redactedLogger) Printf(format string, _ ...any) {
	if strings.HasPrefix(format, "panic serving") {
		l.logf("bounce SMTP handler panic")
		return
	}
	l.logf("bounce SMTP connection error")
}
func (l redactedLogger) Println(...any) { l.logf("bounce SMTP connection error") }

// acceptsDomain reports whether a recipient domain is one of the edge's own
// bounce (HELO) names. Bounce tokens at other domains are classified in Rcpt.
func (b *backend) acceptsDomain(domain string) bool {
	for _, accepted := range b.config.Domains {
		if strings.EqualFold(domain, accepted) {
			return true
		}
	}
	return false
}
func (b *backend) NewSession(conn *smtp.Conn) (smtp.Session, error) {
	return &session{backend: b, conn: conn}, nil
}
func (s *session) Reset()        { s.token = ""; s.txn = transaction{} }
func (s *session) Logout() error { return nil }
func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	s.Reset()
	if s.closed {
		return tooManyErrors()
	}
	s.transactions++
	if s.transactions > maxTransactions {
		return s.closeConnection()
	}
	// The reverse path is forwarded to control as-is, so it must be printable
	// ASCII within the 320-byte path limit: anything else is refused here and
	// can never reach (or break) the control stream.
	if len(from) > maxAddressBytes || !printableASCII(from) {
		return &smtp.SMTPError{Code: 501, EnhancedCode: smtp.EnhancedCode{5, 1, 7}, Message: "Bad sender address syntax"}
	}
	s.txn = transaction{mailFrom: from}
	if opts != nil {
		s.txn.size = opts.Size
	}
	return nil
}
func (s *session) Rcpt(address string, _ *smtp.RcptOptions) error {
	if s.closed {
		return tooManyErrors()
	}
	if s.token != "" {
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 5, 3}, Message: "One recipient per message"}
	}
	local, domain, ok := splitAddress(address)
	if !ok {
		return s.invalidRecipient(unknownRecipient())
	}
	// 1. The edge's own HELO names: bounce, with the strict token rules.
	if s.backend.acceptsDomain(domain) {
		return s.bounceRecipient(local)
	}
	// 2. A domain with a live inbound route (any state but unknown) is inbound,
	// including its tempfail states: inbound wins over a b+ local part.
	inbound := s.backend.config.Inbound
	if inbound != nil {
		if group, state := inbound.Lookup(domain); state != RouteUnknown {
			return s.inboundRecipient(local, domain, group, state)
		}
	}
	// 3. b+<32 lowercase hex> at any other domain is a bounce: the Return-Path
	// domain is the sender's choice (control decides it), and the token is still
	// validated by control before anything is acknowledged.
	if isBounceLocal(local) {
		// Counted only once accepted, so a refused mix of classes isn't.
		err := s.bounceRecipient(local)
		if err == nil {
			s.count("rcpt_bounce_other_domain")
		}
		return err
	}
	if inbound == nil {
		return s.invalidRecipient(unknownRecipient())
	}
	s.count("rcpt_relay_denied")
	return s.invalidRecipient(relayDenied())
}

// isBounceLocal reports whether a local part is b+ followed by exactly 32
// lowercase hex digits (a 128-bit bounce token).
func isBounceLocal(local string) bool {
	token, ok := strings.CutPrefix(local, "b+")
	if !ok || len(token) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(token)
	return err == nil && len(decoded) == 16 && token == strings.ToLower(token)
}

// bounceRecipient accepts one b+<token> recipient. The bounce path allows a
// single recipient and nothing else in the transaction.
func (s *session) bounceRecipient(local string) error {
	if s.txn.class != classNone {
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 5, 3}, Message: "One recipient per message"}
	}
	if !isBounceLocal(local) {
		return s.invalidRecipient(unknownRecipient())
	}
	s.token = strings.TrimPrefix(local, "b+")
	s.txn.class = classBounce
	return nil
}
func (s *session) Data(reader io.Reader) error {
	switch s.txn.class {
	case classInbound:
		return s.inboundData(reader)
	case classBounce:
	default:
		return unknownRecipient()
	}
	select {
	case s.backend.slots <- struct{}{}:
		defer func() { <-s.backend.slots }()
	default:
		return temporary()
	}
	body, err := io.ReadAll(io.LimitReader(reader, s.backend.config.MaxBytes+1))
	if errors.Is(err, smtp.ErrDataTooLarge) || int64(len(body)) > s.backend.config.MaxBytes {
		return smtp.ErrDataTooLarge
	}
	if err != nil || len(body) == 0 {
		return temporary()
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return temporary()
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.backend.config.ForwardTimeout)
	defer cancel()
	if err = s.backend.config.Forwarder.Forward(ctx, s.token, hex.EncodeToString(id[:]), body); err != nil {
		return temporary()
	}
	return nil
}
func unknownRecipient() error {
	return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "Unknown recipient"}
}
func temporary() error {
	return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary delivery failure"}
}
func dnsName(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.ToLower(host), ".") {
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
