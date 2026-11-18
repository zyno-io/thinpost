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

func bootstrapDatabase(args []string) error {
	flags := flag.NewFlagSet("bootstrap-database", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	database := flags.String("database", "", "expected dedicated database")
	schema := flags.String("schema", "", "protected application schema")
	runtime := flags.String("runtime-role", "", "existing runtime login role")
	pooler := flags.String("pooler-auth-role", "", "optional pooler authentication role")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 || *database == "" || *schema == "" || *runtime == "" {
		return errors.New("bootstrap-database requires --database, --schema and --runtime-role")
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
	input := postgres.DatabaseBootstrap{Database: *database, Schema: *schema, RuntimeRole: *runtime, PoolerAuthRole: *pooler}
	if err = postgres.BootstrapDatabase(ctx, db, input); err != nil {
		return errors.New("database bootstrap failed: " + err.Error())
	}
	return nil
}
