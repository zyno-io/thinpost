package controlsession

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/smtpintake"
	"github.com/zyno-io/thinpost/internal/submission"
	"golang.org/x/time/rate"
)

// Submission is the trusted intake of SMTP submissions. *smtpintake.Service
// implements it. Submit must return smtpintake.Accepted only after the message
// is durably committed under the given fence.
type Submission interface {
	Verify(ctx context.Context, id, secret string, clientIP netip.Addr) smtpintake.AuthResult
	Submit(ctx context.Context, fence submission.Fence, m smtpintake.Message) smtpintake.Result
}

const (
	// DefaultSubmissionPartialTTL is how long control keeps a submission the
	// worker has not finished sending. The worker waits at most 60 seconds, so
	// anything older is abandoned.
	DefaultSubmissionPartialTTL = 70 * time.Second
	// SubmissionTimeout bounds one submission from its start to its commit.
	SubmissionTimeout = 45 * time.Second
	// authTimeout bounds one credential check; the worker waits 10 seconds.
	authTimeout = 10 * time.Second
	// Per-session credential check rate: 20 per second, bursts of 40.
	authRate  = 20
	authBurst = 40
)

// byteBudget is the process-wide memory budget of the submission lane. It never
// waits: an over-budget caller is refused at once.
type byteBudget struct {
	mu          sync.Mutex
	used, limit int64
	// acquires and releases count reservations, for tests.
	acquires, releases int64
}

func (b *byteBudget) acquire(n int64) *reservation {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > b.limit-b.used {
		return nil
	}
	b.used += n
	b.acquires++
	return &reservation{budget: b, n: n}
}

func (b *byteBudget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// reservation is a held share of the budget. release is idempotent, so the
// owner that finally gives it back does so exactly once however ownership
// moved: the frame loop owns it while the message assembles, the worker after.
type reservation struct {
	once   sync.Once
	budget *byteBudget
	n      int64
}

func (r *reservation) release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.budget.mu.Lock()
		r.budget.used -= r.n
		r.budget.releases++
		r.budget.mu.Unlock()
	})
}

// submissionBudget returns the Runner-owned budget, created on first use. The
// bridge reuses one Runner across reconnects, so the budget (and the bytes of
// workers still unwinding from an earlier stream) is shared by every session.
func (r *Runner) submissionBudget() *byteBudget {
	budgetInit.Lock()
	defer budgetInit.Unlock()
	if r.budget == nil {
		limit := r.SubmissionBudget
		if limit <= 0 {
			limit = edgeprotocol.ControlSubmissionBudget
		}
		r.budget = &byteBudget{limit: limit}
	}
	return r.budget
}

var budgetInit sync.Mutex

// submissionItem is one submission being received. A refused item holds no data
// and no reservation: its chunks are counted and dropped and the message is
// answered with verdict.
type submissionItem struct {
	start    *controlv1.SubmissionStart
	asm      *edgeprotocol.ChunkAssembler
	res      *reservation
	verdict  controlv1.SubmissionVerdict
	received uint64
	began    time.Time
}

// submissionLane is the per-session state of the submission lane. It is used
// only from the session's frame loop.
type submissionLane struct {
	r           *Runner
	budget      *byteBudget
	items       map[string]*submissionItem
	closed      map[string]struct{}
	closedOrder []string
	evicted     bool
	processing  int
	authPending int
	limiter     *rate.Limiter
	ttl         time.Duration
}

func newSubmissionLane(r *Runner) *submissionLane {
	ttl := r.SubmissionPartialTTL
	if ttl <= 0 {
		ttl = DefaultSubmissionPartialTTL
	}
	return &submissionLane{r: r, budget: r.submissionBudget(), items: make(map[string]*submissionItem), closed: make(map[string]struct{}), limiter: rate.NewLimiter(authRate, authBurst), ttl: ttl}
}

func (l *submissionLane) assembling() int {
	n := 0
	for _, item := range l.items {
		if item.verdict == controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNSPECIFIED {
			n++
		}
	}
	return n
}

// idle reports that nothing is in flight, for the drain condition.
func (l *submissionLane) idle() bool {
	return len(l.items) == 0 && l.processing == 0 && l.authPending == 0
}

// releaseAll gives back the reservations of items still assembling. Items
// handed to a worker are not in the table; the worker releases its own.
func (l *submissionLane) releaseAll() {
	for _, item := range l.items {
		item.res.release()
	}
}

func (l *submissionLane) forget(id string) {
	if len(l.closedOrder) >= edgeprotocol.MaxSubmissionClosed {
		delete(l.closed, l.closedOrder[0])
		l.closedOrder = l.closedOrder[1:]
		l.evicted = true
	}
	l.closed[id] = struct{}{}
	l.closedOrder = append(l.closedOrder, id)
}

