package edgesession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/bounceingest"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/journal"
)

func validBounceID(value string) bool {
	if len(value) != 32 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// bounceBridge keeps at most one in-memory bounce per authenticated session.
// It never opens a connection: only a trusted-initiated stream can attach.
type bounceBridge struct {
	mu     sync.Mutex
	fence  journal.Fence
	active *bounceConnection
}
type bounceConnection struct {
	fence journal.Fence
	send  func(*controlv1.EdgeFrame) error
	valid func() bool
	done  chan struct{}
	busy  bool
	id    string
	ack   chan bool
}

func (b *bounceBridge) activate(fence journal.Fence) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if fence.Epoch < b.fence.Epoch || fence.Epoch == b.fence.Epoch && b.fence.ControllerID != "" && fence.ControllerID != b.fence.ControllerID {
		return journal.ErrStaleController
	}
	if fence != b.fence {
		if b.active != nil {
			close(b.active.done)
			b.active = nil
		}
		b.fence = fence
	}
	return nil
}
func (b *bounceBridge) attach(fence journal.Fence, send func(*controlv1.EdgeFrame) error, valid func() bool) (*bounceConnection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fence != fence {
		return nil, journal.ErrStaleController
	}
	if b.active != nil {
		close(b.active.done)
	}
	c := &bounceConnection{fence: fence, send: send, valid: valid, done: make(chan struct{})}
	b.active = c
	return c, nil
}
func (b *bounceBridge) detach(c *bounceConnection) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active == c {
		b.active = nil
		close(c.done)
	}
}
func (b *bounceBridge) acknowledge(c *bounceConnection, ack *controlv1.BounceAcknowledgement) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != c || ack == nil || !c.busy || ack.BounceId != c.id || !c.valid() {
		return
	}
	select {
	case c.ack <- ack.DurablyAccepted:
	default:
	}
}

// Forward has no disk spool. A timeout or lost stream returns an error so the
// public SMTP peer retries; a late durable commit may therefore be replayed.
func (b *bounceBridge) Forward(ctx context.Context, token, id string, body []byte) error {
	if !validBounceID(token) || !validBounceID(id) || len(body) == 0 || len(body) > bounceingest.MaxBytes {
		return errors.New("invalid bounce")
	}
	b.mu.Lock()
	c := b.active
	if c == nil || c.busy || !c.valid() {
		b.mu.Unlock()
		return errors.New("bounce stream unavailable")
	}
	c.busy = true
	c.id = id
	c.ack = make(chan bool, 1)
	ack := c.ack
	b.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		err := sendBounce(ctx, c, token, id, body)
		if err == nil {
			select {
			case ok := <-ack:
				if !ok || !c.valid() {
					err = errors.New("bounce commit failed")
				}
			case <-c.done:
				err = errors.New("bounce session closed")
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		b.mu.Lock()
		c.busy = false
		c.id = ""
		c.ack = nil
		b.mu.Unlock()
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("bounce session closed")
	}
}
func sendBounce(ctx context.Context, c *bounceConnection, token, id string, body []byte) error {
	if !c.valid() {
		return errors.New("controller lease expired")
	}
	digest := sha256.Sum256(body)
	if err := c.send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Bounce{Bounce: &controlv1.BounceStart{BounceId: id, RecipientToken: token, ExpectedSize: uint64(len(body)), Sha256: digest[:]}}}); err != nil {
		return err
	}
	for offset := 0; offset < len(body); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return errors.New("bounce session closed")
		default:
		}
		if !c.valid() {
			return errors.New("controller lease expired")
		}
		end := offset + edgeprotocol.MaxChunkBytes
		if end > len(body) {
			end = len(body)
		}
		chunk := &controlv1.DataChunk{Id: id, Offset: uint64(offset), Data: bytes.Clone(body[offset:end]), Last: end == len(body)}
		if err := c.send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_BounceChunk{BounceChunk: chunk}}); err != nil {
			return err
		}
		offset = end
	}
	return nil
}
