package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/zyno-io/thinpost/internal/controlapp"
	"github.com/zyno-io/thinpost/internal/postgres"
)

// retryPolicy prints the stored delivery retry policy. The policy is edited in
// the operator console (Operations page); there is no command to change it.
func retryPolicy(args []string) error {
	if len(args) != 1 || args[0] != "inspect" {
		return errors.New("retry-policy requires inspect; edit the policy in the console")
	}
	dsn, err := controlapp.DatabaseURL(os.Getenv)
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return errors.New("database configuration failed")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err = postgres.CheckSchema(ctx, db); err != nil {
		return errors.New("retry policy requires current schema")
	}
	status, err := postgres.InspectRetryPolicy(ctx, db)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Revision    int64  `json:"revision"`
		ChangedBy   string `json:"changed_by,omitempty"`
		MaxAttempts int    `json:"max_attempts"`
		InitialMS   int64  `json:"initial_ms"`
		MaxDelayMS  int64  `json:"max_delay_ms"`
		MaxAgeMS    int64  `json:"max_age_ms"`
	}{status.Revision, status.ChangedBy, status.Policy.MaxAttempts, status.Policy.InitialDelay.Milliseconds(), status.Policy.MaxDelay.Milliseconds(), status.Policy.MaxAge.Milliseconds()})
}
