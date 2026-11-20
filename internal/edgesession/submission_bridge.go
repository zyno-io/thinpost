package edgesession

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/journal"
)

const (
	// verifyTimeout and submitTimeout bound how long the edge waits for control.
	verifyTimeout = 10 * time.Second
	submitTimeout = 60 * time.Second
)

var (
	// ErrSubmissionUnavailable means no authorized session that negotiated the
	// capability is attached, or the edge's own byte budget is spent. The SMTP
	// side answers with a temporary failure.
	ErrSubmissionUnavailable = errors.New("submission stream unavailable")
	// ErrSubmissionSessionClosed means the session ended or its lease changed
	// while a request was waiting.
	ErrSubmissionSessionClosed = errors.New("submission session closed")
)

// SubmissionCredential is an SMTP credential as the client sent it. Every
// formatting verb prints a placeholder so the secret cannot reach a log.
type SubmissionCredential struct{ ID, Secret string }

func (SubmissionCredential) String() string   { return "<smtp credential redacted>" }
func (SubmissionCredential) GoString() string { return "<smtp credential redacted>" }

// Format redacts under every verb, including %+v.
func (c SubmissionCredential) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }

// SubmissionMessage is one submission to send to control. Body is sent in
// place, not copied, and the stream may still be serializing it after Submit
// returns (a write abandoned on cancellation or timeout keeps running). Body
// is therefore immutable forever: never modify it, pool it or reuse it.
type SubmissionMessage struct {
	// ID is the 32-hex transfer ID; a random one is used when empty.
	ID         string
	Credential SubmissionCredential
	MailFrom   string
	Rcpts      []string
	ClientIP   netip.Addr
	// TLSVersion is the version name from the connection's real
	// tls.ConnectionState (for example "TLS 1.3"). Empty means the message did
	// not arrive over TLS; any non-empty value means it did, and control relies
	// on that for Enhanced Protection. Values not shaped like a TLS or SSL
	// version name are refused.
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

// SubmitResult is control's answer. Verdict is never UNSPECIFIED or unknown
// (those are TEMPFAIL), and ACCEPTED always carries a valid message ID.
type SubmitResult struct {
	Verdict   controlv1.SubmissionVerdict
	MessageID string
	Held      bool
}

// SubmissionClient is the SMTP side's view of the submission path.
type SubmissionClient interface {
	// Available reports whether a session able to serve submissions is attached.
	Available() bool
	// Verify checks a credential. The verdict is never UNSPECIFIED or unknown.
	Verify(ctx context.Context, c SubmissionCredential, clientIP netip.Addr) (controlv1.SubmissionAuthVerdict, error)
	// Submit sends one message. Its Body is immutable forever (see
	// SubmissionMessage). A permanent envelope or credential problem comes back
	// as a verdict, not an error.
	// Submit sends one message and waits for control's durable verdict. It has
	// no spool: an error means nothing was committed that the client has been
	// told about, and the SMTP side answers 451.
	Submit(ctx context.Context, m SubmissionMessage) (SubmitResult, error)
}

// SMTPReply is the fixed reply for a verdict. Control never sends free text.
type SMTPReply struct {
	Code     int
	Enhanced string
	Text     string
	// Close reports that the connection must be closed after the reply.
	Close bool
}

// ReplyFor maps a normalized result to its SMTP reply. An accepted result
// without a valid message ID is a temporary failure, never a 250.
func ReplyFor(r SubmitResult) SMTPReply {
	switch r.Verdict {
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED:
		if !edgeprotocol.ValidSubmissionID(r.MessageID) {
			break
		}
		if r.Held {
			return SMTPReply{250, "2.0.0", "OK: held as " + r.MessageID, false}
		}
		return SMTPReply{250, "2.0.0", "OK: queued as " + r.MessageID, false}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_RATE_LIMITED:
		return SMTPReply{451, "4.4.5", "Sending rate limit reached, try again later", false}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_QUOTA_EXCEEDED:
		return SMTPReply{452, "4.3.1", "Sending limit reached, try again later", false}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_BACKLOG:
		return SMTPReply{451, "4.3.1", "Too many messages are queued, try again later", false}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNAVAILABLE:
		return SMTPReply{421, "4.3.2", "Service not available, closing connection", true}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_CREDENTIAL_INVALID:
		return SMTPReply{550, "5.7.8", "Authentication credentials invalid", false}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_APP_PAUSED:
		return SMTPReply{550, "5.7.1", "Sending is paused for this app", false}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_FROM_NOT_PERMITTED:
		return SMTPReply{550, "5.7.1", "Sender address not permitted", false}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MESSAGE_INVALID:
		return SMTPReply{554, "5.6.0", "Message headers are invalid", false}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TOO_LARGE:
		return SMTPReply{552, "5.3.4", "Message too large", false}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_RECIPIENT_NOT_PERMITTED:
		return SMTPReply{550, "5.7.1", "Recipient not permitted", false}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ENCRYPTION_REQUIRED:
		return SMTPReply{550, "5.7.10", "Encryption required", false}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MISCONFIGURED:
		return SMTPReply{451, "4.3.5", "Sender configuration is not ready, try again later", false}
	case controlv1.SubmissionVerdict_SUBMISSION_VERDICT_CONTENT_NOT_SUPPORTED:
		return SMTPReply{554, "5.6.0", "Message content not supported", false}
	}
	return SMTPReply{451, "4.3.0", "Temporary failure, try again later", false}
}

// AuthReply maps a credential verdict to its SMTP reply.
func AuthReply(v controlv1.SubmissionAuthVerdict) SMTPReply {
	switch v {
	case controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK:
		return SMTPReply{235, "2.7.0", "Authentication successful", false}
	case controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_INVALID:
		return SMTPReply{535, "5.7.8", "Authentication credentials invalid", false}
	case controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_UNAVAILABLE:
		return SMTPReply{421, "4.3.2", "Service not available, closing connection", true}
	}
	return SMTPReply{454, "4.7.0", "Temporary authentication failure", false}
}

func normalizeVerdict(v controlv1.SubmissionVerdict) controlv1.SubmissionVerdict {
	if _, known := controlv1.SubmissionVerdict_name[int32(v)]; !known || v == controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNSPECIFIED {
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL
	}
	return v
}

func normalizeAuthVerdict(v controlv1.SubmissionAuthVerdict) controlv1.SubmissionAuthVerdict {
	if _, known := controlv1.SubmissionAuthVerdict_name[int32(v)]; !known || v == controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_UNSPECIFIED {
		return controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL
	}
	return v
}

// submissionBridge connects the SMTP submission listeners to the control
// stream. Like the other bridges it never opens a connection: only a
// trusted-initiated session that negotiated the capability and finished
// reconciliation can attach. It holds no spool; a message lives in memory
// (within the edge byte budget) until its acknowledgement.
type submissionBridge struct {
	mu      sync.Mutex
	enabled bool
	ports   []uint32
	fence   journal.Fence
	active  *submissionConnection
	auths   map[string]chan controlv1.SubmissionAuthVerdict
	items   map[string]chan *controlv1.SubmissionAcknowledgement
	used    int64
	// limit overrides edgeprotocol.EdgeSubmissionBudget, and the timeouts their
	// constants (tests).
	limit                          int64
	verifyTimeout, submitTimeout   time.Duration
	badAckIDs, droppedAcknowledges atomic.Int64
}

type submissionConnection struct {
	fence journal.Fence
	send  func(*controlv1.EdgeFrame) error
	valid func() bool
	done  chan struct{}
}

var _ SubmissionClient = (*submissionBridge)(nil)

func (b *submissionBridge) isEnabled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.enabled
}

