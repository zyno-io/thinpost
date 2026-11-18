package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeNet struct {
	mu       sync.Mutex
	sent     map[netip.Addr]int
	resolved bool
}

func (f *fakeNet) dial(src, gw netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[src]++
	return nil
}
func (f *fakeNet) arp() ([]arpEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	flags := uint64(0)
	if f.resolved {
		flags = 2
	}
	return []arpEntry{
		{Address: netip.MustParseAddr("192.0.2.1"), Flags: flags, Device: "net1"},
		{Address: netip.MustParseAddr("192.0.2.2"), Flags: flags, Device: "net2"},
		// Right gateway, wrong interface: must not count for net2's source.
		{Address: netip.MustParseAddr("192.0.2.2"), Flags: 0x6, Device: "eth0"},
	}, nil
}
func (f *fakeNet) set(v bool) { f.mu.Lock(); f.resolved = v; f.mu.Unlock() }
func (f *fakeNet) count(a netip.Addr) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[a]
}

var (
	srcA = netip.MustParseAddr("198.51.100.10")
	srcB = netip.MustParseAddr("198.51.100.20")
)

func newTestAnnouncer(f *fakeNet, logs *[]string, mu *sync.Mutex) *sourceAnnouncer {
	a, err := newSourceAnnouncer([]netip.Addr{srcA, srcB}, []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}, func(format string, args ...any) {
		mu.Lock()
		*logs = append(*logs, format)
		mu.Unlock()
	})
	if err != nil {
		panic(err)
	}
	a.dial, a.arp = f.dial, f.arp
	a.ifaceOf = func(s netip.Addr) (string, error) {
		if s == srcA {
			return "net1", nil
		}
		return "net2", nil
	}
	a.poll, a.budget, a.backoff, a.refresh = 2*time.Millisecond, 20*time.Millisecond, 6*time.Millisecond, 15*time.Millisecond
	return a
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for", what)
}

func TestAnnouncerBecomesReadyAndLogsOneFixedLine(t *testing.T) {
	f := &fakeNet{sent: map[netip.Addr]int{}, resolved: true}
	var logs []string
	var mu sync.Mutex
	a := newTestAnnouncer(f, &logs, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.run(ctx)
	waitFor(t, "ready", a.ready)
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 1 || logs[0] != "source neighbours ready n=%d" {
		t.Fatalf("logs: %v", logs)
	}
	if f.count(srcA) < 1 || f.count(srcB) < 1 {
		t.Fatal("each source must send")
	}
}

func TestAnnouncerBudgetExpiryIsNotReadyThenRecovers(t *testing.T) {
	f := &fakeNet{sent: map[netip.Addr]int{}}
	var logs []string
	var mu sync.Mutex
	a := newTestAnnouncer(f, &logs, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.run(ctx)
	waitFor(t, "budget expiry", a.expired.Load)
	if a.ready() {
		t.Fatal("must not be ready with unresolved gateways")
	}
	before := f.count(srcA)
	waitFor(t, "retry after expiry", func() bool { return f.count(srcA) > before })
	f.set(true)
	waitFor(t, "ready after late resolution", a.ready)
	mu.Lock()
	defer mu.Unlock()
	pendingLines := 0
	for _, l := range logs {
		if l == announcePendingLogLabel {
			pendingLines++
		}
	}
	if pendingLines != 1 {
		t.Fatalf("pending line logged %d times, want once: %v", pendingLines, logs)
	}
}

func TestAnnouncerResendsPeriodicallyAfterReady(t *testing.T) {
	f := &fakeNet{sent: map[netip.Addr]int{}, resolved: true}
	var logs []string
	var mu sync.Mutex
	a := newTestAnnouncer(f, &logs, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.run(ctx)
	waitFor(t, "ready", a.ready)
	base := f.count(srcA)
	waitFor(t, "periodic resend", func() bool { return f.count(srcA) >= base+2 && f.count(srcB) >= base+2 })
}

func TestAnnouncerNeedsTheInterfaceHoldingTheSource(t *testing.T) {
	f := &fakeNet{sent: map[netip.Addr]int{}, resolved: true}
	var logs []string
	var mu sync.Mutex
	a := newTestAnnouncer(f, &logs, &mu)
	a.ifaceOf = func(netip.Addr) (string, error) { return "net9", nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.run(ctx)
	waitFor(t, "budget expiry", a.expired.Load)
	if a.ready() {
		t.Fatal("a complete entry on another interface must not count")
	}
}

func TestAnnouncerRequiresMatchingCounts(t *testing.T) {
	if _, err := newSourceAnnouncer([]netip.Addr{srcA, srcB}, []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil); err == nil {
		t.Fatal("mismatched counts accepted")
	}
}

func TestParseARP(t *testing.T) {
	in := "IP address       HW type     Flags       HW address            Mask     Device\n" +
		"192.0.2.1        0x1         0x2         00:00:5e:00:53:01     *        net1\n" +
		"192.0.2.2        0x1         0x0         00:00:00:00:00:00     *        net2\n" +
		"garbage\nnot-an-ip 0x1 0x2 aa *  net3\n"
	got, err := parseARP(strings.NewReader(in))
	if err != nil || len(got) != 2 || got[0].Flags != 2 || got[0].Device != "net1" || got[1].Flags != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestReadinessGatingAndLivenessIndependence(t *testing.T) {
	journalOK, nbrOK := true, false
	ready := allReady(func() bool { return journalOK }, func() bool { return nbrOK })
	live := func() bool { return journalOK }
	h := edgeHealthHandler(ready, live)
	code := func(path string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code
	}
	if code("/readyz") != 503 || code("/livez") != 204 {
		t.Fatal("unresolved neighbours must fail readiness only")
	}
	nbrOK = true
	if code("/readyz") != 204 {
		t.Fatal("ready once neighbours resolve")
	}
	journalOK = false
	if code("/readyz") != 503 || code("/livez") != 503 {
		t.Fatal("journal fault fails both")
	}
	if code("/other") != 404 {
		t.Fatal("unknown path")
	}
}
