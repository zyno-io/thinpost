package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

type sealKeys struct {
	sealAAD, openAAD []string
	sealErr, openErr error
}

func (k *sealKeys) Seal(aad, plaintext []byte) ([]byte, error) {
	k.sealAAD = append(k.sealAAD, string(aad))
	return append([]byte("sealed:"), plaintext...), k.sealErr
}
func (k *sealKeys) Open(aad, envelope []byte) ([]byte, error) {
	k.openAAD = append(k.openAAD, string(aad))
	return bytes.TrimPrefix(envelope, []byte("sealed:")), k.openErr
}

type svcStore struct {
	app, id, sealed, endpoint, format string
	target, targetAuth                []byte
	in                                admin.WebhookInput
	auth                              *admin.SealedWebhookAuth
	patch                             admin.WebhookPatch
	rotateFormat                      string
	err                               error
}

func (s *svcStore) ListWebhooks(context.Context, string) ([]admin.Webhook, error) { return nil, s.err }
func (s *svcStore) InsertWebhook(_ context.Context, _, appID, id string, in admin.WebhookInput, sealed []byte, auth *admin.SealedWebhookAuth) (admin.Webhook, error) {
	s.app, s.id, s.sealed, s.in, s.auth = appID, id, string(sealed), in, auth
	return admin.Webhook{ID: id, Format: in.Format, Endpoint: in.Endpoint, Active: true}, s.err
}
func (s *svcStore) UpdateWebhook(_ context.Context, _, _, _ string, p admin.WebhookPatch, auth *admin.SealedWebhookAuth) (admin.Webhook, error) {
	s.patch, s.auth = p, auth
	return admin.Webhook{}, s.err
}
func (s *svcStore) DeleteWebhook(context.Context, string, string, string) error { return s.err }
func (s *svcStore) ReplaceWebhookSecret(_ context.Context, _, appID, id, formatName string, sealed []byte) (admin.Webhook, error) {
	s.app, s.id, s.sealed, s.rotateFormat = appID, id, string(sealed), formatName
	return admin.Webhook{ID: id}, s.err
}
func (s *svcStore) WebhookFormat(context.Context, string, string) (string, error) {
	if s.format == "" {
		return format.Thinpost, s.err
	}
	return s.format, s.err
}
func (s *svcStore) WebhookTarget(context.Context, string, string, string) (admin.WebhookTarget, error) {
	name := s.format
	if name == "" {
		name = format.Thinpost
	}
	return admin.WebhookTarget{Endpoint: s.endpoint, Format: name, SealedSecret: s.target, SealedAuth: s.targetAuth}, s.err
}

type svcSender struct {
	endpoint, eventID string
	request           format.Request
	auth              *admin.WebhookAuth
	deadline          bool
	result            Result
	err               error
}

func (s *svcSender) Send(ctx context.Context, endpoint string, request format.Request, auth *admin.WebhookAuth) (Result, error) {
	s.endpoint, s.request, s.auth = endpoint, request, auth
	s.eventID = request.Header.Get("X-Thinpost-Event-ID")
	_, s.deadline = ctx.Deadline()
	return s.result, s.err
}

var testApp, testHook = strings.Repeat("a", 32), strings.Repeat("b", 32)

func TestServiceCreateAndRotateSealWithBoundAAD(t *testing.T) {
	keys, store := &sealKeys{}, &svcStore{}
	svc := &Service{Store: store, Keys: keys}
	created, err := svc.CreateWebhook(context.Background(), "op", testApp, admin.WebhookInput{Endpoint: "https://hooks.example.com"})
	if err != nil || len(created.Webhook.ID) != 32 || !strings.HasPrefix(created.Secret, SecretPrefix) {
		t.Fatal("create", err, created)
	}
	if created.Webhook.Format != format.Thinpost {
		t.Fatal("format must default to thinpost")
	}
	if want := string(SecretAAD(testApp, created.Webhook.ID)); len(keys.sealAAD) != 1 || keys.sealAAD[0] != want || store.id != created.Webhook.ID || store.app != testApp {
		t.Fatal("seal AAD not bound to app and generated id")
	}
	if store.sealed != "sealed:"+created.Secret {
		t.Fatal("stored value is not the sealed secret")
	}
	rotated, err := svc.RotateWebhookSecret(context.Background(), "op", testApp, testHook)
	if err != nil || rotated.Secret == created.Secret || store.id != testHook || store.app != testApp || store.rotateFormat != format.Thinpost || keys.sealAAD[1] != string(SecretAAD(testApp, testHook)) || store.sealed != "sealed:"+rotated.Secret {
		t.Fatal("rotate", err)
	}
}

func TestServiceErrorsCarryNoSecret(t *testing.T) {
	keys, store := &sealKeys{sealErr: errors.New("boom tpwh_x")}, &svcStore{}
	svc := &Service{Store: store, Keys: keys}
	if _, err := svc.CreateWebhook(context.Background(), "op", testApp, admin.WebhookInput{}); err == nil || strings.Contains(err.Error(), "tpwh_") || store.sealed != "" {
		t.Fatal("seal failure must not reach the store or leak", err)
	}
	keys.sealErr, store.err = nil, admin.ErrConflict
	if _, err := svc.CreateWebhook(context.Background(), "op", testApp, admin.WebhookInput{}); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("store error not passed through", err)
	}
	if _, err := svc.RotateWebhookSecret(context.Background(), "op", testApp, testHook); !errors.Is(err, admin.ErrConflict) {
		t.Fatal(err)
	}
}

