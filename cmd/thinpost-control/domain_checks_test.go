package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/postgres"
)

func fakeKind(name string, n int, run func(context.Context) error, claimErr error) (domainKind, *atomic.Int32) {
	var claims atomic.Int32
	return domainKind{name: name, claim: func(_ context.Context, limit int) ([]domainCheck, error) {
		claims.Add(1)
		if claimErr != nil {
			return nil, claimErr
		}
		if n > limit {
			n = limit
		}
		checks := make([]domainCheck, n)
		for i := range checks {
			checks[i] = run
		}
		return checks, nil
	}}, &claims
}

func TestDomainCheckerBoundsBatchAndConcurrency(t *testing.T) {
	var running, peak, done atomic.Int32
	kind, _ := fakeKind("bounce", 50, func(context.Context) error {
		now := running.Add(1)
		for {
			p := peak.Load()
			if now <= p || peak.CompareAndSwap(p, now) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)
		done.Add(1)
		return nil
	}, nil)
	c := newDomainChecker(kind)
	c.tick(context.Background())
	if done.Load() != domainCheckBatch {
		t.Fatalf("ran %d checks, want one batch of %d", done.Load(), domainCheckBatch)
	}
	if peak.Load() > domainCheckConcurrency || peak.Load() < 2 {
		t.Fatalf("concurrency peaked at %d, limit %d", peak.Load(), domainCheckConcurrency)
	}
}

// One kind failing to claim, and one check failing to record, never stop the
// others; a closed gate is quiet.
func TestDomainCheckerIsolatesFailures(t *testing.T) {
	var ran atomic.Int32
	broken, _ := fakeKind("broken", 0, nil, errors.New("database"))
	gated, gatedClaims := fakeKind("gated", 0, nil, postgres.ErrRuntimeClosed)
	failing, _ := fakeKind("failing", 3, func(context.Context) error { ran.Add(1); return errors.New("record") }, nil)
	healthy, _ := fakeKind("healthy", 2, func(context.Context) error { ran.Add(1); return nil }, nil)
	c := newDomainChecker(broken, gated, failing, healthy)
	c.tick(context.Background())
	if ran.Load() != 5 || gatedClaims.Load() != 1 {
		t.Fatal("ran", ran.Load(), gatedClaims.Load())
	}
}

func TestDomainCheckerTimesOutAChildAndStopsOnCancel(t *testing.T) {
	var sawDeadline atomic.Bool
	kind, _ := fakeKind("bounce", 1, func(ctx context.Context) error {
		_, ok := ctx.Deadline()
		sawDeadline.Store(ok)
		return nil
	}, nil)
	c := newDomainChecker(kind)
	c.tick(context.Background())
	if !sawDeadline.Load() {
		t.Fatal("a check has no deadline")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	kind2, claims := fakeKind("bounce", 1, func(context.Context) error { t.Error("ran after cancel"); return nil }, nil)
	newDomainChecker(kind2).tick(ctx)
	if claims.Load() != 0 {
		t.Fatal("claimed after cancel")
	}
}

func TestStartDomainChecksRunsAndStops(t *testing.T) {
	var mu sync.Mutex
	ticks := 0
	kind := domainKind{name: "bounce", claim: func(context.Context, int) ([]domainCheck, error) {
		mu.Lock()
		ticks++
		mu.Unlock()
		return nil, nil
	}}
	c := newDomainChecker(kind)
	c.every = 5 * time.Millisecond
	stop := startDomainChecks(context.Background(), c)
	time.Sleep(60 * time.Millisecond)
	stop()
	stop()
	mu.Lock()
	n := ticks
	mu.Unlock()
	if n < 2 {
		t.Fatal("ticks", n)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if ticks != n {
		t.Fatal("ticked after stop")
	}
}
