package main

import (
	"errors"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/zyno-io/thinpost/internal/bounce"
)

type smtpAuthSource struct {
	limiter *rate.Limiter
	seen    time.Time
}

// smtpAuthLimiter bounds failed submission AUTH attempts per IPv4 address or
// IPv6 /64: each failure takes a token from a bucket that refills slowly, and
// an address with no token left is blocked. Sources with no debt are dropped
// when the bounded table is full; when it is still full, unknown sources are
// told to retry later instead of being tracked.
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

// evictIdle drops sources whose bucket has fully refilled. Callers hold l.mu.
func (l *smtpAuthLimiter) evictIdle(now time.Time) {
	for key, source := range l.sources {
		if now.Sub(source.seen) >= l.idle && source.limiter.TokensAt(now) >= float64(l.burst) {
			delete(l.sources, key)
		}
	}
}

// Check implements bounce.AuthLimiter.
func (l *smtpAuthLimiter) Check(address netip.Addr) bounce.AuthGate {
	key, ok := authKey(address)
	if !ok {
		return bounce.AuthGateBusy
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if source := l.sources[key]; source != nil {
		if source.limiter.TokensAt(now) < 1 {
			return bounce.AuthGateBlocked
		}
		return bounce.AuthGateOpen
	}
	if len(l.sources) >= l.maxSources {
		l.evictIdle(now)
		if len(l.sources) >= l.maxSources {
			return bounce.AuthGateBusy
		}
	}
	return bounce.AuthGateOpen
}

// Failed implements bounce.AuthLimiter.
func (l *smtpAuthLimiter) Failed(address netip.Addr) {
	key, ok := authKey(address)
	if !ok {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	source := l.sources[key]
	if source == nil {
		if len(l.sources) >= l.maxSources {
			l.evictIdle(now)
			if len(l.sources) >= l.maxSources {
				return
			}
		}
		source = &smtpAuthSource{limiter: rate.NewLimiter(rate.Every(l.interval), l.burst)}
		l.sources[key] = source
	}
	source.seen = now
	source.limiter.AllowN(now, 1)
}
