package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/journal"
)

func waitJournalHealth(t *testing.T, h *journalHealth, want bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if h.ready() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("journal health did not become %v", want)
}

func TestJournalHealthTracksFaultButAllowsFullJournal(t *testing.T) {
	j, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	fence := journal.Fence{ControllerID: "controller", Epoch: 1}
	if err = j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("attempt", "delivery", fence, time.Now()); err != nil {
		t.Fatal(err)
	}
	if remaining, err := j.Remaining(); err != nil || remaining != 0 {
		t.Fatal("journal did not reach record capacity", remaining, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := startJournalHealth(ctx, func() error { _, err := j.Identity(); return err }, 5*time.Millisecond, time.Second)
	waitJournalHealth(t, h, true)
	// Reconciliation must still be able to connect when the journal has no
	// free record slots. This invariant failure, however, latches it closed.
	if err = j.Complete("missing", journal.Accepted, 250, time.Now()); !errors.Is(err, journal.ErrNotFound) {
		t.Fatal("expected missing attempt", err)
	}
	waitJournalHealth(t, h, false)
}

func TestJournalHealthExpiresAStuckCheckWithoutAmplifyingProbes(t *testing.T) {
	blocked := make(chan struct{})
	entered := make(chan struct{})
	defer close(blocked)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	h := startJournalHealth(ctx, func() error {
		if calls.Add(1) == 2 {
			close(entered)
			<-blocked
		}
		return nil
	}, 5*time.Millisecond, 400*time.Millisecond)
	waitJournalHealth(t, h, true)
	<-entered
	handler := healthHandler(h.ready)
	for range 100 {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	}
	if calls.Load() != 2 {
		t.Fatal("health requests triggered journal operations")
	}
	waitJournalHealth(t, h, false)
}

func TestLoopbackProbeReportsReadinessAndIgnoresProxy(t *testing.T) {
	host, _, err := net.SplitHostPort(healthAddress)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("health address is not loopback", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	server := &http.Server{Handler: healthHandler(ready.Load), ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	if err = probeHealth(listener.Addr().String()); err == nil {
		t.Fatal("unready listener passed")
	}
	ready.Store(true)
	if err = probeHealth(listener.Addr().String()); err != nil {
		t.Fatal("ready loopback listener failed", err)
	}
	w := httptest.NewRecorder()
	healthHandler(ready.Load).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/readyz", nil))
	if w.Code != http.StatusNotFound {
		t.Fatal("unexpected health route", w.Code)
	}
}
