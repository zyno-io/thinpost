package messagebody

import (
	"context"
	"errors"
	"io"
	"log"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

const maxInboundObject = 16 << 20

// Audit actions of Enhanced Protection reads of received content (the resource
// is the inbound message ID).
const (
	InboundBodyReadAction = "inbound.body.read"
	InboundRawReadAction  = "inbound.raw.read"
)

// InboundRepository authorizes a read of a received message's stored content. It
// refuses a message past its retention with admin.ErrContentExpired, audits an
// Enhanced Protection read before it returns, and returns the object reference
// with everything the renderer needs except the bytes.
type InboundRepository interface {
	AuthorizeInboundRead(ctx context.Context, actor, appID, inboundID, action string) (key string, enhancedProtection bool, in format.InboundInput, err error)
}

// InboundObjects reads and decrypts a stored message, reporting a missing object
// as admin.ErrContentExpired: the bucket's lifecycle rule may delete it before
// the database retention time, and either way the content is gone.
type InboundObjects interface {
	GetInbound(ctx context.Context, key string, maximum int64) ([]byte, error)
}

// maxInboundReads bounds the stored messages held in memory by console reads at
// once (each is at most 10 MiB, plus the render); more wait their turn.
const maxInboundReads = 4

// InboundService serves content and raw reads of received messages. The decoding
// is the webhook renderer's (internal/webhook/format), so the console and the
// payload agree.
type InboundService struct {
	Repository InboundRepository
	Objects    InboundObjects
	slots      chan struct{}
}

// NewInboundService returns a service allowing a few concurrent reads.
func NewInboundService(repo InboundRepository, objects InboundObjects) *InboundService {
	return &InboundService{Repository: repo, Objects: objects, slots: make(chan struct{}, maxInboundReads)}
}

func (s *InboundService) ready() bool {
	return s != nil && s.Repository != nil && s.Objects != nil && s.slots != nil
}

// acquire takes a read slot or fails when the request ends first.
func (s *InboundService) acquire(ctx context.Context) error {
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *InboundService) release() { <-s.slots }

// load authorizes (and audits) the read, then fetches the message. The slot is
// held by the caller only while the bytes are in memory.
func (s *InboundService) load(ctx context.Context, actor, appID, inboundID, action string) (format.InboundInput, bool, error) {
	key, protected, in, err := s.Repository.AuthorizeInboundRead(ctx, actor, appID, inboundID, action)
	if err != nil {
		return in, false, err
	}
	raw, err := s.Objects.GetInbound(ctx, key, maxInboundObject)
	if err != nil {
		if errors.Is(err, admin.ErrContentExpired) {
			// The database said the content was still kept: the bucket's rule ran early.
			log.Printf("inbound content missing before retention id=%s", inboundID)
		}
		return in, false, err
	}
	in.Raw = raw
	return in, protected, nil
}

// ReadInboundContent returns the decoded headers and first text and HTML parts of
// the message, in the existing MessageBody shape. Attachments are not listed.
func (s *InboundService) ReadInboundContent(ctx context.Context, actor, appID, inboundID string) (admin.MessageBody, error) {
	if !s.ready() {
		return admin.MessageBody{}, errStorage
	}
	if err := s.acquire(ctx); err != nil {
		return admin.MessageBody{}, errStorage
	}
	defer s.release()
	in, protected, err := s.load(ctx, actor, appID, inboundID, InboundBodyReadAction)
	if err != nil {
		return admin.MessageBody{}, err
	}
	view := format.ViewInbound(&in)
	body := admin.MessageBody{
		To: view.To, Cc: view.Cc, ReplyTo: view.ReplyTo, Subject: view.Subject, Text: view.Text, HTML: view.HTML,
		Attachments: []admin.AttachmentMetadata{}, Omitted: view.Omitted, EnhancedProtection: protected,
	}
	if len(view.From) > 0 {
		body.From = view.From[0]
	}
	if body.To == nil {
		body.To = []string{}
	}
	return body, nil
}

type inboundRaw struct {
	service *InboundService
	in      format.InboundInput
	size    int64
}

func (r *inboundRaw) Size() int64 { return r.size }

// WriteTo streams the message and releases the read slot; it must be called once.
func (r *inboundRaw) WriteTo(w io.Writer) (int64, error) {
	defer r.service.release()
	n, err := format.WriteInboundRaw(w, &r.in)
	r.in.Raw = nil
	return n, err
}

// OpenInboundRaw authorizes the read and fetches the message, and returns it
// ready to stream: Thinpost's Authentication-Results and Received fields, then
// the stored bytes without forged same-authserv-id fields (the webhook's raw
// field before base64). The caller must call WriteTo.
func (s *InboundService) OpenInboundRaw(ctx context.Context, actor, appID, inboundID string) (admin.InboundRaw, error) {
	if !s.ready() {
		return nil, errStorage
	}
	if err := s.acquire(ctx); err != nil {
		return nil, errStorage
	}
	// The slot is released on every path that does not hand it to the returned
	// file, a panic included.
	ok := false
	defer func() {
		if !ok {
			s.release()
		}
	}()
	in, _, err := s.load(ctx, actor, appID, inboundID, InboundRawReadAction)
	if err != nil {
		return nil, err
	}
	file := &inboundRaw{service: s, in: in, size: format.InboundRawSize(&in)}
	ok = true
	return file, nil
}
