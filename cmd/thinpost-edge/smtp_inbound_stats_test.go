package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInboundStatsLineHasOnlyFixedNamesAndCounts(t *testing.T) {
	s := &inboundStats{}
	if s.line() != "smtp_inbound" {
		t.Fatal("an idle worker logs a line")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Count("rcpt_accept")
			s.Count("data_accept")
			s.Count("rcpt_accept")
		}()
	}
	wg.Wait()
	if got := s.line(); got != "smtp_inbound data_accept=8 rcpt_accept=16" {
		t.Fatal(got)
	}
	// A runaway set of names cannot grow the table without bound.
	for i := 0; i < 500; i++ {
		s.Count("name" + strings.Repeat("x", i%100) + string(rune('a'+i%26)))
	}
	if len(s.counts) > 64 {
		t.Fatalf("%d counters", len(s.counts))
	}
	var nilStats *inboundStats
	nilStats.Count("x")
}

func TestInboundStatsAreLoggedPeriodicallyAndOnlyWhenActive(t *testing.T) {
	s := &inboundStats{}
	lines := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go logInboundStats(ctx, s, 20*time.Millisecond, func(format string, args ...any) {
		if len(args) > 0 {
			lines <- args[0].(string)
		}
	})
	select {
	case line := <-lines:
		t.Fatalf("an idle worker logged %q", line)
	case <-time.After(100 * time.Millisecond):
	}
	s.Count("data_accept")
	select {
	case line := <-lines:
		if line != "smtp_inbound data_accept=1" {
			t.Fatal(line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no log line")
	}
}
