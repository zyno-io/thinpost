package controlsession

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/postgres"
)

type flakyReports struct {
	failures int
	err      error
	calls    int
}

func (f *flakyReports) RecordEdgeSources(context.Context, postgres.ControllerLease, delivery.EdgeCapabilities) error {
	f.calls++
	if f.calls <= f.failures {
		return f.err
	}
	return nil
}

func TestRecordReportRetriesBoundedly(t *testing.T) {
	old := reportRetryDelays
	reportRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { reportRetryDelays = old }()
	ctx := context.Background()
	boom := errors.New("boom")
	// Succeeds on the third (last) try.
	if r := (&flakyReports{failures: 2, err: boom}); recordReport(ctx, r, postgres.ControllerLease{}, delivery.EdgeCapabilities{}) != nil || r.calls != 3 {
		t.Fatal("did not retry to success", r.calls)
	}
	// Gives up after three tries.
	if r := (&flakyReports{failures: 99, err: boom}); !errors.Is(recordReport(ctx, r, postgres.ControllerLease{}, delivery.EdgeCapabilities{}), boom) || r.calls != 3 {
		t.Fatal("retry is not bounded", r.calls)
	}
	// A lease that is no longer valid is not retried.
	if r := (&flakyReports{failures: 99, err: delivery.ErrLeaseInvalid}); !errors.Is(recordReport(ctx, r, postgres.ControllerLease{}, delivery.EdgeCapabilities{}), delivery.ErrLeaseInvalid) || r.calls != 1 {
		t.Fatal("retried an invalid lease", r.calls)
	}
	// A cancelled context stops the retry.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if r := (&flakyReports{failures: 99, err: boom}); recordReport(cancelled, r, postgres.ControllerLease{}, delivery.EdgeCapabilities{}) == nil || r.calls != 1 {
		t.Fatal("retried after cancel", r.calls)
	}
}
