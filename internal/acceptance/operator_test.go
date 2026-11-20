package acceptance

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

// operatorRepository adds the optional operator capability to the memory
// repository and records what the operator path used.
type operatorRepository struct {
	*memoryRepository
	app            submission.App
	lookupErr      error
	operatorErr    error
	lookups        []string
	operatorCharge []int
	// operatorCharged records the charge flag of each reservation.
	operatorCharged []bool
}

func (r *operatorRepository) LookupOperatorApp(_ context.Context, appID string) (submission.App, error) {
	r.lookups = append(r.lookups, appID)
	if r.lookupErr != nil {
		return submission.App{}, r.lookupErr
	}
	app := r.app
	app.ID = appID
	return app, nil
}

func (r *operatorRepository) ReserveOperatorSubmission(_ context.Context, _ string, messages int, charge bool) error {
	r.operatorCharge = append(r.operatorCharge, messages)
	r.operatorCharged = append(r.operatorCharged, charge)
	return r.operatorErr
}

func consoleRequest() submission.Request {
	return submission.Request{Adapter: submission.AdapterConsole, Message: submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "hello", Text: "body"}}
}

func TestAuthorizeOperatorSetsOnlyTheOperator(t *testing.T) {
	repo := &operatorRepository{memoryRepository: &memoryRepository{}, app: submission.App{ListUnsubscribe: true, DefaultTrackOpens: true, KeyID: "must-not-leak"}}
	service := testService(t, repo, &memoryObjects{})
	app, err := service.AuthorizeOperator(context.Background(), "app-a", "subject-1")
	if err != nil {
		t.Fatal(err)
	}
	if app.ID != "app-a" || app.Operator != "subject-1" || app.KeyID != "" || !app.ListUnsubscribe || !app.DefaultTrackOpens {
		t.Fatalf("unexpected principal: %#v", app)
	}
	for name, actor := range map[string]string{"empty": "", "padded": " a", "control": "a\x00b", "newline": "a\nb", "too long": strings.Repeat("a", 257), "invalid UTF-8": "a\xff"} {
		if _, err := service.AuthorizeOperator(context.Background(), "app-a", actor); !errors.Is(err, submission.ErrInvalidRequest) {
			t.Fatalf("%s: accepted an invalid actor: %v", name, err)
		}
	}
	if _, err := service.AuthorizeOperator(context.Background(), "", "subject-1"); !errors.Is(err, submission.ErrInvalidRequest) {
		t.Fatal("accepted an empty app ID")
	}
	repo.lookupErr = submission.ErrAppPaused
	if _, err := service.AuthorizeOperator(context.Background(), "app-a", "subject-1"); !errors.Is(err, submission.ErrAppPaused) {
		t.Fatalf("paused app: %v", err)
	}
}

