package main

import (
	"context"
	"sync/atomic"
	"time"
)

// smtpAdmissionStats contains process-local, cumulative counters. It never
// stores remote addresses or SMTP data. A restart resets all counters.
type smtpAdmissionStats struct {
	// name is the log line prefix; empty means "smtp_admission".
	name              string
	activeConnections atomic.Int64
	connectionLimit   atomic.Int64
	saturationEntries atomic.Uint64
	admitted          atomic.Uint64
	invalid           atomic.Uint64
	concurrentLimit   atomic.Uint64
	sourceRate        atomic.Uint64
	globalRate        atomic.Uint64
	tableFull         atomic.Uint64
}

func (s *smtpAdmissionStats) addAdmitted() {
	if s != nil {
		s.admitted.Add(1)
	}
}
func (s *smtpAdmissionStats) addInvalid() {
	if s != nil {
		s.invalid.Add(1)
	}
}
func (s *smtpAdmissionStats) addConcurrentLimit() {
	if s != nil {
		s.concurrentLimit.Add(1)
	}
}
func (s *smtpAdmissionStats) addSourceRate() {
	if s != nil {
		s.sourceRate.Add(1)
	}
}
func (s *smtpAdmissionStats) addGlobalRate() {
	if s != nil {
		s.globalRate.Add(1)
	}
}
func (s *smtpAdmissionStats) addTableFull() {
	if s != nil {
		s.tableFull.Add(1)
	}
}

// Connection occupancy is an instantaneous process-local observation. The
// saturation counter counts entries into the full state, not rejected clients.
func (s *smtpAdmissionStats) connectionOpened(limit int) {
	if s == nil {
		return
	}
	active := s.activeConnections.Add(1)
	if active == int64(limit) {
		s.saturationEntries.Add(1)
	}
}

func (s *smtpAdmissionStats) connectionClosed() {
	if s != nil {
		s.activeConnections.Add(-1)
	}
}

func logSMTPAdmissionStats(ctx context.Context, s *smtpAdmissionStats, interval time.Duration, printf func(string, ...any)) {
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
			printf("%s admitted=%d invalid=%d concurrent_source_limit=%d source_rate_limit=%d global_rate_limit=%d source_table_full=%d active_connections=%d connection_limit=%d saturation_entries=%d", s.logName(), s.admitted.Load(), s.invalid.Load(), s.concurrentLimit.Load(), s.sourceRate.Load(), s.globalRate.Load(), s.tableFull.Load(), s.activeConnections.Load(), s.connectionLimit.Load(), s.saturationEntries.Load())
		}
	}
}

func (s *smtpAdmissionStats) logName() string {
	if s.name != "" {
		return s.name
	}
	return "smtp_admission"
}
