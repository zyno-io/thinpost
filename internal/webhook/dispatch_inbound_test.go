package webhook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

type inboundRepo struct {
	claim    Claim
	claimErr error
	status   int
	commits  int
	failed   []string
	deferred []time.Duration
	// ctxErr is the context error seen by the last finishing call.
	ctxErr error
	ttl    time.Duration
}

func (r *inboundRepo) DeferWebhook(ctx context.Context, _ Claim, delay time.Duration) error {
	r.deferred = append(r.deferred, delay)
	r.ctxErr = ctx.Err()
	return nil
}

func (r *inboundRepo) ClaimWebhookInbound(_ context.Context, token string, ttl time.Duration) (Claim, error) {
	r.claim.Token = token
	r.ttl = ttl
	return r.claim, r.claimErr
}
func (r *inboundRepo) CommitWebhook(_ context.Context, _ Claim, status int) error {
	r.status = status
	r.commits++
	return nil
}
func (r *inboundRepo) FailWebhook(_ context.Context, _ Claim, class string) error {
	r.failed = append(r.failed, class)
	return nil
}

type fakeObjects struct {
	data  []byte
	err   error
	calls int
	max   int64
}

func (o *fakeObjects) GetMIME(_ context.Context, _ string, maximum int64) ([]byte, error) {
	o.calls++
	o.max = maximum
	return o.data, o.err
}

const inboundMail = "From: Alice <alice@sender.example>\r\nTo: reply+t1@mail.example.com\r\nSubject: Re: hello\r\nMessage-ID: <r1@sender.example>\r\nAuthentication-Results: smtp.example.com; spf=pass\r\n\r\nthe reply\r\n"

func inboundClaim() Claim {
	yes := true
	return Claim{AppID: "app-a", SubscriptionID: "hook-a", Endpoint: "https://hooks.example/", Format: format.ThinpostInbound, SealedSecret: []byte("sealed"),
		Event: Event{ID: "event-1", Outcome: format.InboundReceived, OccurredAt: time.Now()},
		Inbound: &InboundData{InboundID: "0123456789abcdef0123456789abcdef", Recipient: "reply+t1@mail.example.com", Recipients: []string{"reply+t1@mail.example.com"}, ReceivedAt: time.Now(), ObjectKey: "inbound/v1/1/x", Size: len(inboundMail),
			MailFrom: "alice@sender.example", ClientIP: "198.51.100.7", Helo: "mail.sender.example", Receiver: "smtp.example.com", SPF: "pass", DKIM: "none", DMARC: "none",
			MessageID: "m1", DeliveryID: "d1", Header: "in_reply_to", FromMatches: &yes}}
}

func newInbound(repo *inboundRepo, objects *fakeObjects, sender *fakeDelivery) *InboundDispatcher {
	return &InboundDispatcher{Repository: repo, Sender: sender, Keys: &fakeKeys{}, Objects: objects, Budget: NewRenderBudget(RenderBudgetBytes)}
}

func TestInboundDispatchSignsAndDeliversCorrelatedPayload(t *testing.T) {
	repo := &inboundRepo{claim: inboundClaim()}
	objects := &fakeObjects{data: []byte(inboundMail)}
	var got format.Request
	sender := &capturingSender{status: 200, request: &got}
	dispatcher := newInbound(repo, objects, nil)
	dispatcher.Sender = sender
	if err := dispatcher.DispatchNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.ttl != InboundClaimTTL || repo.commits != 1 || repo.status != 200 || len(repo.failed) != 0 {
		t.Fatal("claim or commit", repo.ttl, repo.commits, repo.status, repo.failed)
	}
	secret := []byte("0123456789abcdef0123456789abcdef")
	if !format.VerifyThinpost(secret, "event-1", got.Header.Get("X-Thinpost-Timestamp"), got.Body, got.Header.Get("X-Thinpost-Signature"), time.Now(), time.Minute) {
		t.Fatal("the delivery is not signed like a native event")
	}
	var doc struct {
		Inbound struct {
			Raw         string
			Correlation struct {
				Matched             bool
				DeliveryID          string `json:"delivery_id"`
				FromMatchesRecipent bool   `json:"from_matches_recipient"`
			}
		}
	}
	if err := json.Unmarshal(got.Body, &doc); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(doc.Inbound.Raw)
	if !doc.Inbound.Correlation.Matched || doc.Inbound.Correlation.DeliveryID != "d1" || !doc.Inbound.Correlation.FromMatchesRecipent {
		t.Fatal("correlation", doc.Inbound.Correlation)
	}
	if !strings.HasPrefix(string(raw), "Authentication-Results: smtp.example.com;") || strings.Count(string(raw), "smtp.example.com; spf=pass\r\n") != 0 {
		t.Fatal("results not prepended or a forged claim survived:\n" + string(raw))
	}
	if objects.max != maxObject {
		t.Fatal("object read is unbounded", objects.max)
	}
}