func (b *submissionBridge) reportedPorts() []uint32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]uint32(nil), b.ports...)
}

func (b *submissionBridge) activate(fence journal.Fence) error {
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
func (b *submissionBridge) attach(fence journal.Fence, send func(*controlv1.EdgeFrame) error, valid func() bool) (*submissionConnection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fence != fence {
		return nil, journal.ErrStaleController
	}
	if b.active != nil {
		close(b.active.done)
	}
	c := &submissionConnection{fence: fence, send: send, valid: valid, done: make(chan struct{})}
	b.active = c
	b.auths = make(map[string]chan controlv1.SubmissionAuthVerdict)
	b.items = make(map[string]chan *controlv1.SubmissionAcknowledgement)
	return c, nil
}

// detach fails every waiter of the session.
func (b *submissionBridge) detach(c *submissionConnection) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active == c {
		b.active = nil
		close(c.done)
	}
}

// Available implements SubmissionClient.
func (b *submissionBridge) Available() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.enabled && b.active != nil && b.active.valid()
}

// acknowledgeAuth records control's answer to a credential check. An unknown ID
// or a stale or unauthorized session is ignored.
func (b *submissionBridge) acknowledgeAuth(c *submissionConnection, result *controlv1.SubmissionAuthResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != c || result == nil || !c.valid() {
		b.droppedAcknowledges.Add(1)
		return
	}
	if ch := b.auths[result.RequestId]; ch != nil {
		select {
		case ch <- result.Verdict:
		default:
		}
	}
}

