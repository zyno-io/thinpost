package postgres

import (
	"context"
	"testing"
	"time"
)

// A submission locks the app and then shares the API key (ReserveSubmission,
// Commit). Revocation must take the same order, or it deadlocks against a
// submission in flight and the key stays live.
func TestKeyRevocationDoesNotDeadlockWithASubmission(t *testing.T) {
	ctx := context.Background()
	db := migratedSchemaForTest(t, ctx, "key_revoke_")
	const generation = "00000000000000000000000000000ddf"
	if _, err := db.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation); err != nil {
		t.Fatal(err)
	}
	repo := &Repository{DB: db, Generation: generation}
	app, err := repo.CreateApp(ctx, "operator", "Payments")
	if err != nil {
		t.Fatal(err)
	}
	issued, err := repo.IssueKeyForOperator(ctx, "operator", app.ID, "Primary")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Rollback()
	if _, err = sub.ExecContext(ctx, `SELECT 1 FROM apps WHERE id=$1 FOR UPDATE`, app.ID); err != nil {
		t.Fatal(err)
	}
	revoked := make(chan error, 1)
	go func() { revoked <- repo.RevokeKeyForOperator(ctx, "operator", app.ID, issued.Key.ID) }()
	time.Sleep(300 * time.Millisecond) // let the revocation start waiting
	if _, err = sub.ExecContext(ctx, `SELECT 1 FROM api_keys WHERE id=$1 FOR SHARE`, issued.Key.ID); err != nil {
		t.Fatal("submission side:", err)
	}
	if err = sub.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-revoked; err != nil {
		t.Fatal("revocation:", err)
	}
	var active bool
	if err = db.QueryRowContext(ctx, `SELECT active FROM api_keys WHERE id=$1`, issued.Key.ID).Scan(&active); err != nil || active {
		t.Fatal("key still active", err)
	}
}
