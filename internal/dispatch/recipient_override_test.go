package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/dispatch"
	"github.com/zyno-io/thinpost/internal/egress"
	"github.com/zyno-io/thinpost/internal/mailroute"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/submission"
)

type overrideRepo struct {
	claim   delivery.Claim
	results []delivery.Result
}

func (r *overrideRepo) ClaimDelivery(_ context.Context, lease postgres.ControllerLease, id string, _ delivery.EdgeCapabilities) (delivery.Claim, error) {
	claim := r.claim
	claim.AttemptID, claim.WorkerID, claim.Epoch = id, lease.WorkerID, lease.Epoch
	return claim, nil
}

func (r *overrideRepo) CommitDeliveryResultIdentified(_ context.Context, result delivery.Result) (delivery.Committed, error) {
	r.results = append(r.results, result)
	return delivery.Committed{}, nil
}

type overrideObjects struct{ source []byte }

func (o overrideObjects) Get(_ context.Context, key string, _ int64) ([]byte, error) {
	if key == "source" {
		return o.source, nil
	}
	return []byte("signed MIME"), nil
}

type overrideEnvelopes struct{}

func (overrideEnvelopes) ForDelivery(context.Context, delivery.Claim) (string, error) {
	return "b+opaque@bounce.example", nil
}

type denyAll struct{}

func (denyAll) Allows(string) bool { return false }

type overrideDNS struct{ address netip.Addr }

func (d overrideDNS) LookupMX(context.Context, string) ([]*net.MX, error) {
	return []*net.MX{{Host: "mx.example.net."}}, nil
}

func (d overrideDNS) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{d.address}, nil
}

func overrideProvider(t *testing.T, override bool, destination string) (*dispatch.Provider, *overrideRepo) {
	t.Helper()
	msg := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "hello", Text: "body"}
	source, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{1, msg})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := egress.NewPolicy([]netip.Prefix{netip.MustParsePrefix("45.0.1.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	r := &overrideRepo{claim: delivery.Claim{DeliveryID: "delivery", SourceKey: "source", FinalKey: "final", PreparedFormat: 1, SourceAddress: "45.0.1.22", HeloName: "mx.example", BounceDomain: "mx.example", RecipientPolicyOverride: override}}
	routes := &mailroute.Resolver{DNS: overrideDNS{address: netip.MustParseAddr(destination)}, Filter: policy}
	return &dispatch.Provider{Repository: r, Objects: overrideObjects{source: source}, Routes: routes, Envelopes: overrideEnvelopes{}, Recipients: denyAll{}}, r
}

// A claim from a console test send with the allowlist skipped is not denied by
// control's allowlist recheck, and is dispatched to a public destination.
func TestClaimOverrideSkipsOnlyTheAllowlist(t *testing.T) {
	lease := postgres.ControllerLease{WorkerID: "worker", Epoch: 1}
	p, r := overrideProvider(t, false, "8.8.8.8")
	if _, _, err := p.Next(context.Background(), lease, overrideCaps()); !errors.Is(err, delivery.ErrNoWork) || len(r.results) != 1 || r.results[0].Outcome != delivery.OutcomeRecipientPolicyDenied {
		t.Fatal("allowlist denial without an override was not held", err, r.results)
	}
	p, r = overrideProvider(t, true, "8.8.8.8")
	start, _, err := p.Next(context.Background(), lease, overrideCaps())
	if err != nil || start == nil || start.Recipient != "b@example.net" || len(r.results) != 0 {
		t.Fatal("override did not skip the allowlist", err, r.results)
	}
}

// The override never relaxes the egress policy: control's resolver filter still
// refuses special-use and infrastructure destinations, so nothing is dispatched
// to the Edge. The Edge no longer re-checks the destination, which makes this
// the only enforcement point.
func TestClaimOverrideStillAppliesEgressPolicy(t *testing.T) {
	lease := postgres.ControllerLease{WorkerID: "worker", Epoch: 1}
	for _, destination := range []string{"10.1.2.3", "127.0.0.1", "169.254.1.1", "45.0.1.40"} {
		p, r := overrideProvider(t, true, destination)
		start, _, err := p.Next(context.Background(), lease, overrideCaps())
		if !errors.Is(err, delivery.ErrNoWork) || start != nil || len(r.results) != 1 || r.results[0].Outcome != "rejected" {
			t.Fatalf("%s: egress-denied destination was dispatched: %v %+v", destination, err, r.results)
		}
	}
}

// overrideCaps is a worker that reported its one source and that source's name.
func overrideCaps() delivery.EdgeCapabilities {
	return delivery.EdgeCapabilities{Sources: []delivery.Source{{Address: netip.MustParseAddr("45.0.1.22"), Hostname: "mx.example"}}}
}
