package bounce

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	smtp "github.com/zyno-io/thinpost/internal/thirdparty/gosmtp"
)

// Submission listener limits.
const (
	// MaxSubmissionBytes is the largest decoded message a submission carries.
	MaxSubmissionBytes = edgeprotocol.MaxSubmissionBytes

	maxSubmissionRecipients   = edgeprotocol.MaxSubmissionRecipients
	maxSubmissionTransactions = 100
	maxAuthAttempts           = 3
	maxRejectedTransfers      = 3

	// submissionChunk is the buffering granularity of the edge byte budget.
	submissionChunk = 64 << 10
	// submissionBudgetWait bounds the wait for the edge byte budget.
	submissionBudgetWait = 15 * time.Second
	// submissionTimeout bounds one Submit call (the bridge waits up to 60 s).
	submissionTimeout = 75 * time.Second
	authTimeout       = 15 * time.Second
)

// Credential is an SMTP credential as the client sent it. Every formatting verb
// prints a placeholder so the secret cannot reach a log.
type Credential struct{ ID, Secret string }

func (Credential) String() string   { return "<smtp credential redacted>" }
func (Credential) GoString() string { return "<smtp credential redacted>" }

// Format redacts under every verb, including %+v.
func (c Credential) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }

// Reply is a fixed SMTP reply chosen by the trusted side's verdict.
type Reply struct {
	Code     int
	Enhanced string
	Text     string
	// Close reports that the connection must be closed after the reply.
	Close bool
}

func (r Reply) err() *smtp.SMTPError {
	var code smtp.EnhancedCode
	parts := strings.Split(r.Enhanced, ".")
	if len(parts) == 3 {
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				code = smtp.EnhancedCodeNotSet
				break
			}
			code[i] = n
		}
	}
	return &smtp.SMTPError{Code: r.Code, EnhancedCode: code, Message: r.Text}
}

// AuthResult is the answer to a credential check. Exactly one of OK and
// Invalid is set for a definite answer; otherwise Reply carries the temporary
// failure (454) or the closing refusal (421).
type AuthResult struct {
	OK      bool
	Invalid bool
	Reply   Reply
}

// SubmissionMessage is one message to hand to the trusted side. Body is
// immutable forever after Submit is called: it is never pooled or reused.
type SubmissionMessage struct {
	Credential Credential
	MailFrom   string
	Rcpts      []string
	ClientIP   netip.Addr
	// TLSVersion is the version name from the connection's real
	// tls.ConnectionState; empty means plaintext.
	TLSVersion string
	Received   time.Time
	// Port is 465 or 587; 0 for the internal listener.
	Port uint32
	Body []byte
}

func (SubmissionMessage) String() string   { return "<submission message redacted>" }
func (SubmissionMessage) GoString() string { return "<submission message redacted>" }

// Format redacts under every verb.
func (m SubmissionMessage) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(m.String())) }

// Submission is the edge bridge's view for the submission listeners. Submit
// returns the reply to give the client, and an error when nothing the client
// should be told about was committed (the session then answers 451).
type Submission interface {
	Available() bool
	Verify(ctx context.Context, c Credential, clientIP netip.Addr) (AuthResult, error)
	Submit(ctx context.Context, m SubmissionMessage) (Reply, error)
}

// AuthGate is the brute-force limiter's decision for a client address.
type AuthGate uint8

const (
	// AuthGateOpen lets the client attempt AUTH.
	AuthGateOpen AuthGate = iota
	// AuthGateBlocked means the address used up its failure budget: 421 + close.
	AuthGateBlocked
	// AuthGateBusy means the limiter's table is full and the address is unknown: 454.
	AuthGateBusy
)

// AuthLimiter bounds failed AUTH attempts per client address.
type AuthLimiter interface {
	Check(addr netip.Addr) AuthGate
	// Failed records one failed attempt.
	Failed(addr netip.Addr)
}

// SubmissionConfig configures the submission and internal listeners. All of
// them share one core: the Submission, the limiter and the byte budget.
type SubmissionConfig struct {
	Submission  Submission
	AuthLimiter AuthLimiter
	// Budget is the process-wide edge byte budget that message buffering draws
	// from. Nil creates one of edgeprotocol.EdgeSubmissionBudget bytes.
	Budget *semaphore.Weighted
	Stats  Stats

	// Test knobs; zero values take the defaults.
	maxBytes       int64 // the session's own limit
	serverMaxBytes int64 // the server's limit; defaults to maxBytes
	budgetWait     time.Duration
}

