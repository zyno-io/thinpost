package acceptance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/submission"
)

func TestHoldIsRecordedFingerprintedAndLogged(t *testing.T) {
	var lines []string
	defer oplog.SetOutput(func(format string, args ...any) {
		lines = append(lines, strings.TrimSpace(format))
	})()
	repo := &memoryRepository{}
	service := testService(t, repo, &memoryObjects{})
	app := submission.App{ID: "app-a"}
	req := validRequest()
	if _, err := service.Accept(context.Background(), app, req); err != nil {
		t.Fatal(err)
	}
	// Without a hold the fingerprint input is byte-identical to the one used
	// before holds existed, so idempotency markers survive the upgrade.
	legacy := `{"app_id":"app-a","adapter":"resend","message":{"From":"a@example.com","To":["b@example.net"],"Subject":"hello","Text":"body","HTML":"","EnhancedProtection":true},"sending_domain":""}`
	mac := hmac.New(sha256.New, []byte("0123456789abcdef0123456789abcdef"))
	_, _ = mac.Write([]byte(legacy))
	if !hmac.Equal(mac.Sum(nil), repo.records[0].RequestHash[:]) || repo.records[0].Hold {
		t.Fatal("fingerprint without a hold changed")
	}
	// The same key with a different hold choice is a different request.
	held := req
	held.Hold = true
	if _, err := service.Accept(context.Background(), app, held); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("hold did not change the fingerprint", err)
	}
	held.IdempotencyKey = "held-key"
	receipt, err := service.Accept(context.Background(), app, held)
	if err != nil || len(repo.records) != 2 || !repo.records[1].Hold {
		t.Fatal("hold not recorded", err)
	}
	if replay, err := service.Accept(context.Background(), app, held); err != nil || !replay.Replayed || replay.ID != receipt.ID {
		t.Fatal("held retry did not replay", err)
	}
	// One accepted line per new message, plus a held line for the held one;
	// a replay logs nothing.
	if len(lines) != 3 {
		t.Fatalf("lifecycle lines %v", lines)
	}
}

func TestBatchHoldIsRecordedAndFingerprinted(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	service := testService(t, repo, &memoryObjects{})
	app := submission.App{ID: "app-a"}
	req := batchRequest()
	if _, err := service.AcceptBatch(context.Background(), app, req); err != nil {
		t.Fatal(err)
	}
	plain := repo.last
	req.Hold = true
	if _, err := service.AcceptBatch(context.Background(), app, req); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("batch hold did not change the fingerprint", err)
	}
	req.IdempotencyKey = "held-batch"
	if _, err := service.AcceptBatch(context.Background(), app, req); err != nil {
		t.Fatal(err)
	}
	if plain.Hold || !repo.last.Hold || plain.RequestHash == repo.last.RequestHash {
		t.Fatal("batch hold not recorded or not fingerprinted")
	}
}
