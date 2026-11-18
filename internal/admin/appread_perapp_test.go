package admin

import (
	"context"
	"testing"
	"time"
)

func TestAppReadPerAppCapLeavesBudgetForOtherApps(t *testing.T) {
	h := &AppReadHandler{readBudget: 10, readWeight: 1, waitTimeout: 50 * time.Millisecond}
	ctx := context.Background()
	var releases []func()
	for i := 0; i < readsPerApp; i++ {
		release, ok := h.acquire(ctx, "app-a")
		if !ok {
			t.Fatalf("read %d of app-a refused", i)
		}
		releases = append(releases, release)
	}
	start := time.Now()
	if _, ok := h.acquire(ctx, "app-a"); ok {
		t.Fatal("app-a exceeded its per-app cap")
	}
	if time.Since(start) > 40*time.Millisecond {
		t.Fatal("over-cap read should be refused without waiting")
	}
	releaseB, ok := h.acquire(ctx, "app-b")
	if !ok {
		t.Fatal("app-b starved by app-a")
	}
	releaseB()
	releases[0]()
	release, ok := h.acquire(ctx, "app-a")
	if !ok {
		t.Fatal("app-a slot not returned after release")
	}
	release()
	releases[1]()
	h.perAppMu.Lock()
	defer h.perAppMu.Unlock()
	if len(h.perApp) != 0 {
		t.Fatalf("per-app counts leaked: %v", h.perApp)
	}
}

func TestAppReadPerAppSlotReturnedWhenBudgetWaitFails(t *testing.T) {
	h := &AppReadHandler{readBudget: 1, readWeight: 1, waitTimeout: 20 * time.Millisecond}
	ctx := context.Background()
	hold, ok := h.acquire(ctx, "app-b")
	if !ok {
		t.Fatal("first read refused")
	}
	if _, ok := h.acquire(ctx, "app-a"); ok {
		t.Fatal("budget exhausted but read admitted")
	}
	hold()
	h.perAppMu.Lock()
	defer h.perAppMu.Unlock()
	if len(h.perApp) != 0 {
		t.Fatalf("per-app slot leaked after a budget timeout: %v", h.perApp)
	}
}
