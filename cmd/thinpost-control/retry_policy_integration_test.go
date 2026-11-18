package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/postgres"
)

func TestRetryPolicyCommand(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	u, err := url.Parse(dsn)
	if err != nil || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("test requires a loopback database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	schema := "retry_cli_" + hex.EncodeToString(id[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = postgres.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	t.Setenv("THINPOST_DATABASE_URL", u.String())
	t.Setenv("THINPOST_ALLOW_INSECURE_LOCAL_DB", "1")
	if err = retryPolicy([]string{"inspect"}); err != nil {
		t.Fatal(err)
	}
	// The command no longer changes the policy; the console does.
	if err = retryPolicy([]string{"set", "--revision", "1", "--actor", "operator"}); err == nil {
		t.Fatal("retry-policy set must be retired")
	}
	status, err := postgres.InspectRetryPolicy(ctx, db)
	if err != nil || status.Revision != 1 || status.Policy.MaxAttempts != 128 || status.ChangedBy != "migration:033" {
		t.Fatal("unexpected stored policy", status, err)
	}
}