type capturingSender struct {
	status  int
	request *format.Request
}

func (s *capturingSender) Send(_ context.Context, _ string, request format.Request, _ *admin.WebhookAuth) (Result, error) {
	*s.request = request
	return Result{StatusCode: s.status, Success: s.status/100 == 2}, nil
}

func TestInboundMissingContentIsAPermanentClassAndNotSent(t *testing.T) {
	for name, tc := range map[string]struct {
		expired bool
		err     error
	}{"missing object": {false, ErrContentMissing}, "past retention": {true, nil}} {
		t.Run(name, func(t *testing.T) {
			claim := inboundClaim()
			claim.Inbound.Expired = tc.expired
			repo := &inboundRepo{claim: claim}
			objects := &fakeObjects{err: tc.err}
			sender := &fakeDelivery{status: 200}
			if err := newInbound(repo, objects, sender).DispatchNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(repo.failed) != 1 || repo.failed[0] != ClassContentUnavailable || repo.commits != 0 || sender.payload != nil {
				t.Fatal("not a permanent content_unavailable", repo.failed, repo.commits)
			}
			if tc.expired && objects.calls != 0 {
				t.Fatal("expired content must not be read")
			}
		})
	}
}

func TestBusyRenderBudgetDefersWithoutAnAttempt(t *testing.T) {
	repo := &inboundRepo{claim: inboundClaim()}
	objects := &fakeObjects{data: []byte(inboundMail)}
	sender := &fakeDelivery{status: 200}
	dispatcher := newInbound(repo, objects, sender)
	dispatcher.BudgetWait = 20 * time.Millisecond
	dispatcher.Budget = NewRenderBudget(1 << 20) // too small for any message: the wait ends
	busy, _ := dispatcher.Budget.Acquire(context.Background(), 1<<20, time.Second)
	defer busy()
	if err := dispatcher.DispatchNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.deferred) != 1 || repo.deferred[0] != BudgetDeferral || repo.commits != 0 || len(repo.failed) != 0 || sender.payload != nil || objects.calls != 0 {
		t.Fatal("a busy budget must defer without committing an attempt", repo.deferred, repo.commits, repo.failed)
	}
}

type blockingObjects struct{ started chan struct{} }

func (o *blockingObjects) GetMIME(ctx context.Context, _ string, _ int64) ([]byte, error) {
	close(o.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestShutdownDuringTheBudgetWaitOrTheObjectReadDefersOnAFreshContext(t *testing.T) {
	t.Run("budget wait", func(t *testing.T) {
		repo := &inboundRepo{claim: inboundClaim()}
		dispatcher := newInbound(repo, &fakeObjects{data: []byte(inboundMail)}, &fakeDelivery{status: 200})
		dispatcher.Budget = NewRenderBudget(1 << 20)
		busy, _ := dispatcher.Budget.Acquire(context.Background(), 1<<20, time.Second)
		defer busy()
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(30 * time.Millisecond); cancel() }()
		if err := dispatcher.DispatchNext(ctx); err != nil {
			t.Fatal(err)
		}
		if len(repo.deferred) != 1 || repo.ctxErr != nil || repo.commits != 0 || len(repo.failed) != 0 {
			t.Fatal("shutdown must defer on a live context", repo.deferred, repo.ctxErr, repo.commits)
		}
	})
	t.Run("object read", func(t *testing.T) {
		repo := &inboundRepo{claim: inboundClaim()}
		objects := &blockingObjects{started: make(chan struct{})}
		dispatcher := newInbound(repo, nil, &fakeDelivery{status: 200})
		dispatcher.Objects = objects
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-objects.started; cancel() }()
		if err := dispatcher.DispatchNext(ctx); err != nil {
			t.Fatal(err)
		}
		if len(repo.deferred) != 1 || repo.ctxErr != nil || repo.commits != 0 {
			t.Fatal("shutdown during the read must defer on a live context", repo.deferred, repo.ctxErr, repo.commits)
		}
	})
}

func TestInboundStorageErrorIsRetried(t *testing.T) {
	repo := &inboundRepo{claim: inboundClaim()}
	sender := &fakeDelivery{status: 200}
	if err := newInbound(repo, &fakeObjects{err: errors.New("storage is down")}, sender).DispatchNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.failed) != 0 || repo.commits != 1 || repo.status != 0 || sender.payload != nil {
		t.Fatal("a storage error must be a plain failed attempt", repo.failed, repo.commits, repo.status)
	}
}

