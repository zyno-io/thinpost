package acceptance

import (
	"context"
	"errors"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

// The SMTP principal has one entry point, AcceptRaw. Every HTTP-facing entry
// refuses it, and a provider adapter cannot claim the SMTP adapter.
func TestHTTPEntryPointsRefuseTheSMTPPrincipal(t *testing.T) {
	repo := &memoryRepository{}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	ctx := context.Background()

	smtpApp := submission.App{ID: "app-a", SMTPCredentialID: "0123456789abcdef0123456789abcdef"}
	if _, err := service.Accept(ctx, smtpApp, validRequest()); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("Accept took an SMTP principal", err)
	}
	for _, adapter := range []string{submission.AdapterSMTP, "smtp"} {
		req := validRequest()
		req.Adapter = adapter
		if _, err := service.Accept(ctx, submission.App{ID: "app-a", KeyID: "key"}, req); !errors.Is(err, submission.ErrInvalidMessage) {
			t.Fatal("Accept took the SMTP adapter", err)
		}
	}
	batchRepo := &memoryBatchRepository{memoryRepository: repo}
	batchService := testService(t, batchRepo, objects)
	if _, err := batchService.AcceptBatch(ctx, smtpApp, submission.BatchRequest{Adapter: "resend", Messages: []submission.Message{validRequest().Message}}); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("AcceptBatch took an SMTP principal", err)
	}
	if batchRepo.calls != 0 {
		t.Fatal("a batch of an SMTP principal reached the repository")
	}
	if len(repo.records) != 0 || len(objects.objects) != 0 || len(repo.reservations) != 0 {
		t.Fatal("a refused SMTP principal reserved, stored or committed something")
	}

	// A key principal is unchanged.
	if _, err := service.Accept(ctx, submission.App{ID: "app-a", KeyID: "key"}, validRequest()); err != nil {
		t.Fatal(err)
	}
	if len(repo.records) != 1 || repo.records[0].SMTPCredentialID != "" || repo.records[0].Raw || repo.records[0].IdempotencyTTL != 0 || repo.records[0].Fence != (submission.Fence{}) {
		t.Fatalf("a key submission picked up SMTP fields: %#v", repo.records)
	}
}
