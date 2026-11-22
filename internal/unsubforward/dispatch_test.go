package unsubforward

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/webhook"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

func TestClassify(t *testing.T) {
	for name, tc := range map[string]struct {
		result webhook.Result
		err    error
		want   Outcome
	}{
		"2xx":                            {webhook.Result{StatusCode: 200, Success: true}, nil, Outcome{Done: true, Status: 200}},
		"204":                            {webhook.Result{StatusCode: 204, Success: true}, nil, Outcome{Done: true, Status: 204}},
		"500 retried":                    {webhook.Result{StatusCode: 500}, nil, Outcome{Retry: true, Status: 500, Failure: FailureHTTPStatus}},
		"503 retried":                    {webhook.Result{StatusCode: 503}, nil, Outcome{Retry: true, Status: 503, Failure: FailureHTTPStatus}},
		"429 retried":                    {webhook.Result{StatusCode: 429}, nil, Outcome{Retry: true, Status: 429, Failure: FailureHTTPStatus}},
		"408 retried":                    {webhook.Result{StatusCode: 408}, nil, Outcome{Retry: true, Status: 408, Failure: FailureHTTPStatus}},
		"404 is final":                   {webhook.Result{StatusCode: 404}, nil, Outcome{Status: 404, Failure: FailureRejected}},
		"400 is final":                   {webhook.Result{StatusCode: 400}, nil, Outcome{Status: 400, Failure: FailureRejected}},
		"redirect not followed is final": {webhook.Result{StatusCode: 302}, nil, Outcome{Status: 302, Failure: FailureRejected}},
		"timeout retried":                {webhook.Result{Failure: webhook.FailureTimeout}, nil, Outcome{Retry: true, Failure: webhook.FailureTimeout}},
		"connection retried":             {webhook.Result{Failure: webhook.FailureConnection}, nil, Outcome{Retry: true, Failure: webhook.FailureConnection}},
		"dns retried":                    {webhook.Result{Failure: webhook.FailureResolution}, nil, Outcome{Retry: true, Failure: webhook.FailureResolution}},
		"tls retried":                    {webhook.Result{Failure: webhook.FailureTLS}, nil, Outcome{Retry: true, Failure: webhook.FailureTLS}},
		"blocked is final":               {webhook.Result{Failure: webhook.FailureBlocked}, nil, Outcome{Failure: webhook.FailureBlocked}},
		"invalid input":                  {webhook.Result{}, errors.New("invalid webhook endpoint"), Outcome{Failure: FailureInvalid}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Classify(tc.result, tc.err); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestRetryPolicyIsBoundedToAboutADay(t *testing.T) {
	var total time.Duration
	for attempt := 1; attempt < MaxAttempts; attempt++ {
		delay := RetryDelay(attempt)
		if delay <= 0 {
			t.Fatal("no delay after attempt", attempt)
		}
		total += delay
	}
	if total < 20*time.Hour || total > 24*time.Hour || RetryDelay(MaxAttempts) != 0 || RetryDelay(0) != 0 {
		t.Fatal("retry window", total)
	}
}

func TestRequestIsTheRFC8058Post(t *testing.T) {
	r := Request()
	if string(r.Body) != "List-Unsubscribe=One-Click" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("%q %v", r.Body, r.Header)
	}
}

func goodClaim() Claim {
	return Claim{DeliveryID: strings.Repeat("a", 32), SourceKey: "source", Host: "unsub.example.com", Attempt: 1}
}

type fakeObjects struct {
	data []byte
	err  error
}

func (o fakeObjects) Get(context.Context, string, int64) ([]byte, error) { return o.data, o.err }

func httpSource(list string, post bool) fakeObjects {
	data, _ := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{1, submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "t", ListUnsubscribe: list, ListUnsubscribePost: post}})
	return fakeObjects{data: data}
}

func newDispatcher(repo Repository, sender Deliverer, objects Objects) *Dispatcher {
	return &Dispatcher{Repository: repo, Sender: sender, Objects: objects}
}

type fakeRepo struct {
	mu      sync.Mutex
	claim   Claim
	claimed bool
	err     error
	commits []Outcome
	commitE error
}

func (r *fakeRepo) ClaimForward(_ context.Context, token string, _ time.Duration) (Claim, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return Claim{}, r.err
	}
	if r.claimed {
		return Claim{}, ErrNoWork
	}
	r.claimed = true
	c := r.claim
	c.Token = token
	return c, nil
}

