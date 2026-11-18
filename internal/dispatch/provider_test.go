package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/submission"
)

type repo struct {
	claim     delivery.Claim
	results   []delivery.Result
	commitErr error
}

func (r *repo) ClaimDelivery(_ context.Context, lease postgres.ControllerLease, id string, _ delivery.EdgeCapabilities) (delivery.Claim, error) {
	r.claim.AttemptID = id
	r.claim.WorkerID = lease.WorkerID
	r.claim.Epoch = lease.Epoch
	return r.claim, nil
}
func (r *repo) CommitDeliveryResultIdentified(_ context.Context, result delivery.Result) (delivery.Committed, error) {
	if r.commitErr != nil {
		return delivery.Committed{}, r.commitErr
	}
	r.results = append(r.results, result)
	return delivery.Committed{AppID: r.claim.AppID, MessageID: r.claim.MessageID}, nil
}

type objects struct {
	source, mime []byte
	fail         bool
}

func (o *objects) Get(_ context.Context, key string, _ int64) ([]byte, error) {
	if o.fail {
		return nil, errors.New("unavailable")
	}
	if key == "source" {
		return o.source, nil
	}
	return o.mime, nil
}

type routes struct {
	seen *int
	err  error
}

func (r routes) ForRecipient(_ context.Context, _, _ string, attempt int) (Route, error) {
	if r.err != nil {
		return Route{}, r.err
	}
	if r.seen != nil {
		*r.seen = attempt
	}
	return Route{MXName: "mx.example", Address: netip.MustParseAddr("8.8.8.8")}, nil
}

func TestLocalDNSFailuresAreCommittedWithoutInventingSMTPResponses(t *testing.T) {
	for _, tc := range []struct {
		err     error
		outcome string
	}{
		{delivery.ErrNullMX, delivery.OutcomeNullMX},
		{delivery.ErrDomainNotFound, delivery.OutcomeDomainNotFound},
		{errors.New("DNS unavailable"), "rejected"},
	} {
		p, r, _, lease := setup(t)
		p.Routes = routes{err: tc.err}
		start, _, err := p.Next(context.Background(), lease, delivery.EdgeCapabilities{})
		if !errors.Is(err, delivery.ErrNoWork) || start != nil || len(r.results) != 1 || r.results[0].Outcome != tc.outcome || r.results[0].SMTPCode != 0 || r.results[0].JournalID != "" {
			t.Fatalf("DNS failure %v not committed as %s: %+v, %v", tc.err, tc.outcome, r.results, err)
		}
	}
}

type envelopes struct{}
type allowRecipients bool

func (a allowRecipients) Allows(string) bool { return bool(a) }

func (envelopes) ForDelivery(context.Context, delivery.Claim) (string, error) {
	return "b+opaque@bounce.example", nil
}
func setup(t *testing.T) (*Provider, *repo, *objects, postgres.ControllerLease) {
	t.Helper()
	msg := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, ReplyTo: []string{"help@example.com"}, Subject: "hello", Text: "body", EnhancedProtection: true}
	source, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{1, msg})
	if err != nil {
		t.Fatal(err)
	}
	r := &repo{claim: delivery.Claim{DeliveryID: "delivery", SourceKey: "source", FinalKey: "final", EnhancedProtection: true}}
	o := &objects{source: source, mime: []byte("signed MIME")}
	return &Provider{Repository: r, Objects: o, Routes: routes{}, Envelopes: envelopes{}, Recipients: allowRecipients(true)}, r, o, postgres.ControllerLease{WorkerID: "worker", Epoch: 4}
}

func TestRecipientPolicyHoldsBeforeDNSOrEnvelope(t *testing.T) {
	p, r, _, lease := setup(t)
	p.Recipients = allowRecipients(false)
	used := -1
	p.Routes = routes{seen: &used}
	start, _, err := p.Next(context.Background(), lease, delivery.EdgeCapabilities{})
	if !errors.Is(err, delivery.ErrNoWork) || start != nil || used != -1 || len(r.results) != 1 || r.results[0].Outcome != delivery.OutcomeRecipientPolicyDenied || r.results[0].SMTPCode != 0 {
		t.Fatal("denied recipient was not held before DNS", err, used, r.results)
	}
	r.commitErr = errors.New("db unavailable")
	if _, _, err = p.Next(context.Background(), lease, delivery.EdgeCapabilities{}); err == nil || errors.Is(err, delivery.ErrNoWork) {
		t.Fatal("failed policy hold commit was reported as safe")
	}
}
func TestDispatchUsesCommittedAttemptAndStoredMIME(t *testing.T) {
	p, r, _, lease := setup(t)
	start, mime, err := p.Next(context.Background(), lease, delivery.EdgeCapabilities{})
	if err != nil || start.Recipient != "b@example.net" || start.MxName != "mx.example" || !start.EnhancedProtection || string(mime) != "signed MIME" || len(r.results) != 0 {
		t.Fatalf("incorrect work: %+v %v", start, err)
	}
}

func TestDispatchCarriesTrackingCutoff(t *testing.T) {
	p, r, _, lease := setup(t)
	r.claim.TrackingSendCutoff = time.Now().Add(time.Hour)
	start, _, err := p.Next(context.Background(), lease, delivery.EdgeCapabilities{})
	if err != nil || start.TrackingSendCutoffUnixMs != r.claim.TrackingSendCutoff.UnixMilli() {
		t.Fatal("tracking cutoff was not included in the delivery descriptor", err)
	}
}
func TestPreparationFailureCommitsBeforeRetry(t *testing.T) {
	p, r, o, lease := setup(t)
	o.fail = true
	if _, _, err := p.Next(context.Background(), lease, delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrNoWork) || len(r.results) != 1 || r.results[0].Outcome != "rejected" {
		t.Fatal("failure not committed")
	}
	r.commitErr = errors.New("db down")
	if _, _, err := p.Next(context.Background(), lease, delivery.EdgeCapabilities{}); err == nil || errors.Is(err, delivery.ErrNoWork) {
		t.Fatal("failed result commit reported safe retry")
	}
}

func TestDispatchUsesPersistedAttemptNumber(t *testing.T) {
	p, r, _, lease := setup(t)
	r.claim.RouteAttempt = 3
	var used int
	p.Routes = routes{seen: &used}
	if _, _, err := p.Next(context.Background(), lease, delivery.EdgeCapabilities{}); err != nil || used != 3 {
		t.Fatal("persisted attempt did not reach route selection", used, err)
	}
}

func TestLocalOutcomeLogsIDsAfterCommit(t *testing.T) {
	var lines []string
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }))
	p, r, _, lease := setup(t)
	r.claim.AppID, r.claim.MessageID, r.claim.DeliveryID = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210", "00112233445566778899aabbccddeeff"
	p.Routes = routes{err: delivery.ErrNullMX}
	if _, _, err := p.Next(context.Background(), lease, delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrNoWork) {
		t.Fatal(err)
	}
	want := "delivery app=0123456789abcdef0123456789abcdef message=fedcba9876543210fedcba9876543210 delivery=00112233445566778899aabbccddeeff status=null_mx smtp=0 ip=primary"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("unexpected log lines %q", lines)
	}
	lines = nil
	r.commitErr = errors.New("db down")
	if _, _, err := p.Next(context.Background(), lease, delivery.EdgeCapabilities{}); err == nil || len(lines) != 0 {
		t.Fatalf("logged without a committed result: %q %v", lines, err)
	}
}