func TestServiceTestSendMapsResult(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	secret := "tpwh_" + strings.Repeat("s", 43)
	for _, tc := range []struct {
		name   string
		result Result
		want   admin.WebhookTestResult
	}{
		{"success", Result{StatusCode: 204, Success: true}, admin.WebhookTestResult{Success: true, HTTPStatus: 204}},
		{"http failure", Result{StatusCode: 500}, admin.WebhookTestResult{HTTPStatus: 500}},
		{"transport failure", Result{Failure: FailureTLS}, admin.WebhookTestResult{Failure: FailureTLS}},
	} {
		keys := &sealKeys{}
		store := &svcStore{endpoint: "https://hooks.example.com/in", target: []byte("sealed:" + secret)}
		sender := &svcSender{result: tc.result}
		svc := &Service{Store: store, Keys: keys, Sender: sender, Now: func() time.Time { return now }}
		got, err := svc.TestWebhook(context.Background(), "op", testApp, testHook)
		if err != nil || got != tc.want {
			t.Fatal(tc.name, err, got)
		}
		if len(keys.openAAD) != 1 || keys.openAAD[0] != string(SecretAAD(testApp, testHook)) {
			t.Fatal("open AAD not bound")
		}
		header := sender.request.Header
		if !format.VerifyThinpost([]byte(secret), sender.eventID, header.Get("X-Thinpost-Timestamp"), sender.request.Body, header.Get("X-Thinpost-Signature"), now, time.Minute) || sender.endpoint != store.endpoint || !sender.deadline || sender.auth != nil || header.Get("X-Thinpost-Timestamp") != strconv.FormatInt(now.Unix(), 10) {
			t.Fatal("sender input", tc.name)
		}
		var body map[string]any
		if json.Unmarshal(sender.request.Body, &body) != nil || body["outcome"] != "test" || body["id"] != sender.eventID || len(sender.eventID) != 32 || body["occurred_at"] != "2026-11-05T12:00:00Z" || len(body) != 3 {
			t.Fatal("payload", string(sender.request.Body))
		}
	}
}

func TestServiceTestSendErrors(t *testing.T) {
	secret := "tpwh_" + strings.Repeat("s", 43)
	store := &svcStore{endpoint: "https://hooks.example.com/in", target: []byte("sealed:" + secret)}
	sender := &svcSender{}
	svc := &Service{Store: store, Keys: &sealKeys{}, Sender: sender}
	store.err = admin.ErrNotFound
	if _, err := svc.TestWebhook(context.Background(), "op", testApp, testHook); !errors.Is(err, admin.ErrNotFound) || sender.eventID != "" {
		t.Fatal("missing endpoint must not send", err)
	}
	store.err = nil
	svc.Keys = &sealKeys{openErr: errors.New("cannot open " + secret)}
	if _, err := svc.TestWebhook(context.Background(), "op", testApp, testHook); err == nil || strings.Contains(err.Error(), secret) || sender.eventID != "" {
		t.Fatal("open failure", err)
	}
	svc.Keys = &sealKeys{}
	sender.err = errors.New("bad https://hooks.example.com/in " + secret)
	if _, err := svc.TestWebhook(context.Background(), "op", testApp, testHook); err == nil || strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), secret) {
		t.Fatal("send error leaked", err)
	}
}

func TestServiceCreatesEveryFormatWithItsSecretShape(t *testing.T) {
	for name, check := range map[string]func(string) bool{
		format.Thinpost: func(s string) bool { return strings.HasPrefix(s, "tpwh_") },
		format.Resend:   func(s string) bool { return strings.HasPrefix(s, "whsec_") },
		format.Mailgun:  func(s string) bool { return len(s) == 64 },
		format.Postmark: func(s string) bool { return s == "" },

		format.ThinpostInbound: func(s string) bool { return strings.HasPrefix(s, "tpwh_") },
	} {
		keys, store := &sealKeys{}, &svcStore{}
		svc := &Service{Store: store, Keys: keys}
		created, err := svc.CreateWebhook(context.Background(), "op", testApp, admin.WebhookInput{Format: name, Endpoint: "https://hooks.example.com"})
		if err != nil || !check(created.Secret) || created.Webhook.Format != name {
			t.Fatal(name, err)
		}
		if name == format.Postmark && (store.sealed != "" || len(keys.sealAAD) != 0) {
			t.Fatal("postmark must store and seal no secret")
		}
	}
	svc := &Service{Store: &svcStore{}, Keys: &sealKeys{}}
	if _, err := svc.CreateWebhook(context.Background(), "op", testApp, admin.WebhookInput{Format: "sendgrid"}); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("unknown format accepted", err)
	}
}