func (r *fakeRepo) CommitForward(_ context.Context, _ Claim, o Outcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commits = append(r.commits, o)
	return r.commitE
}

type fakeSender struct {
	url    string
	body   string
	header http.Header
	result webhook.Result
	err    error
	auth   *admin.WebhookAuth
}

func (s *fakeSender) Send(_ context.Context, endpoint string, req format.Request, auth *admin.WebhookAuth) (webhook.Result, error) {
	s.url, s.body, s.header, s.auth = endpoint, string(req.Body), req.Header, auth
	return s.result, s.err
}

func TestDispatchSendsTheOneClickPostAndRecordsTheOutcome(t *testing.T) {
	repo := &fakeRepo{claim: goodClaim()}
	sender := &fakeSender{result: webhook.Result{StatusCode: 200, Success: true}}
	d := newDispatcher(repo, sender, httpSource("<https://unsub.example.com/u?t=secret>", true))
	if err := d.DispatchNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.url != "https://unsub.example.com/u?t=secret" || sender.body != "List-Unsubscribe=One-Click" || sender.header.Get("Content-Type") != "application/x-www-form-urlencoded" || sender.auth != nil {
		t.Fatalf("%+v", sender)
	}
	if len(repo.commits) != 1 || !repo.commits[0].Done {
		t.Fatalf("%+v", repo.commits)
	}
	if err := d.DispatchNext(context.Background()); !errors.Is(err, ErrNoWork) || len(repo.commits) != 1 {
		t.Fatal("a second pass found work", err)
	}
}

func TestDispatchRetryAndTerminalOutcomesAreRecorded(t *testing.T) {
	for name, tc := range map[string]struct {
		result webhook.Result
		want   Outcome
	}{
		"server error": {webhook.Result{StatusCode: 503}, Outcome{Retry: true, Status: 503, Failure: FailureHTTPStatus}},
		"timeout":      {webhook.Result{Failure: webhook.FailureTimeout}, Outcome{Retry: true, Failure: webhook.FailureTimeout}},
		"blocked":      {webhook.Result{Failure: webhook.FailureBlocked}, Outcome{Failure: webhook.FailureBlocked}},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &fakeRepo{claim: goodClaim()}
			d := newDispatcher(repo, &fakeSender{result: tc.result}, httpSource("<https://unsub.example.com/u?t=secret>", true))
			if err := d.DispatchNext(context.Background()); err != nil || len(repo.commits) != 1 || repo.commits[0] != tc.want {
				t.Fatal(err, repo.commits)
			}
		})
	}
}

func TestDispatchRejectsAnInconsistentClaimAndPropagatesStoreErrors(t *testing.T) {
	d := &Dispatcher{Repository: &fakeRepo{claim: Claim{Host: "unsub.example.com", SourceKey: "k"}}, Sender: &fakeSender{}, Objects: fakeObjects{}}
	if err := d.DispatchNext(context.Background()); err == nil {
		t.Fatal("a claim without a delivery was used")
	}
	d = &Dispatcher{Repository: &fakeRepo{err: errors.New("closed")}, Sender: &fakeSender{}, Objects: fakeObjects{}}
	if err := d.DispatchNext(context.Background()); err == nil || errors.Is(err, ErrNoWork) {
		t.Fatal("store error swallowed")
	}
	if err := (&Dispatcher{}).DispatchNext(context.Background()); err == nil {
		t.Fatal("unconfigured dispatcher ran")
	}
}

// Through the real webhook sender: a name that resolves to a private address
// is never dialed, whatever the sender's link says, and the failure is final.
func TestForwardToANonPublicDestinationIsBlocked(t *testing.T) {
	for _, address := range []string{"10.1.2.3", "127.0.0.1", "169.254.169.254", "::1", "2001:db8::1"} {
		t.Run(address, func(t *testing.T) {
			sender := NewSender()
			sender.Resolver = resolverOf(netip.MustParseAddr(address))
			repo := &fakeRepo{claim: goodClaim()}
			d := newDispatcher(repo, sender, httpSource("<https://unsub.example.com/u?t=secret>", true))
			if err := d.DispatchNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(repo.commits) != 1 || repo.commits[0] != (Outcome{Failure: webhook.FailureBlocked}) {
				t.Fatalf("%+v", repo.commits)
			}
		})
	}
}

