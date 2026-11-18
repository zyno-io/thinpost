package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

// Store is the repository surface the admin service needs. It sees only
// sealed secrets and authentication, never plaintext.
type Store interface {
	ListWebhooks(ctx context.Context, appID string) ([]admin.Webhook, error)
	InsertWebhook(ctx context.Context, actor, appID, id string, in admin.WebhookInput, sealedSecret []byte, auth *admin.SealedWebhookAuth) (admin.Webhook, error)
	UpdateWebhook(ctx context.Context, actor, appID, id string, p admin.WebhookPatch, auth *admin.SealedWebhookAuth) (admin.Webhook, error)
	DeleteWebhook(ctx context.Context, actor, appID, id string) error
	ReplaceWebhookSecret(ctx context.Context, actor, appID, id, formatName string, sealed []byte) (admin.Webhook, error)
	WebhookFormat(ctx context.Context, appID, id string) (string, error)
	WebhookTarget(ctx context.Context, actor, appID, id string) (admin.WebhookTarget, error)
}

// Sealer encrypts a signing secret bound to its owning row.
type Sealer interface {
	Seal(aad, plaintext []byte) ([]byte, error)
	Open(aad, envelope []byte) ([]byte, error)
}

// Service implements admin.WebhookAdmin. Secrets appear only in IssuedWebhook;
// they, endpoints and payloads are never logged or placed in errors.
type Service struct {
	Store  Store
	Keys   Sealer
	Sender Deliverer
	Now    func() time.Time
}

var _ admin.WebhookAdmin = (*Service)(nil)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func newID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("cannot generate webhook id")
	}
	return hex.EncodeToString(raw[:]), nil
}

// newSecret issues a secret in the format's shape and seals it under SecretAAD.
func (s *Service) newSecret(impl format.Format, appID, id string) (secret string, sealed []byte, err error) {
	if secret, err = impl.NewSecret(); err != nil {
		return "", nil, err
	}
	if sealed, err = s.Keys.Seal(SecretAAD(appID, id), []byte(secret)); err != nil {
		return "", nil, errors.New("cannot seal webhook secret")
	}
	return secret, sealed, nil
}

// sealAuth seals authentication under AuthAAD. A zero value yields an empty
// ciphertext, which clears the stored authentication.
func (s *Service) sealAuth(appID, id string, auth admin.WebhookAuth) (*admin.SealedWebhookAuth, error) {
	if auth.IsZero() {
		return &admin.SealedWebhookAuth{}, nil
	}
	if !admin.ValidWebhookAuth(auth) {
		return nil, admin.ErrInvalidInput
	}
	plain, err := auth.Seal()
	if err != nil {
		return nil, errors.New("cannot seal webhook authentication")
	}
	sealed, err := s.Keys.Seal(AuthAAD(appID, id), plain)
	if err != nil {
		return nil, errors.New("cannot seal webhook authentication")
	}
	return &admin.SealedWebhookAuth{Ciphertext: sealed, Summary: auth.Summary()}, nil
}

func (s *Service) ListWebhooks(ctx context.Context, appID string) ([]admin.Webhook, error) {
	return s.Store.ListWebhooks(ctx, appID)
}

func (s *Service) CreateWebhook(ctx context.Context, actor, appID string, in admin.WebhookInput) (admin.IssuedWebhook, error) {
	if in.Format == "" {
		in.Format = format.Thinpost
	}
	impl, ok := format.Lookup(in.Format)
	if !ok {
		return admin.IssuedWebhook{}, admin.ErrInvalidInput
	}
	id, err := newID()
	if err != nil {
		return admin.IssuedWebhook{}, err
	}
	var secret string
	var sealed []byte
	if !format.Unsigned(in.Format) {
		if secret, sealed, err = s.newSecret(impl, appID, id); err != nil {
			return admin.IssuedWebhook{}, err
		}
	}
	var auth *admin.SealedWebhookAuth
	if in.Auth != nil && !in.Auth.IsZero() {
		if auth, err = s.sealAuth(appID, id, *in.Auth); err != nil {
			return admin.IssuedWebhook{}, err
		}
	}
	in.Auth = nil
	item, err := s.Store.InsertWebhook(ctx, actor, appID, id, in, sealed, auth)
	if err != nil {
		return admin.IssuedWebhook{}, err
	}
	return admin.IssuedWebhook{Webhook: item, Secret: secret}, nil
}