func TestServiceSealsAuthUnderItsOwnAAD(t *testing.T) {
	keys, store := &sealKeys{}, &svcStore{}
	svc := &Service{Store: store, Keys: keys}
	auth := admin.NewWebhookAuth("user", "pass-secret", []admin.WebhookHeader{{Name: "X-Token", Value: "value-secret"}})
	created, err := svc.CreateWebhook(context.Background(), "op", testApp, admin.WebhookInput{Format: format.Postmark, Endpoint: "https://hooks.example.com", Auth: &auth})
	if err != nil || store.in.Auth != nil || store.auth == nil || len(store.auth.Ciphertext) == 0 {
		t.Fatal("auth not sealed for the store", err)
	}
	if len(keys.sealAAD) != 1 || keys.sealAAD[0] != string(AuthAAD(testApp, created.Webhook.ID)) || keys.sealAAD[0] == string(SecretAAD(testApp, created.Webhook.ID)) {
		t.Fatal("auth not sealed under AuthAAD", keys.sealAAD)
	}
	if store.auth.Summary.Username != "user" || len(store.auth.Summary.HeaderNames) != 1 || store.auth.Summary.HeaderNames[0] != "X-Token" || strings.Contains(fmt.Sprint(store.auth.Summary), "secret") {
		t.Fatal("summary", store.auth.Summary)
	}
	invalid := admin.NewWebhookAuth("user", "", nil)
	if _, err = svc.CreateWebhook(context.Background(), "op", testApp, admin.WebhookInput{Endpoint: "https://hooks.example.com", Auth: &invalid}); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("invalid auth sealed", err)
	}
	// Update: replace, remove, and leave unchanged.
	if _, err = svc.UpdateWebhook(context.Background(), "op", testApp, testHook, admin.WebhookPatch{Auth: &auth}); err != nil || store.auth == nil || len(store.auth.Ciphertext) == 0 || store.patch.Auth != nil {
		t.Fatal("replace", err)
	}
	empty := admin.WebhookAuth{}
	if _, err = svc.UpdateWebhook(context.Background(), "op", testApp, testHook, admin.WebhookPatch{Auth: &empty}); err != nil || store.auth == nil || len(store.auth.Ciphertext) != 0 {
		t.Fatal("remove", err)
	}
	active := true
	if _, err = svc.UpdateWebhook(context.Background(), "op", testApp, testHook, admin.WebhookPatch{Active: &active}); err != nil || store.auth != nil {
		t.Fatal("unchanged", err)
	}
}

func TestServiceRotateRefusesUnsignedFormat(t *testing.T) {
	keys, store := &sealKeys{}, &svcStore{format: format.Postmark}
	svc := &Service{Store: store, Keys: keys}
	if _, err := svc.RotateWebhookSecret(context.Background(), "op", testApp, testHook); !errors.Is(err, admin.ErrConflict) || len(keys.sealAAD) != 0 || store.sealed != "" {
		t.Fatal("rotating an unsigned endpoint", err)
	}
	store.format = format.Resend
	if r, err := svc.RotateWebhookSecret(context.Background(), "op", testApp, testHook); err != nil || !strings.HasPrefix(r.Secret, "whsec_") || store.rotateFormat != format.Resend {
		t.Fatal("rotate resend", err)
	}
}

func TestServiceTestSendBuildsEachFormat(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	secrets := map[string]string{format.Resend: "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYX", format.Mailgun: strings.Repeat("ab", 32), format.Postmark: "", format.ThinpostInbound: "tpwh_0123456789abcdef0123456789abcdef"}
	for name, secret := range secrets {
		keys := &sealKeys{}
		store := &svcStore{format: name, endpoint: "https://hooks.example.com/in"}
		if secret != "" {
			store.target = []byte("sealed:" + secret)
		}
		auth := admin.NewWebhookAuth("u", "p", nil)
		plain, _ := auth.Seal()
		store.targetAuth = append([]byte("sealed:"), plain...)
		sender := &svcSender{result: Result{StatusCode: 204, Success: true}}
		svc := &Service{Store: store, Keys: keys, Sender: sender, Now: func() time.Time { return now }}
		if got, err := svc.TestWebhook(context.Background(), "op", testApp, testHook); err != nil || !got.Success {
			t.Fatal(name, err, got)
		}
		if sender.request.Header.Get("X-Thinpost-Test") != "true" || sender.auth == nil || sender.auth.Username() != "u" {
			t.Fatal(name, "test header or auth missing")
		}
		wantAAD := []string{string(AuthAAD(testApp, testHook))}
		if secret != "" {
			wantAAD = []string{string(SecretAAD(testApp, testHook)), string(AuthAAD(testApp, testHook))}
		}
		if fmt.Sprint(keys.openAAD) != fmt.Sprint(wantAAD) {
			t.Fatal(name, "open AAD", keys.openAAD)
		}
		if !strings.Contains(string(sender.request.Body), "test@example.com") || !strings.Contains(string(sender.request.Body), "00000000") {
			t.Fatal(name, "test body", string(sender.request.Body))
		}
	}
}
