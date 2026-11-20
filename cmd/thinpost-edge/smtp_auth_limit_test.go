package main

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/bounce"
)

func TestAuthLimiterBlocksAfterBurstAndRefills(t *testing.T) {
	l, err := newSMTPAuthLimiter(time.Minute, 10, 4096)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	l.now = func() time.Time { return now }
	a := netip.MustParseAddr("192.0.2.1")
	for i := 0; i < 10; i++ {
		if l.Check(a) != bounce.AuthGateOpen {
			t.Fatalf("blocked after %d attempts", i)
		}
	}
	if l.Check(a) != bounce.AuthGateBlocked {
		t.Fatal("not blocked after 10 attempts")
	}
	if l.Check(netip.MustParseAddr("192.0.2.2")) != bounce.AuthGateOpen {
		t.Fatal("another address must be unaffected")
	}
	now = now.Add(61 * time.Second)
	if l.Check(a) != bounce.AuthGateOpen {
		t.Fatal("one token should have refilled after a minute")
	}
}

// Parallel attempts are charged when checked, so they cannot all slip through.
func TestAuthLimiterChargesParallelAttempts(t *testing.T) {
	l, _ := newSMTPAuthLimiter(time.Hour, 10, 16)
	a := netip.MustParseAddr("192.0.2.1")
	var open atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Check(a) == bounce.AuthGateOpen {
				open.Add(1)
			}
		}()
	}
	wg.Wait()
	if open.Load() != 10 {
		t.Fatalf("%d parallel attempts passed, want 10", open.Load())
	}
}

func TestAuthLimiterRefundsSuccess(t *testing.T) {
	l, _ := newSMTPAuthLimiter(time.Hour, 2, 16)
	a := netip.MustParseAddr("192.0.2.1")
	for i := 0; i < 20; i++ {
		if l.Check(a) != bounce.AuthGateOpen {
			t.Fatalf("a successful login must not use up the budget (attempt %d)", i)
		}
		l.Succeeded(a)
	}
	l.Check(a)
	l.Check(a)
	if l.Check(a) != bounce.AuthGateBlocked {
		t.Fatal("failures must still accumulate")
	}
}

func TestAuthLimiterKeysIPv6ByPrefixAndMapsV4(t *testing.T) {
	l, _ := newSMTPAuthLimiter(time.Minute, 2, 16)
	now := time.Now()
	l.now = func() time.Time { return now }
	l.Check(netip.MustParseAddr("2001:db8::1"))
	l.Check(netip.MustParseAddr("2001:db8::ffff"))
	if l.Check(netip.MustParseAddr("2001:db8::2")) != bounce.AuthGateBlocked {
		t.Fatal("addresses of one /64 must share a bucket")
	}
	l.Check(netip.MustParseAddr("::ffff:192.0.2.9"))
	l.Check(netip.MustParseAddr("192.0.2.9"))
	if l.Check(netip.MustParseAddr("192.0.2.9")) != bounce.AuthGateBlocked {
		t.Fatal("a mapped address must share the IPv4 bucket")
	}
	if l.Check(netip.Addr{}) != bounce.AuthGateBusy {
		t.Fatal("an invalid address must not pass")
	}
}

func TestAuthLimiterFullTableFailsClosedForUnknownSources(t *testing.T) {
	l, _ := newSMTPAuthLimiter(time.Minute, 2, 2)
	now := time.Now()
	l.now = func() time.Time { return now }
	a, b, c := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("192.0.2.3")
	l.Check(a)
	l.Check(b)
	if l.Check(c) != bounce.AuthGateBusy {
		t.Fatal("an unknown source must wait while the table is full")
	}
	if len(l.sources) != 2 {
		t.Fatal("the table grew past its bound")
	}
	if l.Check(a) != bounce.AuthGateOpen {
		t.Fatal("a known source with tokens left may try")
	}
	now = now.Add(time.Hour)
	if l.Check(c) != bounce.AuthGateOpen {
		t.Fatal("idle sources must be evicted")
	}
}

func TestAuthLimiterRejectsBadConfiguration(t *testing.T) {
	for _, tc := range []struct {
		interval time.Duration
		burst, n int
	}{{0, 10, 1}, {time.Minute, 0, 1}, {time.Minute, 10, 0}, {time.Minute, 10, 70000}, {2 * time.Hour, 10, 1}} {
		if _, err := newSMTPAuthLimiter(tc.interval, tc.burst, tc.n); err == nil {
			t.Fatalf("%+v accepted", tc)
		}
	}
}
