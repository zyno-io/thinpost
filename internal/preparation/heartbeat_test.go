package preparation

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/oplog"
)

// slowObjects delays every upload and honours cancellation like a real store.
type slowObjects struct {
	*fakeObjects
	delay time.Duration
	puts  int
}

func (o *slowObjects) PutMIME(ctx context.Context, key string, data []byte) error {
	o.puts++
	select {
	case <-time.After(o.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return o.fakeObjects.PutMIME(ctx, key, data)
}

func slowSetup(t *testing.T, delay time.Duration) (*Service, *fakeRepository, *slowObjects) {
	t.Helper()
	service, repo, objects, _ := setup(t)
	slow := &slowObjects{fakeObjects: objects, delay: delay}
	service.Objects = slow
	service.LeaseTTL = 200 * time.Millisecond
	service.RenewInterval = 50 * time.Millisecond
	return service, repo, slow
}

func assertNoRenewAfterFinal(t *testing.T, repo *fakeRepository) {
	t.Helper()
	repo.mu.Lock()
	defer repo.mu.Unlock()
	final := false
	for _, event := range repo.events {
		if event != "renew" {
			final = true
		} else if final {
			t.Fatal("renewal after commit or failure:", repo.events)
		}
	}
}

func TestLongPreparationRenewsAndCommits(t *testing.T) {
	// Two uploads of 250 ms outlive the 200 ms lease; only renewal keeps it.
	service, repo, _ := slowSetup(t, 250*time.Millisecond)
	before := runtime.NumGoroutine()
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.commits != 1 || len(repo.failures) != 0 || repo.renewals < 4 {
		t.Fatalf("commits=%d failures=%v renewals=%d", repo.commits, repo.failures, repo.renewals)
	}
	assertNoRenewAfterFinal(t, repo)
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
}

func TestLostClaimCancelsWorkAndWritesNothing(t *testing.T) {
	lines := []string{}
	defer oplog.SetOutput(func(format string, args ...any) { lines = append(lines, format) })()
	service, repo, objects := slowSetup(t, time.Second)
	repo.renewFn = func(call int) error {
		if call >= 2 {
			return ErrLostLease
		}
		return nil
	}
	start := time.Now()
	err := service.PrepareNext(context.Background())
	if !errors.Is(err, ErrLostLease) {
		t.Fatal("expected lost lease, got", err)
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Fatal("upload was not canceled")
	}
	if repo.commits != 0 || len(repo.failures) != 0 || len(objects.final) != 0 || objects.puts != 1 || repo.renewals != 2 {
		t.Fatalf("commits=%d failures=%v puts=%d renewals=%d", repo.commits, repo.failures, objects.puts, repo.renewals)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "preparation_lease_lost") {
		t.Fatal("loss not logged exactly once", lines)
	}
}

func TestTransientRenewalErrorsAreTolerated(t *testing.T) {
	service, repo, _ := slowSetup(t, 250*time.Millisecond)
	service.LeaseTTL = 400 * time.Millisecond
	repo.renewFn = func(call int) error {
		if call == 2 || call == 3 {
			return errors.New("database unavailable")
		}
		return nil
	}
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.commits != 1 || len(repo.failures) != 0 {
		t.Fatal("transient renewal errors aborted preparation")
	}
}

func TestPersistentRenewalErrorsCancelOnceLeaseWouldLapse(t *testing.T) {
	service, repo, objects := slowSetup(t, time.Second)
	repo.renewFn = func(int) error { return errors.New("database unavailable") }
	err := service.PrepareNext(context.Background())
	if !errors.Is(err, ErrLostLease) || repo.commits != 0 || len(repo.failures) != 0 || len(objects.final) != 0 {
		t.Fatalf("err=%v commits=%d failures=%v", err, repo.commits, repo.failures)
	}
	assertNoRenewAfterFinal(t, repo)
}

func TestMaxPreparationStopsRenewal(t *testing.T) {
	service, repo, _ := slowSetup(t, 250*time.Millisecond)
	service.MaxPreparation = 120 * time.Millisecond
	_ = service.PrepareNext(context.Background())
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.renewals == 0 || repo.renewals > 3 {
		t.Fatal("renewal did not stop at the cap", repo.renewals)
	}
}

func TestFailureIsRecordedAfterHeartbeatStops(t *testing.T) {
	service, repo, objects := slowSetup(t, 100*time.Millisecond)
	objects.putErr = errors.New("offline")
	if err := service.PrepareNext(context.Background()); err == nil || len(repo.failures) != 1 {
		t.Fatal("failure not recorded", err)
	}
	assertNoRenewAfterFinal(t, repo)
}
