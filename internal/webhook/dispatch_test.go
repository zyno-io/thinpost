package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

func TestBounceEventDoesNotClaimSMTPAttempt(t *testing.T) {
	event := Event{ID: "event", DeliveryID: "delivery", BounceReceiptID: "receipt", Outcome: "bounce_received_unverified"}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["outcome"] != "bounce_received_unverified" || decoded["bounce_receipt_id"] != "receipt" || decoded["smtp_code"] != float64(0) {
		t.Fatal("incorrect unverified bounce payload", decoded)
	}
	if _, exists := decoded["attempt_id"]; exists {
		t.Fatal("bounce payload invented an SMTP attempt")
	}
}

func TestPreparationFailureEventDoesNotInventDelivery(t *testing.T) {
	event := Event{ID: "event", MessageID: "message", Outcome: "preparation_failed"}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["outcome"] != "preparation_failed" || decoded["message_id"] != "message" || decoded["smtp_code"] != float64(0) {
		t.Fatal("incorrect preparation failure payload", decoded)
	}
	for _, key := range []string{"delivery_id", "attempt_id", "bounce_receipt_id"} {
		if _, exists := decoded[key]; exists {
			t.Fatal("preparation failure invented " + key)
		}
	}
}

type memoryRepo struct {
	claim    Claim
	claimErr error
	status   int
	commits  int
}

func (r *memoryRepo) ClaimWebhook(_ context.Context, token string, _ time.Duration) (Claim, error) {
	r.claim.Token = token
	return r.claim, r.claimErr
}
func (r *memoryRepo) CommitWebhook(_ context.Context, _ Claim, status int) error {
	r.status = status
	r.commits++
	return nil
}

type fakeDelivery struct {
	payload  []byte
	appEvent string
	status   int
	auth     *admin.WebhookAuth
}

func (f *fakeDelivery) Send(_ context.Context, _ string, request format.Request, auth *admin.WebhookAuth) (Result, error) {
	f.payload = append([]byte(nil), request.Body...)
	f.appEvent = request.Header.Get("X-Thinpost-Event-ID")
	f.auth = auth
	return Result{StatusCode: f.status, Success: f.status >= 200 && f.status < 300}, nil
}

type fakeKeys struct {
	fail bool
	aad  []byte
	open []byte
	// opened records every AAD in order; plain maps an AAD to its plaintext.
	opened []string
	plain  map[string][]byte
}

func (k *fakeKeys) Open(aad, envelope []byte) ([]byte, error) {
	k.aad, k.open = aad, envelope
	k.opened = append(k.opened, string(aad))
	if k.fail {
		return nil, errors.New("cannot open")
	}
	if plain, ok := k.plain[string(aad)]; ok {
		return plain, nil
	}
	return []byte("0123456789abcdef0123456789abcdef"), nil
}

func TestDispatcherOpensSealedSecretBoundToEndpoint(t *testing.T) {
	repo := &memoryRepo{claim: Claim{AppID: "app-a", SubscriptionID: "hook-a", Endpoint: "https://hooks.example/", SealedSecret: []byte("sealed"), Event: Event{ID: "event-1", MessageID: "message-1", DeliveryID: "delivery-1", AttemptID: "attempt-1", Outcome: "accepted", SMTPCode: 250, OccurredAt: time.Now()}}}
	deliver := &fakeDelivery{status: 204}
	keys := &fakeKeys{}
	dispatcher := &Dispatcher{Repository: repo, Sender: deliver, Keys: keys}
	if err := dispatcher.DispatchNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.commits != 1 || repo.status != 204 || deliver.appEvent != "event-1" || string(keys.aad) != string(SecretAAD("app-a", "hook-a")) || string(keys.open) != "sealed" {
		t.Fatal("incorrect endpoint-scoped dispatch")
	}
	if strings.Contains(string(deliver.payload), "sealed") || strings.Contains(string(deliver.payload), "https://") {
		t.Fatal("configuration leaked into event payload")
	}
}
func TestOpenFailureDoesNotSendAndCommitsStatusZero(t *testing.T) {
	repo := &memoryRepo{claim: Claim{AppID: "app", SubscriptionID: "hook", SealedSecret: []byte("sealed"), Event: Event{ID: "event"}}}
	deliver := &fakeDelivery{status: 204}
	dispatcher := &Dispatcher{Repository: repo, Sender: deliver, Keys: &fakeKeys{fail: true}}
	if err := dispatcher.DispatchNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deliver.payload != nil || repo.commits != 1 || repo.status != 0 {
		t.Fatal("unreadable secret was sent or not recorded")
	}
}

func TestBuildFailureDoesNotSendAndCommitsStatusZero(t *testing.T) {
	repo := &memoryRepo{claim: Claim{AppID: "app", SubscriptionID: "hook", SealedSecret: []byte("sealed"), Event: Event{ID: "not an opaque id"}}}
	deliver := &fakeDelivery{status: 204}
	dispatcher := &Dispatcher{Repository: repo, Sender: deliver, Keys: &fakeKeys{}}
	if err := dispatcher.DispatchNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deliver.payload != nil || repo.commits != 1 || repo.status != 0 || deliver.auth != nil {
		t.Fatal("unbuildable event was sent or not recorded")
	}
}

