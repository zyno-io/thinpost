package main

import (
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/zyno-io/thinpost/internal/bounce"
)

// smtpAuthSource is a token bucket: attempts take a token, successes give it
// back, and it refills one token per interval.
type smtpAuthSource struct {
	tokens float64
	last   time.Time
	seen   time.Time
}

// smtpAuthLimiter bounds submission AUTH attempts per IPv4 address or IPv6
// /64. Every attempt is charged when it is checked, atomically with the check,
// so attempts in parallel cannot exceed the limit; a successful login refunds
// its attempt, so only failures accumulate. Sources with a full bucket are
// dropped when the bounded table is full; when it is still full, unknown
// sources are told to retry later instead of being tracked.
type smtpAuthLimiter struct {
	mu         sync.Mutex
	interval   time.Duration
	burst      int
	maxSources int
	idle       time.Duration
	sources    map[netip.Prefix]*smtpAuthSource
	now        func() time.Time
}

var _ bounce.AuthLimiter = (*smtpAuthLimiter)(nil)

func newSMTPAuthLimiter(interval time.Duration, burst, maxSources int) (*smtpAuthLimiter, error) {
	if interval < time.Millisecond || interval > time.Hour || burst < 1 || burst > 1000 || maxSources < 1 || maxSources > 65536 {
		return nil, errors.New("invalid SMTP authentication limits")
	}
	idle := interval * time.Duration(burst)
	if idle < time.Minute {
		idle = time.Minute
	}
	return &smtpAuthLimiter{interval: interval, burst: burst, maxSources: maxSources, idle: idle, sources: make(map[netip.Prefix]*smtpAuthSource), now: time.Now}, nil
}

func authKey(address netip.Addr) (netip.Prefix, bool) {
	address = address.Unmap()
	if !address.IsValid() || address.IsUnspecified() || address.Zone() != "" {
		return netip.Prefix{}, false
	}
	bits := 32
	if address.Is6() {
		bits = 64
	}
	return netip.PrefixFrom(address, bits).Masked(), true
}

// refill brings the bucket up to date. Callers hold l.mu.
func (l *smtpAuthLimiter) refill(s *smtpAuthSource, now time.Time) {
	if elapsed := now.Sub(s.last); elapsed > 0 {
		s.tokens = min(float64(l.burst), s.tokens+float64(elapsed)/float64(l.interval))
	}
	s.last = now
}

// evictIdle drops sources whose bucket has fully refilled. Callers hold l.mu.
func (l *smtpAuthLimiter) evictIdle(now time.Time) {
	for key, source := range l.sources {
		l.refill(source, now)
		if now.Sub(source.seen) >= l.idle && source.tokens >= float64(l.burst) {
			delete(l.sources, key)
		}
	}
}

// Check implements bounce.AuthLimiter: it charges one attempt.
func (l *smtpAuthLimiter) Check(address netip.Addr) bounce.AuthGate {
	key, ok := authKey(address)
	if !ok {
		return bounce.AuthGateBusy
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	source := l.sources[key]
	if source == nil {
		if len(l.sources) >= l.maxSources {
			l.evictIdle(now)
			if len(l.sources) >= l.maxSources {
				return bounce.AuthGateBusy
			}
		}
		source = &smtpAuthSource{tokens: float64(l.burst), last: now}
		l.sources[key] = source
	}
	l.refill(source, now)
	source.seen = now
	if source.tokens < 1 {
		return bounce.AuthGateBlocked
	}
	source.tokens--
	return bounce.AuthGateOpen
}

// Succeeded implements bounce.AuthLimiter: it refunds the attempt.
func (l *smtpAuthLimiter) Succeeded(address netip.Addr) {
	key, ok := authKey(address)
	if !ok {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if source := l.sources[key]; source != nil {
		l.refill(source, now)
		source.tokens = min(float64(l.burst), source.tokens+1)
	}
}
