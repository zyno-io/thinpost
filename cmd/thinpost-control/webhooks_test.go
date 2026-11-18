package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/webhook"
)

func TestWebhookRuntimeNeedsKeyring(t *testing.T) {
	startWebhooks(context.Background(), nil, nil, nil, nil)()
}

type idleKeys struct{}

func (idleKeys) Open(_, _ []byte) ([]byte, error) { return nil, nil }

type idleRepository struct{ claims atomic.Int32 }

func (r *idleRepository) ClaimWebhook(context.Context, string, time.Duration) (webhook.Claim, error) {
	r.claims.Add(1)
	return webhook.Claim{}, webhook.ErrNoWork
}
func (r *idleRepository) CommitWebhook(context.Context, webhook.Claim, int) error { return nil }

func TestWebhookWorkersStartAndStopCleanly(t *testing.T) {
	repository := &idleRepository{}
	stop := runWebhooks(context.Background(), repository, idleKeys{}, nil)
	deadline := time.Now().Add(2 * time.Second)
	for repository.claims.Load() < webhookWorkers && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := repository.claims.Load(); got < webhookWorkers {
		t.Fatalf("only %d workers claimed", got)
	}
	stop()
	stop()
}