func (l *submissionLane) unforget(id string) {
	if _, ok := l.closed[id]; !ok {
		return
	}
	delete(l.closed, id)
	for i, v := range l.closedOrder {
		if v == id {
			l.closedOrder = append(l.closedOrder[:i], l.closedOrder[i+1:]...)
			break
		}
	}
}

// expire drops partials the worker abandoned (they would otherwise hold a slot,
// a reservation and a drain forever) and remembers them as stragglers.
func (l *submissionLane) expire() {
	for id, item := range l.items {
		if time.Since(item.began) > l.ttl {
			item.res.release()
			delete(l.items, id)
			l.forget(id)
		}
	}
}

// begin registers a SubmissionStart that passed framing. It reserves the
// message's share of the budget without waiting; a start that cannot be served
// is answered with a verdict once its chunks have been swallowed.
func (l *submissionLane) begin(start *controlv1.SubmissionStart, draining bool) error {
	if l.items[start.SubmissionId] != nil || len(l.items) >= 4*edgeprotocol.MaxSubmissionItems {
		return errors.New("submission not available")
	}
	item := &submissionItem{start: start, began: time.Now()}
	switch {
	case edgeprotocol.SubmissionStartVerdict(start) != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNSPECIFIED:
		item.verdict = edgeprotocol.SubmissionStartVerdict(start)
	case draining || l.assembling()+l.processing >= edgeprotocol.MaxSubmissionItems:
		// More than the cap in flight means the worker's count has drifted from
		// ours (it gave up on one we still hold). Refuse with a tempfail,
		// swallowing the chunks, and keep the session unless it is far off.
		item.verdict = controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL
	default:
		item.res = l.budget.acquire(edgeprotocol.SubmissionWeight(start.ExpectedSize))
		if item.res == nil {
			item.verdict = controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL
		} else {
			var err error
			if item.asm, err = edgeprotocol.NewChunkAssembler(start.SubmissionId, start.ExpectedSize, start.Sha256); err != nil {
				item.res.release()
				return err
			}
		}
	}
	l.unforget(start.SubmissionId)
	l.items[start.SubmissionId] = item
	return nil
}

// chunkResult is what a chunk completed, if anything.
type chunkResult struct {
	item *submissionItem
	data []byte // set when a message to process completed
}

// add feeds one chunk. A nil item means the chunk was absorbed (or was a
// straggler of an abandoned item). A non-nil item with nil data is a refused
// item that finished; with data it is a complete message whose reservation the
// caller now owns.
func (l *submissionLane) add(chunk *controlv1.DataChunk) (chunkResult, error) {
	item := l.items[chunk.GetId()]
	if item == nil {
		if _, ok := l.closed[chunk.GetId()]; ok {
			return chunkResult{}, nil
		}
		if l.evicted {
			return chunkResult{}, errors.New("submission straggler overflow")
		}
		return chunkResult{}, errors.New("unexpected submission chunk")
	}
	if item.asm == nil {
		size := item.start.ExpectedSize
		if chunk.Offset != item.received || len(chunk.Data) == 0 || len(chunk.Data) > edgeprotocol.MaxChunkBytes || uint64(len(chunk.Data)) > size-item.received {
			return chunkResult{}, errors.New("invalid submission chunk")
		}
		item.received += uint64(len(chunk.Data))
		if chunk.Last != (item.received == size) {
			return chunkResult{}, errors.New("invalid final submission chunk")
		}
		if !chunk.Last {
			return chunkResult{}, nil
		}
		delete(l.items, item.start.SubmissionId)
		return chunkResult{item: item}, nil
	}
	data, err := item.asm.Add(chunk)
	if err != nil {
		return chunkResult{}, err
	}
	if data == nil {
		return chunkResult{}, nil
	}
	delete(l.items, item.start.SubmissionId)
	return chunkResult{item: item, data: data}, nil
}

func submissionMessage(start *controlv1.SubmissionStart, body []byte) smtpintake.Message {
	m := smtpintake.Message{
		TransferID: start.SubmissionId, CredentialID: start.CredentialId, Secret: start.Secret, MailFrom: start.MailFrom, Rcpts: start.RcptTo,
		TLS: start.TlsVersion != "", Received: time.UnixMilli(start.ReceivedUnixMs), Body: body,
	}
	if start.ClientIp != "" {
		m.ClientIP, _ = netip.ParseAddr(start.ClientIp)
	}
	return m
}

