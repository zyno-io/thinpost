package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/submission"
)

func testSubmissionBurst(t *testing.T, ctx context.Context, owner *sql.DB, repo *Repository) {
	t.Helper()
	const app = "fffffffffffffffffffffffffffffe01"
	const key = "fffffffffffffffffffffffffffffe02"
	digest := sha256.Sum256([]byte("burst-test-key"))
	if _, err := owner.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'burst')`, app); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ExecContext(ctx, `INSERT INTO api_keys(id,app_id,digest) VALUES ($1,$2,$3)`, key, app, digest[:]); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 1); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatal("default was not disabled", err)
	}
	if _, err := repo.SetSubmissionRate(ctx, "operator", app, 1, 7); err != nil {
		t.Fatal("enable rate", err)
	}
	if err := repo.ReserveSubmission(ctx, app, "key-a", 1); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("another app's key was admitted", err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 8); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatal("oversized batch admitted", err)
	}
	const workers = 40
	results := make(chan error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Separate instances share only PostgreSQL, as control replicas do.
			peer := &Repository{DB: repo.DB, Generation: repo.Generation}
			results <- peer.ReserveSubmission(ctx, app, key, 1)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, denied := 0, 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, submission.ErrRateLimited):
			denied++
		default:
			t.Fatal("reservation failed unexpectedly", err)
		}
	}
	if accepted != 7 || denied != workers-7 {
		t.Fatal("concurrent replicas exceeded capacity", accepted, denied)
	}
	if _, err := owner.ExecContext(ctx, `UPDATE apps SET submission_tokens=0,submission_refilled_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 1); err != nil {
		t.Fatal("refill did not admit a message", err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 1); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatal("refill admitted a second message", err)
	}
	if _, err := owner.ExecContext(ctx, `UPDATE apps SET submission_tokens=0,submission_refilled_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 1); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatal("backward clock granted tokens", err)
	}
	if _, err := repo.SetSubmissionRate(ctx, "operator", app, 2, 10); err != nil {
		t.Fatal("change enabled rate", err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 1); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatal("changing the rate manufactured tokens", err)
	}
	if _, err := repo.SetSubmissionRate(ctx, "operator", app, 0, 10); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("invalid rate pair accepted", err)
	}
	if _, err := repo.SetSubmissionRate(ctx, "operator", app, 0, 0); err != nil {
		t.Fatal("disable rate", err)
	}
	if _, err := repo.SetSubmissionRate(ctx, "operator", app, 1, 7); err != nil {
		t.Fatal("reenable rate", err)
	}
	if _, err := owner.ExecContext(ctx, `UPDATE api_keys SET active=false WHERE id=$1`, key); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 1); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("revoked key was admitted", err)
	}
	if _, err := owner.ExecContext(ctx, `UPDATE api_keys SET active=true WHERE id=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ExecContext(ctx, `UPDATE apps SET active=false WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 1); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("paused app was admitted", err)
	}
	var audits int
	if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='app.submission_rate.updated'`, app).Scan(&audits); err != nil || audits != 4 {
		t.Fatal("rate changes were not audited", audits, err)
	}
	testSubmissionRateUpdates(t, ctx, owner, repo)
}

func testSubmissionRateUpdates(t *testing.T, ctx context.Context, owner *sql.DB, repo *Repository) {
	t.Helper()
	const app = "fffffffffffffffffffffffffffffe03"
	const key = "fffffffffffffffffffffffffffffe04"
	digest := sha256.Sum256([]byte("rate-update-test-key"))
	if _, err := owner.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'rate updates')`, app); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ExecContext(ctx, `INSERT INTO api_keys(id,app_id,digest) VALUES ($1,$2,$3)`, key, app, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetSubmissionRate(ctx, "operator", app, 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ExecContext(ctx, `UPDATE apps SET submission_tokens=0,submission_refilled_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	var before, after time.Time
	if err := owner.QueryRowContext(ctx, `SELECT submission_refilled_at FROM apps WHERE id=$1`, app).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetSubmissionRate(ctx, "operator", app, 1, 2); err != nil {
		t.Fatal(err)
	}
	var tokens int64
	if err := owner.QueryRowContext(ctx, `SELECT submission_tokens,submission_refilled_at FROM apps WHERE id=$1`, app).Scan(&tokens, &after); err != nil || tokens != 0 || !before.Equal(after) {
		t.Fatal("identical save changed bucket state", tokens, before, after, err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 1); err != nil {
		t.Fatal("identical save lost accrued refill", err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 1); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatal("identical save manufactured tokens", err)
	}
	if _, err := owner.ExecContext(ctx, `UPDATE apps SET submission_tokens=0,submission_refilled_at=clock_timestamp()-interval '2 minutes' WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetSubmissionRate(ctx, "operator", app, 2, 5); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 2); err != nil {
		t.Fatal("rate change lost refill at the previous rate", err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 1); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatal("rate change applied the new rate or capacity retroactively", err)
	}
	if _, err := owner.ExecContext(ctx, `UPDATE apps SET submission_tokens=$2,submission_refilled_at=clock_timestamp() WHERE id=$1`, app, 5*submissionTokenUnit); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetSubmissionRate(ctx, "operator", app, 2, 1); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 1); err != nil {
		t.Fatal("shrinking capacity lost valid credit", err)
	}
	if err := repo.ReserveSubmission(ctx, app, key, 1); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatal("shrinking capacity retained excess tokens", err)
	}
	if _, err := owner.ExecContext(ctx, `UPDATE apps SET submission_tokens=0,submission_refilled_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			peer := &Repository{DB: repo.DB, Generation: repo.Generation}
			if _, err := peer.SetSubmissionRate(ctx, "operator", app, 2, 1); err != nil {
				results <- err
				return
			}
			results <- peer.ReserveSubmission(ctx, app, key, 1)
		}()
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, submission.ErrRateLimited) {
			t.Fatal("concurrent save/reservation failed", err)
		}
	}
	if accepted != 1 {
		t.Fatal("concurrent no-op saves changed refill", accepted)
	}
	var audits int
	if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='app.submission_rate.updated'`, app).Scan(&audits); err != nil || audits != 3 {
		t.Fatal("changes or no-op saves were audited incorrectly", audits, err)
	}
}
