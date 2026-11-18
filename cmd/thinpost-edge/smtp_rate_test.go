package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSMTPAdmissionCountersAndLogExcludeSources(t *testing.T) {
	stats := &smtpAdmissionStats{}
	limiter, err := newSMTPRateLimiter(time.Hour, time.Hour, 2, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	limiter.stats = stats
	now := time.Now()
	a := netip.MustParseAddr("192.0.2.1")
	b := netip.MustParseAddr("192.0.2.2")
	if !limiter.allow(a, now) || limiter.allow(a, now) || limiter.allow(b, now) || limiter.allow(b, now) || limiter.allow(netip.Addr{}, now) {
		t.Fatal("unexpected admission results")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := make(chan string, 1)
	go logSMTPAdmissionStats(ctx, stats, time.Millisecond, func(format string, args ...any) {
		select {
		case logs <- fmt.Sprintf(format, args...):
		default:
		}
	})
	select {
	case line := <-logs:
		for _, expected := range []string{"admitted=1", "invalid=1", "source_rate_limit=1", "global_rate_limit=1", "source_table_full=1", "active_connections=0", "connection_limit=0", "saturation_entries=0"} {
			if !strings.Contains(line, expected) {
				t.Fatalf("missing %q in %q", expected, line)
			}
		}
		if strings.Contains(line, a.String()) || strings.Contains(line, b.String()) {
			t.Fatal("admission log exposed a source address")
		}
	case <-time.After(time.Second):
		t.Fatal("admission statistics were not logged")
	}
}

func TestSMTPRateLimitsSourceGlobalAndIPv6Prefix(t *testing.T) {
	l, err := newSMTPRateLimiter(time.Second, time.Second, 4, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	allow := func(s string, at time.Time) bool { return l.allow(netip.MustParseAddr(s), at) }
	if !allow("192.0.2.1", now) || !allow("::ffff:192.0.2.1", now) || allow("192.0.2.1", now) {
		t.Fatal("IPv4 and mapped source did not share the source burst")
	}
	if !allow("2001:db8:1:2::1", now) || !allow("2001:db8:1:2::2", now) || allow("2001:db8:1:2::3", now) {
		t.Fatal("IPv6 /64 did not share the source burst")
	}
	if allow("2001:db8:1:3::1", now) {
		t.Fatal("global burst was not enforced")
	}
	if !allow("2001:db8:1:3::1", now.Add(time.Second)) {
		t.Fatal("global budget did not replenish")
	}
	if !allow("192.0.2.1", now.Add(2*time.Second)) {
		t.Fatal("source budget did not replenish")
	}
	if l.allow(netip.Addr{}, now) || l.allow(netip.MustParseAddr("::"), now) || l.allow(netip.MustParseAddr("fe80::1%eth0"), now) {
		t.Fatal("invalid source was accepted")
	}
}

func TestSMTPRateTableRetainsDebtAndFailsClosed(t *testing.T) {
	l, err := newSMTPRateLimiter(10*time.Millisecond, time.Minute, 10, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a, b := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	if !l.allow(a, now) || !l.allow(a, now) {
		t.Fatal("initial source denied")
	}
	if l.allow(b, now.Add(time.Minute)) {
		t.Fatal("evicted a source before its bucket refilled")
	}
	if !l.allow(b, now.Add(2*time.Minute)) {
		t.Fatal("fully refilled idle source was not reclaimed")
	}
	if len(l.sources) != 1 {
		t.Fatal("source table exceeded its bound")
	}
}

func TestSMTPRateLimiterSerializesConcurrentAdmissions(t *testing.T) {
	l, err := newSMTPRateLimiter(time.Second, time.Second, 100, 4, 20)
	if err != nil {
		t.Fatal(err)
	}
	address, now := netip.MustParseAddr("192.0.2.1"), time.Now()
	var wg sync.WaitGroup
	results := make(chan bool, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- l.allow(address, now) }()
	}
	wg.Wait()
	close(results)
	accepted := 0
	for ok := range results {
		if ok {
			accepted++
		}
	}
	if accepted != 4 {
		t.Fatalf("accepted %d, want 4", accepted)
	}
}

func TestSMTPRateListenerClosesExcessAndReleasesConnectionSlot(t *testing.T) {
	raw := &testListener{connections: make(chan net.Conn, 5), closed: make(chan struct{})}
	limited, err := limitSMTPConnections(raw, 3, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	rateLimit, err := newSMTPRateLimiter(time.Second, time.Hour, 10, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	listener := &smtpRateListener{Listener: limited, limiter: rateLimit}
	defer listener.Close()
	firstClient := queueConn(raw, "192.0.2.1")
	defer firstClient.Close()
	first, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	rejected := queueConn(raw, "192.0.2.1")
	defer rejected.Close()
	otherClient := queueConn(raw, "192.0.2.2")
	defer otherClient.Close()
	other, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := rejected.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("rate-rejected connection remained open: %v", err)
	}
}

func TestSMTPRateRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		global, source time.Duration
		gb, sb, max    int
	}{
		{0, time.Second, 1, 1, 1}, {time.Second, time.Hour + 1, 1, 1, 1}, {time.Second, time.Second, 0, 1, 1}, {time.Second, time.Second, 1, 1001, 1}, {time.Second, time.Second, 1, 1, 0},
	} {
		if _, err := newSMTPRateLimiter(tc.global, tc.source, tc.gb, tc.sb, tc.max); err == nil {
			t.Fatal("accepted invalid rate configuration")
		}
	}
}
