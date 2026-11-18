package smtptransport

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
)

// Timeouts bounds individual SMTP stages and each message data block. The
// caller additionally owns cancellation and any overall attempt deadline.
type Timeouts struct {
	Connect, Greeting, Command, DataInit, DataBlock, DataFinal time.Duration
}

func DefaultTimeouts() Timeouts {
	return Timeouts{Connect: 5 * time.Minute, Greeting: 5 * time.Minute, Command: 5 * time.Minute, DataInit: 2 * time.Minute, DataBlock: 3 * time.Minute, DataFinal: 10 * time.Minute}
}

func (t Timeouts) Validate() error {
	for _, d := range []time.Duration{t.Connect, t.Greeting, t.Command, t.DataInit, t.DataBlock, t.DataFinal} {
		if d < time.Second || d > 30*time.Minute {
			return errors.New("invalid SMTP stage timeout")
		}
	}
	return nil
}

// setStageDeadline never extends the caller's deadline. Recheck cancellation
// after setting it so an already-fired cancellation cannot be overwritten.
func setStageDeadline(ctx context.Context, conn net.Conn, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	if outer, ok := ctx.Deadline(); ok && outer.Before(deadline) {
		deadline = outer
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = conn.SetDeadline(time.Now())
		return err
	}
	return nil
}

type blockTimeoutWriter struct {
	ctx     context.Context
	conn    net.Conn
	timeout time.Duration
	writer  io.Writer
}

func (w blockTimeoutWriter) Write(p []byte) (int, error) {
	if err := setStageDeadline(w.ctx, w.conn, w.timeout); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}