// SubmissionListener is one submission listener's identity.
type SubmissionListener struct {
	Hostname string
	TLS      *tls.Config
	// Port is 465 or 587 for a public listener and 0 for the internal one.
	Port     uint32
	Internal bool
}

type submissionBackend struct {
	cfg      SubmissionConfig
	listener SubmissionListener
}

// NewSubmissionServers returns one server per listener (587 and 465 per
// sending IP, plus the optional internal listener), all sharing one core. Each
// is DATA-only (no CHUNKING), limited to MaxSubmissionBytes and 100
// recipients. It does not bind sockets: serve 587 and the internal listener
// as-is and wrap 465's listener with tls.NewListener, below which the caller
// installs ClampListener.
func NewSubmissionServers(cfg SubmissionConfig, listeners []SubmissionListener) ([]*smtp.Server, error) {
	if cfg.Submission == nil || len(listeners) == 0 || len(listeners) > 16 {
		return nil, errors.New("invalid submission listener configuration")
	}
	if cfg.maxBytes == 0 {
		cfg.maxBytes = MaxSubmissionBytes
	}
	if cfg.serverMaxBytes == 0 {
		cfg.serverMaxBytes = cfg.maxBytes
	}
	if cfg.budgetWait == 0 {
		cfg.budgetWait = submissionBudgetWait
	}
	if cfg.Budget == nil {
		cfg.Budget = semaphore.NewWeighted(edgeprotocol.EdgeSubmissionBudget)
	}
	servers := make([]*smtp.Server, 0, len(listeners))
	for _, l := range listeners {
		if !dnsName(l.Hostname) || l.TLS == nil || len(l.TLS.Certificates) == 0 && l.TLS.GetCertificate == nil {
			return nil, errors.New("invalid submission listener configuration")
		}
		switch l.Port {
		case 0, 465, 587:
		default:
			return nil, errors.New("invalid submission listener configuration")
		}
		server := smtp.NewServer(&submissionBackend{cfg: cfg, listener: l})
		server.Domain = l.Hostname
		server.TLSConfig = l.TLS
		server.MaxMessageBytes = cfg.serverMaxBytes
		server.MaxRecipients = maxSubmissionRecipients
		server.DisableCHUNKING = true
		server.ReadTimeout = 30 * time.Second
		server.WriteTimeout = 30 * time.Second
		server.ErrorLog = redactedLogger{logf: log.Printf}
		servers = append(servers, server)
	}
	return servers, nil
}

// submissionTxn is one mail transaction. Its pointer is swapped by Mail and
// Reset; Data captures it at entry and never reads the session's pointer again.
type submissionTxn struct {
	mailFrom string
	rcpts    []string
	seen     map[string]struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

type submissionSession struct {
	backend *submissionBackend
	conn    *smtp.Conn
	clamp   *clampConn

	// mu guards what Logout (from the server's close path) can touch.
	mu           sync.Mutex
	auth         *Credential
	authAttempts int
	rejected     int
	transactions int
	closed       bool
	txn          *submissionTxn
}

func (b *submissionBackend) NewSession(conn *smtp.Conn) (smtp.Session, error) {
	s := &submissionSession{backend: b, conn: conn, clamp: findClamp(conn.Conn())}
	if s.clamp != nil {
		// A new session (after STARTTLS) starts unauthenticated.
		s.clamp.logout()
	}
	return s, nil
}

// findClamp reaches the clampConn through the TLS layer.
func findClamp(c net.Conn) *clampConn {
	for i := 0; i < 4 && c != nil; i++ {
		switch t := c.(type) {
		case *clampConn:
			return t
		case *tls.Conn:
			c = t.NetConn()
		default:
			return nil
		}
	}
	return nil
}

func (s *submissionSession) count(name string) {
	if st := s.backend.cfg.Stats; st != nil {
		st.Count(name)
	}
}

func (s *submissionSession) clientIP() netip.Addr {
	if s.conn == nil || s.conn.Conn() == nil {
		return netip.Addr{}
	}
	if host, _, err := net.SplitHostPort(s.conn.Conn().RemoteAddr().String()); err == nil {
		if a, err := netip.ParseAddr(host); err == nil {
			return a.Unmap()
		}
	}
	return netip.Addr{}
}

func (s *submissionSession) tlsVersion() string {
	if s.conn != nil {
		if state, ok := s.conn.TLSConnectionState(); ok {
			return tls.VersionName(state.Version)
		}
	}
	return ""
}

// closeConnection answers the current command (the caller returns the 421) and
// closes the connection shortly after the reply is written. go-smtp does not
// close on a 421 returned by a session.
func (s *submissionSession) closeConnection() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.count("connection_closed_abuse")
	if conn := s.conn; conn != nil {
		time.AfterFunc(250*time.Millisecond, func() { _ = conn.Close() })
	}
}