// The sender never takes an operator's internal-destination exception and
// refuses links the webhook rules refuse, even if one reached it.
func TestSenderRefusesLinksThatAreNotPublicHTTPS(t *testing.T) {
	for _, link := range []string{"http://unsub.example.com/u", "https://unsub.example.com:8443/u", "https://127.0.0.1/u", "https://user@unsub.example.com/u"} {
		result, err := NewSender().Send(context.Background(), link, Request(), nil)
		if err == nil && result.Failure != webhook.FailureBlocked {
			t.Fatalf("%s: %+v", link, result)
		}
		if got := Classify(result, err); got.Done || got.Retry {
			t.Fatalf("%s retried or succeeded: %+v", link, got)
		}
	}
}

type resolverOf netip.Addr

func (r resolverOf) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.Addr(r)}, nil
}

// The link is re-read from the stored source when the forward runs, and used
// only if it still yields the host recorded at preparation.
func TestDispatchReReadsTheSourceAndChecksTheHost(t *testing.T) {
	raw := func(list string) fakeObjects {
		data, err := submission.EncodeRawSource(submission.RawHeader{MailFrom: "a@example.com", Recipients: []string{"b@example.net"}, Kinds: []string{"to"}, SenderDomain: "example.com"},
			[]byte("From: a@example.com\r\nTo: b@example.net\r\nList-Unsubscribe: "+list+"\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n\r\nbody\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		return fakeObjects{data: data}
	}
	for name, tc := range map[string]struct {
		objects fakeObjects
		sent    string
		want    Outcome
	}{
		"http source":           {httpSource("<https://unsub.example.com/u?t=1>", true), "https://unsub.example.com/u?t=1", Outcome{Done: true, Status: 200}},
		"raw source":            {raw("<mailto:u@example.com>, <https://Unsub.Example.com/raw?t=2>"), "https://unsub.example.com/raw?t=2", Outcome{Done: true, Status: 200}},
		"another host":          {httpSource("<https://other.example.com/u>", true), "", Outcome{Failure: FailureTargetChanged}},
		"no longer forwardable": {httpSource("<https://unsub.example.com/u>", false), "", Outcome{Failure: FailureTargetChanged}},
		"no headers any more":   {httpSource("", false), "", Outcome{Failure: FailureTargetChanged}},
		"undecodable":           {fakeObjects{data: []byte("junk")}, "", Outcome{Failure: FailureTargetChanged}},
		"unreadable is retried": {fakeObjects{err: errors.New("store down")}, "", Outcome{Retry: true, Failure: FailureSourceUnavailable}},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &fakeRepo{claim: goodClaim()}
			sender := &fakeSender{result: webhook.Result{StatusCode: 200, Success: true}}
			if err := newDispatcher(repo, sender, tc.objects).DispatchNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if sender.url != tc.sent || len(repo.commits) != 1 || repo.commits[0] != tc.want {
				t.Fatalf("sent %q, %+v", sender.url, repo.commits)
			}
		})
	}
}

// An Enhanced Protection source decodes with the claim's flag, and the forward
// goes out like any other.
func TestDispatchReadsAnEnhancedProtectionSource(t *testing.T) {
	data, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{1, submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "t", EnhancedProtection: true, ListUnsubscribe: "<https://unsub.example.com/u?t=1>", ListUnsubscribePost: true}})
	if err != nil {
		t.Fatal(err)
	}
	claim := goodClaim()
	claim.EnhancedProtection = true
	repo := &fakeRepo{claim: claim}
	sender := &fakeSender{result: webhook.Result{StatusCode: 200, Success: true}}
	if err = newDispatcher(repo, sender, fakeObjects{data: data}).DispatchNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.url != "https://unsub.example.com/u?t=1" || len(repo.commits) != 1 || !repo.commits[0].Done {
		t.Fatalf("%q %+v", sender.url, repo.commits)
	}
	// A claim whose flag disagrees with the source does not decode: target_changed.
	repo = &fakeRepo{claim: goodClaim()}
	if err = newDispatcher(repo, &fakeSender{}, fakeObjects{data: data}).DispatchNext(context.Background()); err != nil || repo.commits[0] != (Outcome{Failure: FailureTargetChanged}) {
		t.Fatal(err, repo.commits)
	}
}
