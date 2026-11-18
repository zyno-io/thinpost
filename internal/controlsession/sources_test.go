package controlsession

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/postgres"
)

// capsWork records the capabilities each Next call receives.
type capsWork struct {
	mu   sync.Mutex
	caps []delivery.EdgeCapabilities
	seen chan struct{}
}

func (w *capsWork) Next(_ context.Context, _ postgres.ControllerLease, caps delivery.EdgeCapabilities) (*controlv1.DeliveryStart, []byte, error) {
	w.mu.Lock()
	w.caps = append(w.caps, caps)
	w.mu.Unlock()
	select {
	case w.seen <- struct{}{}:
	default:
	}
	return nil, nil, delivery.ErrNoWork
}

type recordingReports struct {
	mu   sync.Mutex
	caps []delivery.EdgeCapabilities
	err  error
	done chan struct{}
}

func (r *recordingReports) RecordEdgeSources(_ context.Context, _ postgres.ControllerLease, caps delivery.EdgeCapabilities) error {
	r.mu.Lock()
	r.caps = append(r.caps, caps)
	r.mu.Unlock()
	close(r.done)
	return r.err
}

var legacyCaps = delivery.EdgeCapabilities{Addresses: []netip.Addr{netip.MustParseAddr("45.0.1.22")}, BounceDomains: []string{"bounce.example.net"}}

// pullOnce drives a session through Ready, replay and one Pull, and returns the
// capabilities the work source saw.
func pullOnce(t *testing.T, ready func(*controlv1.Ready), reports *recordingReports) delivery.EdgeCapabilities {
	t.Helper()
	work := &capsWork{seen: make(chan struct{}, 1)}
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Work, runner.Legacy = work, legacyCaps
	if reports != nil {
		runner.Reports = reports
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	open := (<-out).GetOpen()
	frame := readyFrame(open)
	ready(frame.GetReady())
	in <- frame
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, open)
	<-out
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Pull{Pull: &controlv1.Pull{AvailableSlots: 1}}}
	select {
	case <-work.seen:
	case err := <-done:
		t.Fatalf("session ended before work was requested: %v", err)
	case <-ctx.Done():
		t.Fatal("work was never requested")
	}
	work.mu.Lock()
	defer work.mu.Unlock()
	cancel()
	<-done
	return work.caps[0]
}

func TestReadyWithoutSourcesIsLegacy(t *testing.T) {
	caps := pullOnce(t, func(*controlv1.Ready) {}, nil)
	if caps.Explicit || len(caps.Addresses) != 1 || caps.Addresses[0] != legacyCaps.Addresses[0] || len(caps.BounceDomains) != 1 || caps.BounceDomains[0] != "bounce.example.net" {
		t.Fatalf("legacy capabilities %+v", caps)
	}
}

func TestReadyWithSourcesIsExplicit(t *testing.T) {
	reports := &recordingReports{done: make(chan struct{})}
	caps := pullOnce(t, func(r *controlv1.Ready) {
		r.SourceAddresses = []string{"45.0.1.22", "45.0.0.10"}
		r.BounceDomains = []string{"bounce.example.net", "bounce.hosting.example.org"}
	}, reports)
	if !caps.Explicit || len(caps.Addresses) != 2 || caps.Addresses[0].String() != "45.0.1.22" || caps.Addresses[1].String() != "45.0.0.10" || len(caps.BounceDomains) != 2 {
		t.Fatalf("explicit capabilities %+v", caps)
	}
	if primary, ok := caps.Primary(); !ok || primary.String() != "45.0.1.22" || !caps.Has("45.0.0.10") || caps.Has("45.0.0.11") {
		t.Fatal("capability accessors")
	}
	select {
	case <-reports.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the report was not recorded")
	}
	if len(reports.caps) != 1 || !reports.caps[0].Explicit {
		t.Fatal("recorded the wrong report")
	}
}

// Recording is display-only: its failure must not end or stall the session.
func TestFailedReportWriteDoesNotEndTheSession(t *testing.T) {
	reports := &recordingReports{done: make(chan struct{}), err: errors.New("database unavailable")}
	caps := pullOnce(t, func(r *controlv1.Ready) {
		r.SourceAddresses = []string{"45.0.1.22"}
		r.BounceDomains = []string{"bounce.example.net"}
	}, reports)
	if !caps.Explicit {
		t.Fatal("work was not requested after a failed report write")
	}
}

func TestInvalidReadyEndsTheSession(t *testing.T) {
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 40) + ".example"
	var many []string
	for i := 0; i < 17; i++ {
		many = append(many, netip.AddrFrom4([4]byte{45, 0, 0, byte(i + 1)}).String())
	}
	for name, mutate := range map[string]func(*controlv1.Ready){
		"duplicate source":     func(r *controlv1.Ready) { r.SourceAddresses = []string{"45.0.0.10", "45.0.0.10"} },
		"private source":       func(r *controlv1.Ready) { r.SourceAddresses = []string{"10.1.2.3"} },
		"documentation source": func(r *controlv1.Ready) { r.SourceAddresses = []string{"203.0.113.9"} },
		"IPv6 source":          func(r *controlv1.Ready) { r.SourceAddresses = []string{"2607:f8b0:4000::1"} },
		"mapped source":        func(r *controlv1.Ready) { r.SourceAddresses = []string{"::ffff:45.0.0.10"} },
		"padded source":        func(r *controlv1.Ready) { r.SourceAddresses = []string{" 45.0.0.10"} },
		"not an address":       func(r *controlv1.Ready) { r.SourceAddresses = []string{"mail.example.com"} },
		"empty source":         func(r *controlv1.Ready) { r.SourceAddresses = []string{""} },
		"too many sources":     func(r *controlv1.Ready) { r.SourceAddresses = many },
		"uppercase domain": func(r *controlv1.Ready) {
			r.SourceAddresses, r.BounceDomains = []string{"45.0.0.10"}, []string{"Bounce.example"}
		},
		"duplicate domain": func(r *controlv1.Ready) {
			r.SourceAddresses, r.BounceDomains = []string{"45.0.0.10"}, []string{"b.example", "b.example"}
		},
		"overlong domain": func(r *controlv1.Ready) { r.SourceAddresses, r.BounceDomains = []string{"45.0.0.10"}, []string{long} },
		"too many domains": func(r *controlv1.Ready) {
			r.SourceAddresses, r.BounceDomains = []string{"45.0.0.10"}, strings.Split(strings.Repeat("a.example,", 9)[:89], ",")
		},
		"domains without sources": func(r *controlv1.Ready) { r.BounceDomains = []string{"bounce.example.net"} },
		"domain that is an address": func(r *controlv1.Ready) {
			r.SourceAddresses, r.BounceDomains = []string{"45.0.0.10"}, []string{"45.0.0.10"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			work := &capsWork{seen: make(chan struct{}, 1)}
			runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
			runner.Work, runner.Legacy = work, legacyCaps
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()
			open := (<-out).GetOpen()
			frame := readyFrame(open)
			mutate(frame.GetReady())
			in <- frame
			if err := <-done; err == nil || !strings.Contains(err.Error(), "invalid edge readiness") {
				t.Fatalf("session continued after an invalid readiness: %v", err)
			}
			if len(work.caps) != 0 {
				t.Fatal("work was requested after an invalid readiness")
			}
		})
	}
}
