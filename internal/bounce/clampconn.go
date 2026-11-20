package bounce

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// clampMode selects how a clampConn treats reads.
type clampMode uint8

const (
	// modePassthrough is the default and always applies before AUTH: the
	// server's own command deadlines apply, but never beyond preAuth from
	// connect, so idle sockets cannot hold admission slots.
	modePassthrough clampMode = iota
	// modeAuthenticated clamps the server's command deadlines to the
	// connection lifetime.
	modeAuthenticated
	// modeTransaction (MAIL until Reset) replaces the server's absolute
	// deadline with an idle timeout, bounded by the transaction and the
	// lifetime. There is deliberately no wire byte cap: the decoded-byte limit
	// is enforced by the server and the session, and heavy dot-stuffing cannot
	// trip a byte cap.
	modeTransaction
	// modeDrain applies after DATA was refused early: the rest of the stream is
	// read within a total time, an idle time and a wire byte cap, or the
	// connection is closed.
	modeDrain
)

// clampLimits are the bounds of the modes. Tests shrink them.
type clampLimits struct {
	// preAuth bounds the whole connection before AUTH succeeds.
	preAuth     time.Duration
	idle        time.Duration
	transaction time.Duration
	lifetime    time.Duration
	drainTotal  time.Duration
	drainIdle   time.Duration
	drainBytes  int64
}

func defaultClampLimits() clampLimits {
	return clampLimits{preAuth: 2 * time.Minute, idle: 30 * time.Second, transaction: 10 * time.Minute, lifetime: 30 * time.Minute, drainTotal: 60 * time.Second, drainIdle: 30 * time.Second, drainBytes: 2 * MaxSubmissionBytes}
}

// clampConn is the outermost non-TLS wrapper of a submission or internal
// connection. The session reaches it through (*tls.Conn).NetConn(), which works
// for implicit TLS and for STARTTLS alike. It is never installed on the MX
// listener, so the MX's command deadlines are unchanged.
type clampConn struct {
	net.Conn
	limits clampLimits
	born   time.Time

	mu         sync.Mutex
	mode       clampMode
	authed     bool
	requested  time.Time // the server's own read deadline
	txnStart   time.Time
	drainStart time.Time
	drained    int64
}

func newClampConn(c net.Conn, limits clampLimits) *clampConn {
	return &clampConn{Conn: c, limits: limits, born: time.Now()}
}

func earliest(first time.Time, rest ...time.Time) time.Time {
	for _, t := range rest {
		if t.Before(first) {
			first = t
		}
	}
	return first
}

// SetReadDeadline records the server's deadline. In the transaction and drain
// modes the clamp's own deadlines replace it.
func (c *clampConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.requested = t
	passthrough := c.mode == modePassthrough
	c.mu.Unlock()
	if passthrough {
		return c.Conn.SetReadDeadline(c.preAuthDeadline(t))
	}
	return nil
}

func (c *clampConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.Conn.SetWriteDeadline(t)
}

func (c *clampConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.mode == modePassthrough {
		requested := c.requested
		c.mu.Unlock()
		_ = c.Conn.SetReadDeadline(c.preAuthDeadline(requested))
		return c.Conn.Read(p)
	}
	now := time.Now()
	lifetime := c.born.Add(c.limits.lifetime)
	deadline := lifetime
	switch c.mode {
	case modeAuthenticated:
		if !c.requested.IsZero() {
			deadline = earliest(deadline, c.requested)
		}
	case modeTransaction:
		deadline = earliest(deadline, now.Add(c.limits.idle), c.txnStart.Add(c.limits.transaction))
	case modeDrain:
		deadline = earliest(deadline, now.Add(c.limits.drainIdle), c.drainStart.Add(c.limits.drainTotal))
		if remaining := c.limits.drainBytes - c.drained; remaining <= 0 {
			c.mu.Unlock()
			_ = c.Conn.Close()
			return 0, io.ErrClosedPipe
		} else if int64(len(p)) > remaining+1 {
			p = p[:remaining+1]
		}
	}
	c.mu.Unlock()
	_ = c.Conn.SetReadDeadline(deadline)
	n, err := c.Conn.Read(p)
	if err != nil {
		// A stalled upload must end the connection: if only the read failed, the
		// server would answer and then run the rest of the body as commands.
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			c.mu.Lock()
			bounded := c.mode == modeTransaction || c.mode == modeDrain
			c.mu.Unlock()
			if bounded {
				_ = c.Conn.Close()
			}
		}
	}
	if n > 0 {
		c.mu.Lock()
		over := false
		if c.mode == modeDrain {
			c.drained += int64(n)
			over = c.drained > c.limits.drainBytes
		}
		c.mu.Unlock()
		if over {
			_ = c.Conn.Close()
			return n, io.ErrClosedPipe
		}
	}
	return n, err
}

// preAuthDeadline is the server's deadline (zero for none), never later than
// the pre-authentication limit from connect.
func (c *clampConn) preAuthDeadline(requested time.Time) time.Time {
	limit := c.born.Add(c.limits.preAuth)
	if c.limits.preAuth <= 0 || requested.IsZero() {
		if c.limits.preAuth <= 0 {
			return requested
		}
		return limit
	}
	return earliest(limit, requested)
}

// authenticate enters the authenticated mode after a successful AUTH.
func (c *clampConn) authenticate() {
	c.mu.Lock()
	c.authed = true
	if c.mode == modePassthrough {
		c.mode = modeAuthenticated
	}
	c.mu.Unlock()
}

// logout returns to passthrough (a new session on the same connection, as
// after STARTTLS, starts unauthenticated).
func (c *clampConn) logout() {
	c.mu.Lock()
	c.authed, c.mode = false, modePassthrough
	c.mu.Unlock()
}

// beginTransaction starts the transaction mode (MAIL).
func (c *clampConn) beginTransaction() {
	c.mu.Lock()
	if c.authed {
		c.mode, c.txnStart = modeTransaction, time.Now()
	}
	c.mu.Unlock()
}

// beginDrain starts the drain mode after an early DATA refusal.
func (c *clampConn) beginDrain() {
	c.mu.Lock()
	if c.authed {
		c.mode, c.drainStart, c.drained = modeDrain, time.Now(), 0
	}
	c.mu.Unlock()
}

// endTransaction returns the connection to the authenticated mode (Reset).
func (c *clampConn) endTransaction() {
	c.mu.Lock()
	if c.mode == modeTransaction || c.mode == modeDrain {
		c.mode = modeAuthenticated
	}
	c.mu.Unlock()
}

func (c *clampConn) currentMode() clampMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

type clampListener struct {
	net.Listener
	limits clampLimits
}

func (l *clampListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return newClampConn(conn, l.limits), nil
}

// ClampListener wraps every accepted connection of listener in a clampConn.
// Install it below TLS (and above any admission wrapper) on the submission and
// internal listeners only.
func ClampListener(listener net.Listener) net.Listener {
	return &clampListener{Listener: listener, limits: defaultClampLimits()}
}