// process runs the committed-or-not decision of a complete message on a worker
// goroutine. The worker owns the item's reservation and releases it when it
// exits, even if the session has ended meanwhile, so the bytes of a worker that
// is still unwinding stay counted against the process-wide budget.
func (l *submissionLane) process(ctx context.Context, item *submissionItem, body []byte, fence submission.Fence, done chan<- submissionDone) {
	l.processing++
	res, start := item.res, item.start
	deadline := item.began.Add(SubmissionTimeout)
	intake := l.r.Submission
	go func() {
		defer res.release()
		runCtx, stop := context.WithDeadline(ctx, deadline)
		defer stop()
		result := intake.Submit(runCtx, fence, submissionMessage(start, body))
		select {
		case done <- submissionDone{id: start.SubmissionId, result: result}:
		case <-ctx.Done():
		}
	}()
}

type submissionDone struct {
	id     string
	result smtpintake.Result
}

type authDone struct {
	id      string
	verdict controlv1.SubmissionAuthVerdict
}

// verify handles one SubmissionAuth. It returns a verdict to send at once, or
// zero when a worker was started whose result will arrive on done.
func (l *submissionLane) verify(ctx context.Context, auth *controlv1.SubmissionAuth, draining bool, done chan<- authDone) controlv1.SubmissionAuthVerdict {
	switch {
	case draining:
		return controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL
	case !edgeprotocol.ValidSubmissionAuth(auth):
		return controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_INVALID
	case l.authPending >= edgeprotocol.MaxAuthPending || !l.limiter.Allow():
		return controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_THROTTLED
	}
	l.authPending++
	intake := l.r.Submission
	go func() {
		verifyCtx, stop := context.WithTimeout(ctx, authTimeout)
		defer stop()
		ip, _ := netip.ParseAddr(auth.ClientIp)
		verdict := authVerdict(intake.Verify(verifyCtx, auth.CredentialId, auth.Secret, ip))
		select {
		case done <- authDone{id: auth.RequestId, verdict: verdict}:
		case <-ctx.Done():
		}
	}()
	return controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_UNSPECIFIED
}

// authVerdict maps an intake result onto the wire. The zero value and anything
// unknown is a temporary failure, never a success.
func authVerdict(r smtpintake.AuthResult) controlv1.SubmissionAuthVerdict {
	switch r {
	case smtpintake.AuthOK:
		return controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK
	case smtpintake.AuthInvalid:
		return controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_INVALID
	case smtpintake.AuthUnavailable:
		return controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_UNAVAILABLE
	}
	return controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL
}

// submissionVerdict maps an intake outcome onto the wire. The zero value and
// anything unknown is a temporary failure, never a success.
func submissionVerdict(o smtpintake.Outcome) controlv1.SubmissionVerdict {
	switch o {
	case smtpintake.Accepted:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED
	case smtpintake.RateLimited:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_RATE_LIMITED
	case smtpintake.QuotaExceeded:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_QUOTA_EXCEEDED
	case smtpintake.Backlog:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_BACKLOG
	case smtpintake.Unavailable:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNAVAILABLE
	case smtpintake.CredentialInvalid:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_CREDENTIAL_INVALID
	case smtpintake.AppPaused:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_APP_PAUSED
	case smtpintake.FromNotPermitted:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_FROM_NOT_PERMITTED
	case smtpintake.MessageInvalid:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MESSAGE_INVALID
	case smtpintake.TooLarge:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TOO_LARGE
	case smtpintake.RecipientNotPermitted:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_RECIPIENT_NOT_PERMITTED
	case smtpintake.EncryptionRequired:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ENCRYPTION_REQUIRED
	case smtpintake.Misconfigured:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MISCONFIGURED
	case smtpintake.ContentNotSupported:
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_CONTENT_NOT_SUPPORTED
	}
	return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL
}

// submissionAck builds the acknowledgement for a finished submission. The
// message ID and the held flag belong to ACCEPTED only.
func submissionAck(id string, result smtpintake.Result) *controlv1.ControlFrame {
	ack := &controlv1.SubmissionAcknowledgement{SubmissionId: id, Verdict: submissionVerdict(result.Outcome)}
	if ack.Verdict == controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED {
		ack.MessageId, ack.Held = result.MessageID, result.Held
	}
	return &controlv1.ControlFrame{Body: &controlv1.ControlFrame_SubmissionAck{SubmissionAck: ack}}
}

func refusalAck(id string, verdict controlv1.SubmissionVerdict) *controlv1.ControlFrame {
	return &controlv1.ControlFrame{Body: &controlv1.ControlFrame_SubmissionAck{SubmissionAck: &controlv1.SubmissionAcknowledgement{SubmissionId: id, Verdict: verdict}}}
}

func authResultFrame(id string, verdict controlv1.SubmissionAuthVerdict) *controlv1.ControlFrame {
	return &controlv1.ControlFrame{Body: &controlv1.ControlFrame_AuthResult{AuthResult: &controlv1.SubmissionAuthResult{RequestId: id, Verdict: verdict}}}
}
