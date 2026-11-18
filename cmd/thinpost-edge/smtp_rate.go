package main

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type smtpRateSource struct {
	limiter *rate.Limiter
	seen    time.Time
}

// smtpRateLimiter bounds accepted connection attempts globally and per IPv4
// address or IPv6 /64. It retains source debt until a bucket could have fully
// refilled; when the bounded table is full, unknown sources fail closed.
type smtpRateLimiter struct {
	mu             sync.Mutex
	global         *rate.Limiter
	sourceInterval time.Duration
	sourceBurst    int
	maxSources     int
	idle           time.Duration
	sources        map[netip.Prefix]*smtpRateSource
	stats          *smtpAdmissionStats
}

func newSMTPRateLimiter(globalInterval, sourceInterval time.Duration, globalBurst, sourceBurst, maxSources int) (*smtpRateLimiter, error) {
	if globalInterval < 10*time.Millisecond || globalInterval > time.Hour || sourceInterval < 10*time.Millisecond || sourceInterval > time.Hour || globalBurst < 1 || globalBurst > 1000 || sourceBurst < 1 || sourceBurst > 1000 || maxSources < 1 || maxSources > 65536 {
		return nil, errors.New("invalid SMTP connection rate limits")
	}
	idle := sourceInterval * time.Duration(sourceBurst)
	if idle < time.Minute {
		idle = time.Minute
	}
	return &smtpRateLimiter{global: rate.NewLimiter(rate.Every(globalInterval), globalBurst), sourceInterval: sourceInterval, sourceBurst: sourceBurst, maxSources: maxSources, idle: idle, sources: make(map[netip.Prefix]*smtpRateSource)}, nil
}

func (l *smtpRateLimiter) allow(address netip.Addr, now time.Time) bool {
	address = address.Unmap()
	if !address.IsValid() || address.IsUnspecified() || address.Zone() != "" {
		l.stats.addInvalid()
		return false
	}
	bits := 32
	if address.Is6() {
		bits = 64
	}
	key := netip.PrefixFrom(address, bits).Masked()
	l.mu.Lock()
	defer l.mu.Unlock()
	if source := l.sources[key]; source != nil {
		source.seen = now
		if !source.limiter.AllowN(now, 1) {
			l.stats.addSourceRate()
			return false
		}
		if !l.global.AllowN(now, 1) {
			l.stats.addGlobalRate()
			return false
		}
		l.stats.addAdmitted()
		return true
	}
	// A global token bounds both admission and the cost of scanning a full map.
	if !l.global.AllowN(now, 1) {
		l.stats.addGlobalRate()
		return false
	}
	if len(l.sources) >= l.maxSources {
		for key, source := range l.sources {
			if now.Sub(source.seen) >= l.idle && source.limiter.TokensAt(now) >= float64(l.sourceBurst) {
				delete(l.sources, key)
			}
		}
		if len(l.sources) >= l.maxSources {
			l.stats.addTableFull()
			return false
		}
	}
	source := &smtpRateSource{limiter: rate.NewLimiter(rate.Every(l.sourceInterval), l.sourceBurst), seen: now}
	if !source.limiter.AllowN(now, 1) {
		l.stats.addSourceRate()
		return false
	}
	l.sources[key] = source
	l.stats.addAdmitted()
	return true
}

type smtpRateListener struct {
	net.Listener
	limiter *smtpRateLimiter
}

func (l *smtpRateListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		remote, ok := conn.RemoteAddr().(*net.TCPAddr)
		if ok && remote.Zone == "" {
			address, valid := netip.AddrFromSlice(remote.IP)
			if valid && l.limiter.allow(address, time.Now()) {
				return conn, nil
			}
			if !valid {
				l.limiter.stats.addInvalid()
			}
		} else {
			l.limiter.stats.addInvalid()
		}
		_ = conn.Close()
	}
}
