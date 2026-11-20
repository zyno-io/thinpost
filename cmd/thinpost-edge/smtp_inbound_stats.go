package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// inboundStats holds cumulative inbound SMTP counters by fixed name (recipient
// and DATA outcomes). It stores no address, domain or content. A restart resets
// all counters.
type inboundStats struct {
	mu     sync.Mutex
	counts map[string]uint64
	// name is the log line prefix; empty means "smtp_inbound".
	name string
}

func (s *inboundStats) prefix() string {
	if s.name != "" {
		return s.name
	}
	return "smtp_inbound"
}

func (s *inboundStats) Count(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts == nil {
		s.counts = make(map[string]uint64)
	}
	// Names are fixed by the receiver; the bound only guards a future mistake.
	if _, known := s.counts[name]; known || len(s.counts) < 64 {
		s.counts[name]++
	}
}

func (s *inboundStats) line() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.counts))
	for name := range s.counts {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(s.prefix())
	for _, name := range names {
		fmt.Fprintf(&b, " %s=%d", name, s.counts[name])
	}
	return b.String()
}

// logInboundStats prints the counters once a minute, and only after the first
// inbound event so an idle worker stays quiet.
func logInboundStats(ctx context.Context, s *inboundStats, interval time.Duration, printf func(string, ...any)) {
	if s == nil || interval <= 0 || printf == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if line := s.line(); line != s.prefix() {
				printf("%s", line)
			}
		}
	}
}
