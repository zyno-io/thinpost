package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/zyno-io/thinpost/internal/controlapp"
	"github.com/zyno-io/thinpost/internal/postgres"
)

func grantRuntime(args []string) error {
	flags := flag.NewFlagSet("grant-runtime", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	role := flags.String("role", "", "existing runtime login role")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 || *role == "" {
		return errors.New("grant-runtime requires --role")
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
	if err = postgres.GrantRuntimeRole(ctx, db, *role); err != nil {
		return errors.New("runtime database grants failed: " + err.Error())
	}
	return nil
}

func grantCollector(args []string) error {
	flags := flag.NewFlagSet("grant-collector", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	role := flags.String("role", "", "existing collector login role")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 || *role == "" {
		return errors.New("grant-collector requires --role")
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
	if err = postgres.GrantCollectorRole(ctx, db, *role); err != nil {
		return errors.New("collector database grants failed: " + err.Error())
	}
	return nil
}

func grantTrackingRetirement(args []string) error {
	flags := flag.NewFlagSet("grant-tracking-retirement", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	role := flags.String("role", "", "existing retirement login role")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 || *role == "" {
		return errors.New("grant-tracking-retirement requires --role")
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
	if err = postgres.GrantTrackingRetirementRole(ctx, db, *role); err != nil {
		return errors.New("tracking retirement grants failed: " + err.Error())
	}
	return nil
}