func TestInboundIsolationFromOutboundClaims(t *testing.T) {
	// A claim without recorded inbound data is refused, never sent.
	claim := inboundClaim()
	claim.Inbound = nil
	repo := &inboundRepo{claim: claim}
	if err := newInbound(repo, &fakeObjects{data: []byte(inboundMail)}, &fakeDelivery{status: 200}).DispatchNext(context.Background()); err == nil {
		t.Fatal("accepted a claim without inbound data")
	}
	// An outbound endpoint format on the inbound lane is not rendered.
	claim = inboundClaim()
	claim.Format = format.Thinpost
	repo = &inboundRepo{claim: claim}
	sender := &fakeDelivery{status: 200}
	if err := newInbound(repo, &fakeObjects{data: []byte(inboundMail)}, sender).DispatchNext(context.Background()); err != nil || sender.payload != nil || repo.commits != 1 || repo.status != 0 {
		t.Fatal("rendered an outbound format on the inbound lane")
	}
}

func TestInboundSkippedAndIdleClaims(t *testing.T) {
	repo := &inboundRepo{claimErr: ErrSkipped}
	if err := newInbound(repo, &fakeObjects{}, &fakeDelivery{}).DispatchNext(context.Background()); err != nil {
		t.Fatal("skipped is not an error")
	}
	repo = &inboundRepo{claimErr: ErrNoWork}
	if err := newInbound(repo, &fakeObjects{}, &fakeDelivery{}).DispatchNext(context.Background()); !errors.Is(err, ErrNoWork) {
		t.Fatal("idle must report no work")
	}
}

func TestRenderBudgetBoundsMemoryAndWaitsBriefly(t *testing.T) {
	b := NewRenderBudget(100)
	first, err := b.Acquire(context.Background(), 60, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Does not fit beside the first: refused once the wait ends.
	start := time.Now()
	if _, err = b.Acquire(context.Background(), 60, 50*time.Millisecond); !errors.Is(err, ErrBudgetBusy) || time.Since(start) < 40*time.Millisecond {
		t.Fatal("a second large render must wait and then be refused", err)
	}
	// A smaller one fits.
	small, err := b.Acquire(context.Background(), 40, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// A waiter proceeds when memory is released.
	done := make(chan error, 1)
	go func() {
		release, err := b.Acquire(context.Background(), 60, 5*time.Second)
		if err == nil {
			release()
		}
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	first()
	first() // releasing twice must not add budget twice
	small()
	if err = <-done; err != nil {
		t.Fatal("the waiter did not get the released memory", err)
	}
	// A request larger than the budget is capped to it, so a maximal message is
	// never starved forever.
	release, err := b.Acquire(context.Background(), 10_000, time.Second)
	if err != nil {
		t.Fatal("an oversized request must be capped to the budget", err)
	}
	if _, err = b.Acquire(context.Background(), 1, 20*time.Millisecond); !errors.Is(err, ErrBudgetBusy) {
		t.Fatal("the cap must be the whole budget")
	}
	release()
	ctx, cancel := context.WithCancel(context.Background())
	hold, _ := b.Acquire(context.Background(), 100, time.Second)
	cancel()
	if _, err = b.Acquire(ctx, 1, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatal("a cancelled wait must stop", err)
	}
	hold()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if release, err := b.Acquire(context.Background(), 30, 5*time.Second); err == nil {
				time.Sleep(time.Millisecond)
				release()
			}
		}()
	}
	wg.Wait()
	if r, err := b.Acquire(context.Background(), 100, time.Second); err != nil {
		t.Fatal("budget leaked", err)
	} else {
		r()
	}
}

func TestInboundSenderLimits(t *testing.T) {
	big := strings.Repeat("a", 2<<20)
	if _, err := (&Sender{}).Send(context.Background(), "https://hooks.example.com/", request(big), nil); err == nil {
		t.Fatal("the default sender must refuse a 2 MiB body")
	}
	sender, _, endpoint := setupServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		if n != int64(len(big)) {
			t.Errorf("received %d bytes", n)
		}
		w.WriteHeader(204)
	}))
	sender.MaxPayload, sender.Timeout = MaxInboundPayload, InboundTimeout
	result, err := sender.Send(context.Background(), endpoint, request(big), nil)
	if err != nil || !result.Success {
		t.Fatal("the inbound sender must carry a large body", result, err)
	}
	if _, err = sender.Send(context.Background(), endpoint, request(strings.Repeat("a", MaxInboundPayload+1)), nil); err == nil {
		t.Fatal("the inbound sender still has a limit")
	}
	// A receiver that answers slowly but within the inbound deadline succeeds,
	// where the 15 second default would not be tried here; the deadline itself
	// is the configured value.
	if InboundTimeout != 120*time.Second || InboundWorkers != 2 || RenderBudgetBytes != 48<<20 || InboundClaimTTL != 3*time.Minute {
		t.Fatal("inbound lane constants")
	}
}