func abuseClose(s *submissionSession) error {
	s.closeConnection()
	return tooManyErrors()
}

// endTransaction cancels and clears the transaction. Callers hold s.mu.
func (s *submissionSession) endTransactionLocked() {
	if s.txn != nil {
		s.txn.cancel()
		s.txn = nil
	}
}

func (s *submissionSession) Reset() {
	s.mu.Lock()
	s.endTransactionLocked()
	s.mu.Unlock()
	if s.clamp != nil {
		s.clamp.endTransaction()
	}
}

func (s *submissionSession) Logout() error {
	s.mu.Lock()
	s.auth = nil
	s.endTransactionLocked()
	s.mu.Unlock()
	if s.clamp != nil {
		s.clamp.logout()
	}
	return nil
}

func (s *submissionSession) Mail(from string, opts *smtp.MailOptions) error {
	s.mu.Lock()
	s.endTransactionLocked()
	if s.clamp != nil {
		s.clamp.endTransaction()
	}
	switch {
	case s.closed:
		s.mu.Unlock()
		return tooManyErrors()
	case s.auth == nil:
		s.mu.Unlock()
		s.count("mail_unauthenticated")
		return &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Authentication required"}
	case s.rejected >= maxRejectedTransfers:
		s.mu.Unlock()
		return abuseClose(s)
	}
	s.mu.Unlock()
	if opts != nil && opts.Size > s.backend.cfg.maxBytes {
		s.count("mail_too_large")
		return tooLarge()
	}
	if !s.backend.cfg.Submission.Available() {
		s.count("mail_no_session")
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary failure, try again later"}
	}
	s.mu.Lock()
	s.transactions++
	over := s.transactions > maxSubmissionTransactions
	s.mu.Unlock()
	if over {
		return abuseClose(s)
	}
	if len(from) > maxAddressBytes || !printableASCII(from) {
		return &smtp.SMTPError{Code: 501, EnhancedCode: smtp.EnhancedCode{5, 1, 7}, Message: "Bad sender address syntax"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.txn = &submissionTxn{mailFrom: from, seen: make(map[string]struct{}), ctx: ctx, cancel: cancel}
	s.mu.Unlock()
	if s.clamp != nil {
		s.clamp.beginTransaction()
	}
	return nil
}

// Rcpt checks only the syntax, the 100 cap and duplicates. The recipient
// allowlist is enforced by control at DATA.
func (s *submissionSession) Rcpt(address string, _ *smtp.RcptOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return tooManyErrors()
	}
	txn := s.txn
	if txn == nil {
		return &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "Need MAIL before RCPT"}
	}
	local, domain, ok := splitAddress(address)
	if !ok {
		return &smtp.SMTPError{Code: 501, EnhancedCode: smtp.EnhancedCode{5, 1, 3}, Message: "Bad recipient address syntax"}
	}
	key := local + "@" + domain
	if _, dup := txn.seen[key]; dup {
		return nil
	}
	if len(txn.rcpts) >= maxSubmissionRecipients {
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 5, 3}, Message: "Too many recipients"}
	}
	txn.seen[key] = struct{}{}
	txn.rcpts = append(txn.rcpts, key)
	return nil
}

func roundUpChunk(n int64) int64 {
	return (n + submissionChunk - 1) / submissionChunk * submissionChunk
}

// refuse marks an early refusal of DATA: it counts against the connection and
// puts the rest of the stream into the bounded drain, then returns err. Only
// the captured transaction still being the session's current one counts.
func (s *submissionSession) refuse(txn *submissionTxn, err error) error {
	s.mu.Lock()
	if s.txn == txn {
		s.rejected++
	}
	s.mu.Unlock()
	if s.clamp != nil {
		s.clamp.beginDrain()
	}
	return err
}