// acknowledge records control's verdict for an in-flight message, with the same
// rules.
func (b *submissionBridge) acknowledge(c *submissionConnection, ack *controlv1.SubmissionAcknowledgement) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != c || ack == nil || !c.valid() {
		b.droppedAcknowledges.Add(1)
		return
	}
	if ch := b.items[ack.SubmissionId]; ch != nil {
		select {
		case ch <- ack:
		default:
		}
	}
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// Verify implements SubmissionClient. At most MaxAuthPending checks are in
// flight; more are THROTTLED without being sent.
func (b *submissionBridge) Verify(ctx context.Context, cred SubmissionCredential, clientIP netip.Addr) (controlv1.SubmissionAuthVerdict, error) {
	tempfail := controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL
	id, err := randomID()
	if err != nil {
		return tempfail, err
	}
	auth := &controlv1.SubmissionAuth{RequestId: id, CredentialId: cred.ID, Secret: cred.Secret}
	if clientIP.IsValid() {
		auth.ClientIp = clientIP.String()
	}
	if !edgeprotocol.ValidSubmissionAuth(auth) {
		return controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_INVALID, nil
	}
	b.mu.Lock()
	c := b.active
	if !b.enabled || c == nil || !c.valid() {
		b.mu.Unlock()
		return tempfail, nil
	}
	if len(b.auths) >= edgeprotocol.MaxAuthPending {
		b.mu.Unlock()
		return controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_THROTTLED, nil
	}
	result := make(chan controlv1.SubmissionAuthVerdict, 1)
	b.auths[id] = result
	timeout := b.verifyTimeout
	b.mu.Unlock()
	if timeout <= 0 {
		timeout = verifyTimeout
	}
	defer func() {
		b.mu.Lock()
		if b.auths[id] == result {
			delete(b.auths, id)
		}
		b.mu.Unlock()
	}()
	if err = c.send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Auth{Auth: auth}}); err != nil {
		return tempfail, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case verdict := <-result:
		if !c.valid() {
			return tempfail, nil
		}
		return normalizeAuthVerdict(verdict), nil
	case <-c.done:
		return tempfail, ErrSubmissionSessionClosed
	case <-timer.C:
		return tempfail, nil
	case <-ctx.Done():
		return tempfail, ctx.Err()
	}
}

func (b *submissionBridge) acquire(n int64) bool {
	limit := b.limit
	if limit <= 0 {
		limit = edgeprotocol.EdgeSubmissionBudget
	}
	if n > limit-b.used {
		return false
	}
	b.used += n
	return true
}