func TestAuthorizeOperatorNeedsRepositoryCapability(t *testing.T) {
	service := testService(t, &memoryRepository{}, &memoryObjects{})
	if _, err := service.AuthorizeOperator(context.Background(), "app-a", "subject-1"); !errors.Is(err, submission.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	// Accept refuses an operator principal the repository cannot serve.
	if _, err := service.Accept(context.Background(), submission.App{ID: "app-a", Operator: "subject-1"}, consoleRequest()); !errors.Is(err, submission.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestOperatorSubmissionUsesTheOperatorReservationAndRecord(t *testing.T) {
	repo := &operatorRepository{memoryRepository: &memoryRepository{}}
	service := testService(t, repo, &memoryObjects{})
	receipt, err := service.Accept(context.Background(), submission.App{ID: "app-a", Operator: "subject-1"}, consoleRequest())
	if err != nil || receipt.ID == "" {
		t.Fatal(err)
	}
	if len(repo.memoryRepository.reservations) != 0 || len(repo.operatorCharge) != 1 || repo.operatorCharge[0] != 1 {
		t.Fatalf("wrong reservation path: key=%v operator=%v", repo.memoryRepository.reservations, repo.operatorCharge)
	}
	if len(repo.records) != 1 || repo.records[0].Operator != "subject-1" || repo.records[0].KeyID != "" || repo.records[0].Adapter != submission.AdapterConsole || repo.records[0].IdempotencyKey != "" {
		t.Fatalf("wrong record: %#v", repo.records)
	}
	// The same policy checks apply: a refused reservation stops the submission.
	repo.operatorErr = submission.ErrRateLimited
	if _, err := service.Accept(context.Background(), submission.App{ID: "app-a", Operator: "subject-1"}, consoleRequest()); !errors.Is(err, submission.ErrRateLimited) || len(repo.records) != 1 {
		t.Fatalf("reservation refusal ignored: %v", err)
	}
}

func TestOperatorPrincipalInvariants(t *testing.T) {
	repo := &operatorRepository{memoryRepository: &memoryRepository{}}
	service := testService(t, repo, &memoryObjects{})
	withKey := consoleRequest()
	withKey.IdempotencyKey = "key"
	otherAdapter := consoleRequest()
	otherAdapter.Adapter = "resend"
	native := consoleRequest()
	native.Adapter = submission.AdapterNative
	tests := []struct {
		name string
		app  submission.App
		req  submission.Request
	}{
		{"operator with idempotency key", submission.App{ID: "a", Operator: "s"}, withKey},
		{"operator with provider adapter", submission.App{ID: "a", Operator: "s"}, otherAdapter},
		{"operator with native adapter", submission.App{ID: "a", Operator: "s"}, native},
		{"key and operator together", submission.App{ID: "a", KeyID: "k", Operator: "s"}, consoleRequest()},
		{"invalid operator subject", submission.App{ID: "a", Operator: " s"}, consoleRequest()},
		{"console adapter with key", submission.App{ID: "a", KeyID: "k"}, consoleRequest()},
		{"console adapter without principal", submission.App{ID: "a"}, consoleRequest()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.Accept(context.Background(), tc.app, tc.req); !errors.Is(err, submission.ErrInvalidMessage) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if len(repo.records) != 0 || len(repo.operatorCharge) != 0 || len(repo.memoryRepository.reservations) != 0 {
		t.Fatal("a refused combination reserved or committed")
	}
}

func TestKeySubmissionPathIsUnchangedByOperatorSupport(t *testing.T) {
	repo := &operatorRepository{memoryRepository: &memoryRepository{}}
	service := testService(t, repo, &memoryObjects{})
	native := validRequest()
	native.Adapter = submission.AdapterNative
	if _, err := service.Accept(context.Background(), submission.App{ID: "app-a", KeyID: "key-1"}, native); err != nil {
		t.Fatal(err)
	}
	if len(repo.memoryRepository.reservations) != 1 || len(repo.operatorCharge) != 0 || repo.records[0].KeyID != "key-1" || repo.records[0].Operator != "" {
		t.Fatalf("key path changed: %#v", repo.records)
	}
}

func TestBatchRejectsOperator(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	service := testService(t, repo, &memoryObjects{})
	req := batchRequest()
	if _, err := service.AcceptBatch(context.Background(), submission.App{ID: "app", Operator: "subject-1"}, req); !errors.Is(err, submission.ErrInvalidMessage) || repo.calls != 0 {
		t.Fatalf("batch accepted an operator: %v", err)
	}
}

func TestOverridesAreRefusedForKeyPrincipalsAndNonConsoleAdapters(t *testing.T) {
	repo := &operatorRepository{memoryRepository: &memoryRepository{}}
	service := testService(t, repo, &memoryObjects{})
	for _, override := range []submission.Overrides{{Limits: true}, {RecipientPolicy: true}, {Limits: true, RecipientPolicy: true}} {
		for _, adapter := range []string{submission.AdapterNative, "resend", "postmark", "sendgrid", "mailgun"} {
			req := validRequest()
			req.Adapter = adapter
			req.Overrides = override
			if _, err := service.Accept(context.Background(), submission.App{ID: "app-a", KeyID: "key-1"}, req); !errors.Is(err, submission.ErrInvalidMessage) {
				t.Fatalf("%s %+v: key principal accepted an override: %v", adapter, override, err)
			}
		}
		// The console adapter is bound to the operator principal, so a key cannot reach it either.
		req := consoleRequest()
		req.Overrides = override
		if _, err := service.Accept(context.Background(), submission.App{ID: "app-a", KeyID: "key-1"}, req); !errors.Is(err, submission.ErrInvalidMessage) {
			t.Fatalf("console adapter with a key accepted an override: %v", err)
		}
		// An operator on a provider adapter is refused before any override matters.
		req = consoleRequest()
		req.Adapter = "resend"
		req.Overrides = override
		if _, err := service.Accept(context.Background(), submission.App{ID: "app-a", Operator: "subject-1"}, req); !errors.Is(err, submission.ErrInvalidMessage) {
			t.Fatalf("operator on a provider adapter accepted an override: %v", err)
		}
	}
	if len(repo.records) != 0 || len(repo.operatorCharge) != 0 || len(repo.memoryRepository.reservations) != 0 {
		t.Fatal("a refused override reserved or committed")
	}
}

func TestLimitsOverrideReservesWithoutCharge(t *testing.T) {
	repo := &operatorRepository{memoryRepository: &memoryRepository{}}
	service := testService(t, repo, &memoryObjects{})
	operator := submission.App{ID: "app-a", Operator: "subject-1"}
	if _, err := service.Accept(context.Background(), operator, consoleRequest()); err != nil {
		t.Fatal(err)
	}
	req := consoleRequest()
	req.Overrides = submission.Overrides{Limits: true}
	if _, err := service.Accept(context.Background(), operator, req); err != nil {
		t.Fatal(err)
	}
	if len(repo.operatorCharged) != 2 || !repo.operatorCharged[0] || repo.operatorCharged[1] {
		t.Fatalf("charge flags %v", repo.operatorCharged)
	}
	if repo.records[0].LimitsOverride || repo.records[0].RecipientPolicyOverride || !repo.records[1].LimitsOverride || repo.records[1].RecipientPolicyOverride {
		t.Fatalf("records carry wrong overrides: %#v", repo.records)
	}
	// A recipient-policy-only override still charges.
	req.Overrides = submission.Overrides{RecipientPolicy: true}
	if _, err := service.Accept(context.Background(), operator, req); err != nil || !repo.operatorCharged[2] || !repo.records[2].RecipientPolicyOverride || repo.records[2].LimitsOverride {
		t.Fatalf("recipient override changed charging: %v %v", err, repo.operatorCharged)
	}
}

func TestRecipientPolicyOverrideSkipsOnlyTheAllowlist(t *testing.T) {
	repo := &operatorRepository{memoryRepository: &memoryRepository{}}
	service, err := New(repo, &memoryObjects{}, []byte("0123456789abcdef0123456789abcdef"), allowRecipients(false))
	if err != nil {
		t.Fatal(err)
	}
	operator := submission.App{ID: "app-a", Operator: "subject-1"}
	if _, err = service.Accept(context.Background(), operator, consoleRequest()); !errors.Is(err, submission.ErrInvalidMessage) || len(repo.records) != 0 {
		t.Fatalf("allowlist did not apply to a console send: %v", err)
	}
	req := consoleRequest()
	req.Overrides = submission.Overrides{RecipientPolicy: true}
	if _, err = service.Accept(context.Background(), operator, req); err != nil || len(repo.records) != 1 || !repo.records[0].RecipientPolicyOverride {
		t.Fatalf("override did not skip the allowlist: %v", err)
	}
	// A limits override alone does not skip the allowlist.
	req.Overrides = submission.Overrides{Limits: true}
	if _, err = service.Accept(context.Background(), operator, req); !errors.Is(err, submission.ErrInvalidMessage) || len(repo.records) != 1 {
		t.Fatalf("limits override skipped the allowlist: %v", err)
	}
	// Everything else still applies with both overrides set: a reservation refusal stops the send.
	req.Overrides = submission.Overrides{Limits: true, RecipientPolicy: true}
	repo.operatorErr = submission.ErrAppPaused
	if _, err = service.Accept(context.Background(), operator, req); !errors.Is(err, submission.ErrAppPaused) || len(repo.records) != 1 {
		t.Fatalf("a paused app was bypassed: %v", err)
	}
}
