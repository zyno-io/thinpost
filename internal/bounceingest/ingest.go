// Package bounceingest stores untrusted inbound messages without treating them
// as authenticated delivery failures. It runs only in the trusted plane.
package bounceingest

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/objectkey"
)

const MaxBytes = 16 << 20

type Record struct {
	ID, ObjectKey          string
	TokenHash, Fingerprint [32]byte
}
type Status uint8

const (
	Unknown Status = iota
	New
	Duplicate
	Full
)

type Repository interface {
	CheckBounce(context.Context, [32]byte, [32]byte) (Status, error)
	CommitBounce(context.Context, Record) error
}
type Objects interface {
	PutMIME(context.Context, string, []byte) error
}

// FeedbackHandler receives mail addressed to the global feedback mailbox
// (spam-complaint reports), which is not a delivery's bounce address.
type FeedbackHandler interface {
	Mailbox(ctx context.Context, tokenHash [32]byte) (bool, error)
	Process(ctx context.Context, body []byte) error
}
type Service struct {
	Repository Repository
	Objects    Objects
	// Feedback is optional. When set, a token that hashes to the feedback
	// mailbox goes to it; any other token takes the bounce path below.
	Feedback FeedbackHandler
}

// Forward succeeds only after durable object storage and database commit. The
// request ID comes from edge; a fresh object ID is generated in trusted to make
// retries safe when an acknowledgement was lost. Duplicate content is recognized
// per token before upload. Concurrent duplicates may still leave unreferenced
// objects, which require orphan collection.
func (s *Service) Forward(ctx context.Context, token, requestID string, body []byte) error {
	if s.Repository == nil || s.Objects == nil || len(body) == 0 || len(body) > MaxBytes || len(token) != 32 || token != strings.ToLower(token) || len(requestID) != 32 || requestID != strings.ToLower(requestID) {
		return errors.New("invalid bounce")
	}
	secret, err := hex.DecodeString(token)
	if err != nil || len(secret) != 16 {
		return errors.New("invalid bounce")
	}
	if _, err = hex.DecodeString(requestID); err != nil {
		return errors.New("invalid bounce")
	}
	tokenHash := sha256.Sum256([]byte(token))
	if s.Feedback != nil {
		isMailbox, err := s.Feedback.Mailbox(ctx, tokenHash)
		if err != nil {
			return err
		}
		if isMailbox {
			return s.Feedback.Process(ctx, body)
		}
	}
	// This keyed fingerprint identifies exact retries without storing a global
	// content hash or revealing the raw bounce token in the database.
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("thinpost-bounce-body-v1\x00"))
	_, _ = mac.Write(body)
	var fingerprint [32]byte
	copy(fingerprint[:], mac.Sum(nil))
	state, err := s.Repository.CheckBounce(ctx, tokenHash, fingerprint)
	if err != nil {
		return err
	}
	if state == Duplicate {
		return nil
	}
	if state != New {
		return errors.New("unknown bounce recipient")
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return err
	}
	objectID := hex.EncodeToString(id[:])
	objectKey, err := objectkey.New("bounces", objectID, time.Now())
	if err != nil {
		return err
	}
	if err = s.Objects.PutMIME(ctx, objectKey, body); err != nil {
		return err
	}
	return s.Repository.CommitBounce(ctx, Record{ID: objectID, ObjectKey: objectKey, TokenHash: tokenHash, Fingerprint: fingerprint})
}