// Submit implements SubmissionClient.
func (b *submissionBridge) Submit(ctx context.Context, m SubmissionMessage) (SubmitResult, error) {
	id := m.ID
	if id == "" {
		var err error
		if id, err = randomID(); err != nil {
			return SubmitResult{}, err
		}
	}
	digest := sha256.Sum256(m.Body)
	start := &controlv1.SubmissionStart{
		SubmissionId: id, ExpectedSize: uint64(len(m.Body)), Sha256: digest[:], CredentialId: m.Credential.ID, Secret: m.Credential.Secret,
		MailFrom: m.MailFrom, RcptTo: m.Rcpts, TlsVersion: m.TLSVersion, ReceivedUnixMs: m.Received.UnixMilli(), Port: m.Port,
	}
	if m.ClientIP.IsValid() {
		start.ClientIp = m.ClientIP.String()
	}
	if !edgeprotocol.ValidSubmissionFraming(start) {
		return SubmitResult{}, errors.New("invalid submission")
	}
	if verdict := edgeprotocol.SubmissionStartVerdict(start); verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNSPECIFIED {
		// A permanent envelope or credential problem: answer it like control
		// would, instead of an error that makes the client retry forever.
		return SubmitResult{Verdict: verdict}, nil
	}
	b.mu.Lock()
	c := b.active
	if !b.enabled || c == nil || !c.valid() || len(b.items) >= edgeprotocol.MaxSubmissionItems {
		b.mu.Unlock()
		return SubmitResult{}, ErrSubmissionUnavailable
	}
	if _, exists := b.items[id]; exists {
		b.mu.Unlock()
		return SubmitResult{}, errors.New("duplicate submission id")
	}
	if !b.acquire(int64(len(m.Body))) {
		b.mu.Unlock()
		return SubmitResult{}, ErrSubmissionUnavailable
	}
	ack := make(chan *controlv1.SubmissionAcknowledgement, 1)
	b.items[id] = ack
	timeout := b.submitTimeout
	b.mu.Unlock()
	if timeout <= 0 {
		timeout = submitTimeout
	}
	defer func() {
		b.mu.Lock()
		if b.items[id] == ack {
			delete(b.items, id)
		}
		b.used -= int64(len(m.Body))
		b.mu.Unlock()
	}()
	if err := sendSubmission(ctx, c, id, start, m.Body); err != nil {
		return SubmitResult{}, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case got := <-ack:
		if !c.valid() {
			return SubmitResult{}, errors.New("controller lease expired")
		}
		result := SubmitResult{Verdict: normalizeVerdict(got.Verdict)}
		if result.Verdict == controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED {
			if !edgeprotocol.ValidSubmissionID(got.MessageId) {
				// The commit may have happened, but without an ID the client could
				// not be told which message it was. Tempfail, so it retries and the
				// replay window answers with the original ID.
				b.badAckIDs.Add(1)
				result.Verdict = controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL
			} else {
				result.MessageID, result.Held = got.MessageId, got.Held
			}
		}
		return result, nil
	case <-c.done:
		return SubmitResult{}, ErrSubmissionSessionClosed
	case <-timer.C:
		return SubmitResult{}, errors.New("submission acknowledgement timed out")
	case <-ctx.Done():
		return SubmitResult{}, ctx.Err()
	}
}

// sendSubmission sends the start and then the body's chunks. The chunks alias
// body: the stream marshals them before Send returns, and the caller must not
// modify body until Submit returns.
func sendSubmission(ctx context.Context, c *submissionConnection, id string, start *controlv1.SubmissionStart, body []byte) error {
	if !c.valid() {
		return errors.New("controller lease expired")
	}
	if err := c.send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Submission{Submission: start}}); err != nil {
		return err
	}
	for offset := 0; offset < len(body); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return ErrSubmissionSessionClosed
		default:
		}
		if !c.valid() {
			return errors.New("controller lease expired")
		}
		end := min(offset+edgeprotocol.MaxChunkBytes, len(body))
		chunk := &controlv1.DataChunk{Id: id, Offset: uint64(offset), Data: body[offset:end:end], Last: end == len(body)}
		if err := c.send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_SubmissionChunk{SubmissionChunk: chunk}}); err != nil {
			return err
		}
		offset = end
	}
	return nil
}
