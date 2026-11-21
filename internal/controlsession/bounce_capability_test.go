package controlsession

import (
	"context"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/postgres"
)

type anyDomainReports struct{ got chan bool }

func (r *anyDomainReports) RecordEdgeSources(_ context.Context, _ postgres.ControllerLease, c delivery.EdgeCapabilities) error {
	r.got <- c.AnyBounceDomain
	return nil
}

// AnyBounceDomain is the edge's own statement in Ready.capabilities. Control
// never offers it, and an edge that does not list it gets the HELO name.
func TestReadyBounceAnyDomainCapability(t *testing.T) {
	for name, tc := range map[string]struct {
		capabilities []string
		want         bool
	}{
		"listed":      {[]string{edgeprotocol.CapAnyBounceDomain}, true},
		"with others": {[]string{edgeprotocol.CapInbound, edgeprotocol.CapAnyBounceDomain}, true},
		"not listed":  {nil, false},
		"other only":  {[]string{edgeprotocol.CapInbound}, false},
	} {
		t.Run(name, func(t *testing.T) {
			reports := &anyDomainReports{got: make(chan bool, 1)}
			runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
			runner.Reports = reports
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()
			open := (<-out).GetOpen()
			for _, offered := range open.GetCapabilities() {
				if offered == edgeprotocol.CapAnyBounceDomain {
					t.Fatal("control offered an edge-only capability")
				}
			}
			ready := readyFrame(open)
			ready.GetReady().Capabilities = tc.capabilities
			in <- ready
			in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
			assertReconciled(t, <-out, open)
			select {
			case got := <-reports.got:
				if got != tc.want {
					t.Fatalf("AnyBounceDomain %v, want %v", got, tc.want)
				}
			case err := <-done:
				t.Fatalf("session ended: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("no report")
			}
		})
	}
}
