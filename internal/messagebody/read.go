// Package messagebody returns authorized and audited message contents: the
// original source body and attachments, and the header fields of the signed
// per-recipient MIME object. Authorization (and, for Enhanced Protection
// messages, the audit row) always commits before any object is fetched.
package messagebody

import (
	"context"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/prepared"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

const (
	maxSourceSize = 16 << 20
	// maxFinalSize covers a full object (at most edgeprotocol.MaxMIMEBytes) and a
	// recipe object (at most MaxMIMEBytes plus prepared.MaxOverhead).
	maxFinalSize = int64(edgeprotocol.MaxMIMEBytes + prepared.MaxOverhead)
)

// Repository authorizes a content read and returns the object reference and
// the message's Enhanced Protection flag. Implementations audit Enhanced
// Protection reads before returning.
type Repository interface {
	AuthorizeBodyRead(ctx context.Context, actor, appID, messageID string) (string, bool, error)
	// AuthorizeRawRead authorizes a download of the original message of a
	// version 3 source; Enhanced Protection reads are audited as message.raw.read.
	AuthorizeRawRead(ctx context.Context, actor, appID, messageID string) (string, bool, error)
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
func (s *Service) readSource(ctx context.Context, key string, protected bool) (submission.Source, error) {
	data, err := s.Objects.Get(ctx, key, maxSourceSize)
	if err != nil {
		return submission.Source{}, errStorage
	}
	source, err := submission.DecodeSource(data, protected)
	if err != nil {
		return submission.Source{}, errSource
	}
	return source, nil
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
	source, err := s.readSource(ctx, key, protected)
	if err != nil {
		return admin.MessageBody{}, err
	}
	if source.Version == submission.RawSourceVersion {
		return rawBody(source), nil
	}
	message := source.Message
	// Always a non-nil slice: legacy sources without attachments report [].
	attachments := make([]admin.AttachmentMetadata, len(message.Attachments))
	for i, attachment := range message.Attachments {
		attachments[i] = admin.AttachmentMetadata{Index: i, Filename: attachment.Filename, ContentType: attachment.ContentType, Size: len(attachment.Content), Inline: attachment.Inline, ContentID: attachment.ContentID}
	}
	return admin.MessageBody{From: message.From, To: message.To, Cc: message.Cc, Bcc: message.Bcc, ReplyTo: message.ReplyTo, Subject: message.Subject, Text: message.Text, HTML: message.HTML, EnhancedProtection: message.EnhancedProtection, Attachments: attachments}, nil
}

// rawBody builds the view of a version 3 source. From is the effective From;
// To, Cc and Reply-To come from the message's own headers through the shared
// inbound parser, and Bcc from the envelope kinds. Attachments are never
// listed: the original is downloaded whole.
func rawBody(source submission.Source) admin.MessageBody {
	view := format.ViewMessage(source.Raw)
	from := source.RawHeader.From
	if from == "" && len(view.From) > 0 {
		from = view.From[0]
	}
	var bcc []string
	for i, rcpt := range source.Envelope() {
		if source.RecipientKind(i) == submission.RecipientBcc {
			bcc = append(bcc, rcpt)
		}
	}
	return admin.MessageBody{
		From: from, To: view.To, Cc: view.Cc, Bcc: bcc, ReplyTo: view.ReplyTo, Subject: view.Subject, Text: view.Text, HTML: view.HTML,
		EnhancedProtection: source.RawHeader.EnhancedProtection, Attachments: []admin.AttachmentMetadata{}, Raw: true, Omitted: view.Omitted,
	}
}

// OpenMessageRaw returns the original message bytes of a version 3 source, as
// received. Any other version is not found. The raw read is audited before the
// object is fetched when the message uses Enhanced Protection.
func (s *Service) OpenMessageRaw(ctx context.Context, actor, appID, messageID string) ([]byte, error) {
	if !s.ready() {
		return nil, errStorage
	}
	key, protected, err := s.Repository.AuthorizeRawRead(ctx, actor, appID, messageID)
	if err != nil {
		return nil, err
	}
	source, err := s.readSource(ctx, key, protected)
	if err != nil {
		return nil, err
	}
	if source.Version != submission.RawSourceVersion {
		return nil, admin.ErrNotFound
	}
	return source.Raw, nil
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
	source, err := s.readSource(ctx, key, protected)
	if err != nil {
		return admin.AttachmentContent{}, err
	}
	// A raw message has no stored attachments: they are parts of the original.
	if source.Version == submission.RawSourceVersion {
		return admin.AttachmentContent{}, admin.ErrNotFound
	}
	message := source.Message
	if index >= len(message.Attachments) {
		return admin.AttachmentContent{}, admin.ErrNotFound
	}
	attachment := message.Attachments[index]
	return admin.AttachmentContent{Filename: attachment.Filename, ContentType: attachment.ContentType, Content: attachment.Content}, nil
}

// ReadDeliveryHeaders returns the header fields of the signed per-recipient
// message in stored order. Only the header block is parsed and returned. A
// delivery's object is either the complete message or a recipe, which holds the
// exact header block; both give the same fields.
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
	if prepared.IsRecipe(data) {
		recipe, decodeErr := prepared.Decode(data)
		if decodeErr != nil || recipe.DeliveryID() != deliveryID {
			return nil, errStorage
		}
		data = recipe.Header()
	}
	headers, err := ParseHeaders(data)
	if err != nil {
		return nil, errStorage
	}
	return headers, nil
}
