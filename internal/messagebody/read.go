// Package messagebody returns authorized and audited message contents: the
// original source body and attachments, and the header fields of the signed
// per-recipient MIME object. Authorization (and, for Enhanced Protection
// messages, the audit row) always commits before any object is fetched.
package messagebody

import (
	"context"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/submission"
)

const (
	maxSourceSize = 16 << 20
	maxFinalSize  = 16 << 20
)

// Repository authorizes a content read and returns the object reference and
// the message's Enhanced Protection flag. Implementations audit Enhanced
// Protection reads before returning.
type Repository interface {
	AuthorizeBodyRead(ctx context.Context, actor, appID, messageID string) (string, bool, error)
	AuthorizeAttachmentRead(ctx context.Context, actor, appID, messageID string, index int) (string, bool, error)
	AuthorizeDeliveryHeadersRead(ctx context.Context, actor, appID, messageID, deliveryID string) (string, bool, error)
}
type Objects interface {
	Get(context.Context, string, int64) ([]byte, error)
}
type Service struct {
	Repository Repository
	Objects    Objects
}

var errStorage = errors.New("message storage unavailable")
var errSource = errors.New("invalid message source")

// readSource fetches and strictly decodes a stored source object, checking
// that its recorded policy matches the database.
func (s *Service) readSource(ctx context.Context, key string, protected bool) (submission.Message, error) {
	data, err := s.Objects.Get(ctx, key, maxSourceSize)
	if err != nil {
		return submission.Message{}, errStorage
	}
	source, err := submission.DecodeSource(data, protected)
	if err != nil {
		return submission.Message{}, errSource
	}
	return source.Message, nil
}

func (s *Service) ready() bool { return s.Repository != nil && s.Objects != nil }

func (s *Service) ReadMessageBody(ctx context.Context, actor, appID, messageID string) (admin.MessageBody, error) {
	if !s.ready() {
		return admin.MessageBody{}, errStorage
	}
	key, protected, err := s.Repository.AuthorizeBodyRead(ctx, actor, appID, messageID)
	if err != nil {
		return admin.MessageBody{}, err
	}
	message, err := s.readSource(ctx, key, protected)
	if err != nil {
		return admin.MessageBody{}, err
	}
	// Always a non-nil slice: legacy sources without attachments report [].
	attachments := make([]admin.AttachmentMetadata, len(message.Attachments))
	for i, attachment := range message.Attachments {
		attachments[i] = admin.AttachmentMetadata{Index: i, Filename: attachment.Filename, ContentType: attachment.ContentType, Size: len(attachment.Content), Inline: attachment.Inline, ContentID: attachment.ContentID}
	}
	return admin.MessageBody{From: message.From, To: message.To, Cc: message.Cc, Bcc: message.Bcc, ReplyTo: message.ReplyTo, Subject: message.Subject, Text: message.Text, HTML: message.HTML, EnhancedProtection: message.EnhancedProtection, Attachments: attachments}, nil
}

// ReadAttachment returns one submitted attachment. The index is checked before
// authorization; an index beyond the message's attachments is not found.
func (s *Service) ReadAttachment(ctx context.Context, actor, appID, messageID string, index int) (admin.AttachmentContent, error) {
	if !s.ready() {
		return admin.AttachmentContent{}, errStorage
	}
	if index < 0 || index > admin.MaxAttachmentIndex {
		return admin.AttachmentContent{}, admin.ErrNotFound
	}
	key, protected, err := s.Repository.AuthorizeAttachmentRead(ctx, actor, appID, messageID, index)
	if err != nil {
		return admin.AttachmentContent{}, err
	}
	message, err := s.readSource(ctx, key, protected)
	if err != nil {
		return admin.AttachmentContent{}, err
	}
	if index >= len(message.Attachments) {
		return admin.AttachmentContent{}, admin.ErrNotFound
	}
	attachment := message.Attachments[index]
	return admin.AttachmentContent{Filename: attachment.Filename, ContentType: attachment.ContentType, Content: attachment.Content}, nil
}

// ReadDeliveryHeaders returns the header fields of the signed per-recipient
// MIME object in stored order. Only the header block is parsed and returned.
func (s *Service) ReadDeliveryHeaders(ctx context.Context, actor, appID, messageID, deliveryID string) ([]admin.HeaderField, error) {
	if !s.ready() {
		return nil, errStorage
	}
	key, _, err := s.Repository.AuthorizeDeliveryHeadersRead(ctx, actor, appID, messageID, deliveryID)
	if err != nil {
		return nil, err
	}
	data, err := s.Objects.Get(ctx, key, maxFinalSize)
	if err != nil {
		return nil, errStorage
	}
	headers, err := ParseHeaders(data)
	if err != nil {
		return nil, errStorage
	}
	return headers, nil
}