func providerClaim(name string) Claim {
	return Claim{AppID: "app-a", SubscriptionID: "hook-a", Endpoint: "https://hooks.example/", Format: name, SealedSecret: []byte("sealed-secret"),
		Context: format.Context{AcceptedAt: time.Now(), Subject: "Hello", SenderDomain: "sender.example", Recipients: []string{"to@example.net"}},
		Event:   Event{ID: "event-1", MessageID: "0123456789abcdef0123456789abcdef", DeliveryID: "d", AttemptID: "a", Outcome: "accepted", SMTPCode: 250, OccurredAt: time.Now(), DeliveryState: "accepted"}}
}

func TestDispatcherBuildsEachFormatAndOpensSecretAndAuthWithTheirOwnAAD(t *testing.T) {
	auth := admin.NewWebhookAuth("user", "pass", []admin.WebhookHeader{{Name: "X-Token", Value: "v"}})
	plainAuth, _ := auth.Seal()
	secrets := map[string][]byte{format.Resend: []byte("whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYX"), format.Mailgun: []byte(strings.Repeat("ab", 32)), format.Thinpost: []byte("0123456789abcdef0123456789abcdef"), format.Postmark: nil}
	for name, secret := range secrets {
		claim := providerClaim(name)
		claim.SealedAuth = []byte("sealed-auth")
		keys := &fakeKeys{plain: map[string][]byte{string(SecretAAD("app-a", "hook-a")): secret, string(AuthAAD("app-a", "hook-a")): plainAuth}}
		if secret == nil {
			claim.SealedSecret = nil
		}
		repo := &memoryRepo{claim: claim}
		deliver := &fakeDelivery{status: 204}
		if err := (&Dispatcher{Repository: repo, Sender: deliver, Keys: keys}).DispatchNext(context.Background()); err != nil {
			t.Fatal(name, err)
		}
		want := []string{string(SecretAAD("app-a", "hook-a")), string(AuthAAD("app-a", "hook-a"))}
		if secret == nil {
			want = want[1:]
		}
		if repo.status != 204 || deliver.payload == nil || strings.Join(keys.opened, "|") != strings.Join(want, "|") || deliver.auth == nil || deliver.auth.Username() != "user" {
			t.Fatal(name, "dispatch", repo.status, keys.opened)
		}
		if name != format.Thinpost && !strings.Contains(string(deliver.payload), "to@example.net") && name != format.Mailgun {
			t.Fatal(name, "payload lacks the recipient", string(deliver.payload))
		}
	}
}

func TestDispatcherFailuresCommitZeroAndLogOnlyTheEndpointID(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	cases := map[string]func(*Claim, *fakeKeys){
		"unknown format": func(c *Claim, _ *fakeKeys) { c.Format = "sendgrid" },
		"bad auth open": func(c *Claim, k *fakeKeys) {
			c.SealedAuth = []byte("x")
			k.plain = map[string][]byte{string(AuthAAD("app-a", "hook-a")): []byte("not json pass-secret")}
		},
		"short resend key": func(c *Claim, k *fakeKeys) {
			c.Format = format.Resend
			k.plain = map[string][]byte{string(SecretAAD("app-a", "hook-a")): []byte("whsec_c2VjcmV0")}
		},
		"unrepresentable": func(c *Claim, _ *fakeKeys) {
			c.Format = format.Postmark
			c.SealedSecret = nil
			c.Event.Outcome = "uncertain"
		},
	}
	for name, mutate := range cases {
		claim := providerClaim(format.Mailgun)
		keys := &fakeKeys{}
		mutate(&claim, keys)
		repo, deliver := &memoryRepo{claim: claim}, &fakeDelivery{status: 204}
		if err := (&Dispatcher{Repository: repo, Sender: deliver, Keys: keys}).DispatchNext(context.Background()); err != nil {
			t.Fatal(name, err)
		}
		if deliver.payload != nil || repo.commits != 1 || repo.status != 0 {
			t.Fatal(name, "sent or not recorded")
		}
	}
	if out := logs.String(); !strings.Contains(out, "hook-a") || strings.Contains(out, "secret") || strings.Contains(out, "pass") || strings.Contains(out, "example") {
		t.Fatal("log must name only the endpoint ID:", out)
	}
}

func TestSkippedClaimIsNotAnError(t *testing.T) {
	repo := &memoryRepo{claimErr: ErrSkipped}
	deliver := &fakeDelivery{status: 204}
	if err := (&Dispatcher{Repository: repo, Sender: deliver, Keys: &fakeKeys{}}).DispatchNext(context.Background()); err != nil || repo.commits != 0 || deliver.payload != nil {
		t.Fatal("skip must return nil without committing or sending", err)
	}
}
