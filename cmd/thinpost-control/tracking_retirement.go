package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/zyno-io/thinpost/internal/controlapp"
	"github.com/zyno-io/thinpost/internal/postgres"
)

// This command only retires references. It does not collect ciphertext,
// authenticate the claimed operator, or make a restored database safe.
func retireTracking(args []string) error {
	flags := flag.NewFlagSet("retire-tracking", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	actor := flags.String("actor", "", "authenticated operator or service identity")
	generation := flags.String("generation", "", "current deployment generation")
	limit := flags.Int("limit", 100, "maximum references in this batch")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 || *actor == "" || *generation == "" || *limit < 1 || *limit > 100 {
		return errors.New("retire-tracking requires --actor, --generation and a limit from 1 to 100")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = postgres.CheckSchema(ctx, db); err != nil {
		return errors.New("database schema check failed")
	}
	if err = postgres.CheckTrackingRetirementRolePrivileges(ctx, db); err != nil {
		return errors.New("tracking retirement database role rejected")
	}
	repo := &postgres.Repository{DB: db, Generation: *generation}
	count, err := repo.RetireExpiredTrackingMappings(ctx, *actor, *limit)
	if err != nil {
		return errors.New("tracking retirement batch failed")
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Retired int `json:"retired"`
	}{Retired: count})
}
