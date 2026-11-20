package main

import (
	"errors"
	"net"
	"net/netip"
	"sync"
)

// smtpAdmission bounds accepted SMTP connections across every listener: first
// all of them together, then each source address. One admission is shared by all
// listeners so a source cannot multiply its allowance by using more of the
// Edge's addresses. The total bound also bounds the in-memory source map.
type smtpAdmission struct {
	sem    chan struct{}
	limit  int
	total  int
	mu     sync.Mutex
	active map[netip.Addr]int
	stats  *smtpAdmissionStats
	// countAdmitted makes the admission count admitted connections itself, for
	// a listener that has no rate limiter.
	countAdmitted bool
}

func newSMTPAdmission(total, perSource int, stats *smtpAdmissionStats) (*smtpAdmission, error) {
	if total <= 0 || perSource <= 0 || perSource > total {
		return nil, errors.New("invalid SMTP connection limits")
	}
	if stats != nil {
		stats.connectionLimit.Store(int64(total))
	}
	return &smtpAdmission{sem: make(chan struct{}, total), limit: perSource, total: total, active: make(map[netip.Addr]int), stats: stats}, nil
}

// Wrap returns listener limited by this admission. The caller keeps no other
// reference to the original listener.
func (a *smtpAdmission) Wrap(listener net.Listener) net.Listener {
	return &admittedListener{Listener: listener, admission: a, done: make(chan struct{})}
}

type admittedListener struct {
	net.Listener
	admission *smtpAdmission
	done      chan struct{}
	closeOnce sync.Once
}

func (l *admittedListener) Close() error {
	err := l.Listener.Close()
	l.closeOnce.Do(func() { close(l.done) })
	return err
}

// acquire takes a total slot before the underlying Accept, so connections
// beyond the bound wait in the kernel backlog rather than in this process.
func (l *admittedListener) acquire() bool {
	select {
	case l.admission.sem <- struct{}{}:
		return true
	case <-l.done:
		return false
	}
}

func (l *admittedListener) Accept() (net.Conn, error) {
	a := l.admission
	for {
		if !l.acquire() {
			return nil, net.ErrClosed
		}
		raw, err := l.Listener.Accept()
		if err != nil {
			<-a.sem
			return nil, err
		}
		a.stats.connectionOpened(a.total)
		conn := net.Conn(&countedSMTPConn{Conn: &semaphoreConn{Conn: raw, release: func() { <-a.sem }}, stats: a.stats})
		remote, ok := conn.RemoteAddr().(*net.TCPAddr)
		if !ok || remote.Zone != "" {
			a.stats.addInvalid()
			_ = conn.Close()
			continue
		}
		address, ok := netip.AddrFromSlice(remote.IP)
		if !ok || address.IsUnspecified() {
			a.stats.addInvalid()
			_ = conn.Close()
			continue
		}
		address = address.Unmap()
		a.mu.Lock()
		if a.active[address] >= a.limit {
			a.mu.Unlock()
			a.stats.addConcurrentLimit()
			_ = conn.Close()
			continue
		}
		a.active[address]++
		a.mu.Unlock()
		if a.countAdmitted {
			a.stats.addAdmitted()
		}
		return &sourceLimitedConn{Conn: conn, release: func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.active[address]--
			if a.active[address] == 0 {
				delete(a.active, address)
			}
		}}, nil
	}
}

type sourceLimitedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *sourceLimitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// semaphoreConn gives its total slot back once, after closing the connection.
type semaphoreConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *semaphoreConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// Release the observation before the underlying limiter releases its slot, so
// concurrent Accept calls cannot transiently count more than the configured limit.
type countedSMTPConn struct {
	net.Conn
	stats *smtpAdmissionStats
	once  sync.Once
}

func (c *countedSMTPConn) Close() error {
	c.once.Do(c.stats.connectionClosed)
	return c.Conn.Close()
}
