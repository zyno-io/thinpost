package main

import (
	"testing"
	"time"

	"net/netip"

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
			t.Fatalf("blocked after %d failures", i)
		}
		l.Failed(a)
	}
	if l.Check(a) != bounce.AuthGateBlocked {
		t.Fatal("not blocked after 10 failures")
	}
	if l.Check(netip.MustParseAddr("192.0.2.2")) != bounce.AuthGateOpen {
		t.Fatal("another address must be unaffected")
	}
	now = now.Add(61 * time.Second)
	if l.Check(a) != bounce.AuthGateOpen {
		t.Fatal("one token should have refilled after a minute")
	}
}

func TestAuthLimiterKeysIPv6ByPrefixAndMapsV4(t *testing.T) {
	l, _ := newSMTPAuthLimiter(time.Minute, 2, 16)
	now := time.Now()
	l.now = func() time.Time { return now }
	l.Failed(netip.MustParseAddr("2001:db8::1"))
	l.Failed(netip.MustParseAddr("2001:db8::ffff"))
	if l.Check(netip.MustParseAddr("2001:db8::2")) != bounce.AuthGateBlocked {
		t.Fatal("addresses of one /64 must share a bucket")
	}
	l.Failed(netip.MustParseAddr("::ffff:192.0.2.9"))
	l.Failed(netip.MustParseAddr("192.0.2.9"))
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
	l.Failed(a)
	l.Failed(b)
	if l.Check(c) != bounce.AuthGateBusy {
		t.Fatal("an unknown source must wait while the table is full")
	}
	l.Failed(c) // not tracked
	if len(l.sources) != 2 {
		t.Fatal("the table grew past its bound")
	}
	if l.Check(a) != bounce.AuthGateOpen {
		t.Fatal("a known source with tokens left may try")
	}
	// Once the debt of the known sources has refilled and they are idle, space frees up.
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