// fill reads until buf is full or the stream ends. eof reports a clean end.
func fill(r io.Reader, buf []byte) (n int, eof bool, err error) {
	for n < len(buf) {
		m, rerr := r.Read(buf[n:])
		n += m
		if rerr == io.EOF {
			return n, true, nil
		}
		if rerr != nil {
			return n, false, rerr
		}
	}
	return n, false, nil
}

var errBodyTooLarge = errors.New("message exceeds the submission limit")

// readBody reads the decoded message into memory. held is the number of budget
// bytes still reserved afterwards. A body of up to one chunk never allocates
// the full-size buffer; the unused part of the reservation is released.
func (s *submissionSession) readBody(r io.Reader, limit int64, held *int64, release func(int64)) ([]byte, error) {
	first := make([]byte, submissionChunk)
	n, eof, err := fill(r, first)
	if err != nil {
		return nil, err
	}
	if eof {
		release(*held - submissionChunk)
		*held = submissionChunk
		return first[:n:n], nil
	}
	body := make([]byte, n, limit)
	copy(body, first)
	for int64(len(body)) < limit {
		m, eof, err := fill(r, body[len(body):limit])
		body = body[:len(body)+m]
		if err != nil {
			return nil, err
		}
		if eof {
			return s.trim(body, held, release), nil
		}
	}
	// Exactly limit bytes so far: a clean end means exactly the limit, any
	// further decoded byte means too large.
	var probe [1]byte
	m, eof, err := fill(r, probe[:])
	if err != nil {
		return nil, err
	}
	if m > 0 || !eof {
		return nil, errBodyTooLarge
	}
	return body, nil
}

// trim gives back the unused part of the reservation. A small message is
// copied out of the full-size buffer, so the memory is really freed.
func (s *submissionSession) trim(body []byte, held *int64, release func(int64)) []byte {
	need := roundUpChunk(int64(len(body)))
	if need >= *held/2 {
		return body
	}
	small := make([]byte, len(body), need)
	copy(small, body)
	release(*held - need)
	*held = need
	return small
}

func (s *submissionSession) Data(r io.Reader) error {
	s.mu.Lock()
	txn, auth := s.txn, s.auth
	s.mu.Unlock()
	if txn == nil || auth == nil || len(txn.rcpts) == 0 {
		return &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "Bad sequence of commands"}
	}
	cfg := s.backend.cfg
	if !cfg.Submission.Available() {
		s.count("data_no_session")
		return s.refuse(txn, temporary())
	}
	held := roundUpChunk(cfg.maxBytes)
	waitCtx, cancelWait := context.WithTimeout(txn.ctx, cfg.budgetWait)
	err := cfg.Budget.Acquire(waitCtx, held)
	cancelWait()
	if err != nil {
		if txn.ctx.Err() != nil {
			return temporary()
		}
		s.count("data_budget_timeout")
		return s.refuse(txn, temporary())
	}
	release := func(n int64) {
		if n > 0 {
			cfg.Budget.Release(n)
		}
	}
	defer func() { release(held) }()

	body, err := s.readBody(r, cfg.maxBytes, &held, release)
	switch {
	case errors.Is(err, smtp.ErrDataTooLarge), errors.Is(err, errBodyTooLarge):
		s.count("data_too_large")
		return s.refuse(txn, tooLarge())
	case errors.Is(err, smtp.ErrTooLongLine):
		s.count("data_line_too_long")
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "Message content not supported"}
	case err != nil:
		// A read error, a closed connection or a cancelled transaction is
		// cleanup, not an early refusal.
		s.count("data_tempfail")
		return temporary()
	case len(body) == 0:
		s.count("data_empty")
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "Message is empty"}
	}

	message := SubmissionMessage{Credential: *auth, MailFrom: txn.mailFrom, Rcpts: append([]string(nil), txn.rcpts...), ClientIP: s.clientIP(),
		TLSVersion: s.tlsVersion(), Received: time.Now(), Port: s.backend.listener.Port, Body: body}
	ctx, cancel := context.WithTimeout(txn.ctx, submissionTimeout)
	defer cancel()
	reply, err := cfg.Submission.Submit(ctx, message)
	if err != nil {
		s.count("data_tempfail")
		return temporary()
	}
	switch {
	case reply.Code == 250:
		s.count("data_accept")
	case reply.Code >= 400 && reply.Code < 500:
		s.count("data_tempfail")
	default:
		s.count("data_rejected")
	}
	if reply.Close {
		s.closeConnection()
	}
	return reply.err()
}
