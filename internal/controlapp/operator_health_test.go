package controlapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func waitHealth(t *testing.T, health *OperatorHealth, want bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if health.Ready() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("operator readiness did not become %v", want)
}

func TestOperatorHealthTracksDatabaseAndRecovers(t *testing.T) {
	results := make(chan chan error, 1)
	ping := func(ctx context.Context) error {
		result := make(chan error, 1)
		select {
		case results <- result:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case err := <-result:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	health := startOperatorHealth(context.Background(), ping, 10*time.Millisecond, 200*time.Millisecond, time.Second)
	t.Cleanup(func() { _ = health.Close() })
	if health.Ready() {
		t.Fatal("ready before the first database check")
	}
	first := <-results
	first <- nil
	waitHealth(t, health, true)
	second := <-results
	second <- errors.New("database unavailable")
	waitHealth(t, health, false)
	third := <-results
	third <- nil
	waitHealth(t, health, true)
	if err := health.Close(); err != nil || health.Ready() {
		t.Fatal("closed health monitor remained ready", err)
	}
}

func TestOperatorHealthExpiresStuckChecksAndDoesNotAmplifyRequests(t *testing.T) {
	started := make(chan struct{}, 1)
	block := make(chan struct{})
	defer close(block)
	var calls atomic.Int32
	ping := func(context.Context) error {
		if calls.Add(1) != 2 {
			return nil
		}
		started <- struct{}{}
		<-block // Simulate a broken dependency that ignores cancellation.
		return nil
	}
	health := startOperatorHealth(context.Background(), ping, 5*time.Millisecond, 20*time.Millisecond, 300*time.Millisecond)
	t.Cleanup(func() { _ = health.Close() })
	waitHealth(t, health, true)
	<-started
	router := Router(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil, nil, health.Ready)
	for range 100 {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/operator-readyz", nil))
	}
	if calls.Load() != 2 {
		t.Fatal("HTTP readiness requests triggered database checks")
	}
	waitHealth(t, health, false)
}