func (s *Service) UpdateWebhook(ctx context.Context, actor, appID, id string, patch admin.WebhookPatch) (admin.Webhook, error) {
	var auth *admin.SealedWebhookAuth
	if patch.Auth != nil {
		var err error
		if auth, err = s.sealAuth(appID, id, *patch.Auth); err != nil {
			return admin.Webhook{}, err
		}
	}
	patch.Auth = nil
	return s.Store.UpdateWebhook(ctx, actor, appID, id, patch, auth)
}

func (s *Service) DeleteWebhook(ctx context.Context, actor, appID, id string) error {
	return s.Store.DeleteWebhook(ctx, actor, appID, id)
}

// RotateWebhookSecret refuses unsigned formats with ErrConflict. The format is
// immutable, so reading it first cannot race with a change.
func (s *Service) RotateWebhookSecret(ctx context.Context, actor, appID, id string) (admin.IssuedWebhook, error) {
	name, err := s.Store.WebhookFormat(ctx, appID, id)
	if err != nil {
		return admin.IssuedWebhook{}, err
	}
	impl, ok := format.Lookup(name)
	if !ok || format.Unsigned(name) {
		return admin.IssuedWebhook{}, admin.ErrConflict
	}
	secret, sealed, err := s.newSecret(impl, appID, id)
	if err != nil {
		return admin.IssuedWebhook{}, err
	}
	item, err := s.Store.ReplaceWebhookSecret(ctx, actor, appID, id, name, sealed)
	if err != nil {
		return admin.IssuedWebhook{}, err
	}
	return admin.IssuedWebhook{Webhook: item, Secret: secret}, nil
}

// TestWebhook sends one test event in the endpoint's format outside any
// transaction. Nothing is queued, retried or added to delivery history.
// Provider formats render their primary event with all-zero IDs and a
// reserved recipient, marked with X-Thinpost-Test.
func (s *Service) TestWebhook(ctx context.Context, actor, appID, id string) (admin.WebhookTestResult, error) {
	target, err := s.Store.WebhookTarget(ctx, actor, appID, id)
	if err != nil {
		return admin.WebhookTestResult{}, err
	}
	impl, ok := format.Lookup(target.Format)
	if !ok {
		return admin.WebhookTestResult{}, errors.New("webhook test send failed")
	}
	if internalBlocked(s.Sender, target.Endpoint, target.Format, target.SealedAuth != nil) {
		return admin.WebhookTestResult{Failure: FailureBlocked}, nil
	}
	var secret []byte
	if target.SealedSecret != nil {
		if secret, err = s.Keys.Open(SecretAAD(appID, id), target.SealedSecret); err != nil {
			return admin.WebhookTestResult{}, errors.New("webhook secret unavailable")
		}
	}
	var auth *admin.WebhookAuth
	if target.SealedAuth != nil {
		plain, openErr := s.Keys.Open(AuthAAD(appID, id), target.SealedAuth)
		if openErr != nil {
			return admin.WebhookTestResult{}, errors.New("webhook authentication unavailable")
		}
		opened, parseErr := admin.OpenWebhookAuth(plain)
		if parseErr != nil {
			return admin.WebhookTestResult{}, errors.New("webhook authentication unavailable")
		}
		auth = &opened
	}
	eventID, err := newID()
	if err != nil {
		return admin.WebhookTestResult{}, err
	}
	now := s.now().UTC()
	event, hookContext := format.TestEvent(eventID, now)
	request, err := impl.Build(format.Input{Event: event, Context: hookContext, AppID: appID, Secret: secret, Now: now, Test: true})
	if err != nil {
		return admin.WebhookTestResult{}, errors.New("webhook test send failed")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	result, err := s.Sender.Send(ctx, target.Endpoint, request, auth)
	if err != nil {
		return admin.WebhookTestResult{}, errors.New("webhook test send failed")
	}
	return admin.WebhookTestResult{Success: result.Success, HTTPStatus: result.StatusCode, Failure: result.Failure}, nil
}
