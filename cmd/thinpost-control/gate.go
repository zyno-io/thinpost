package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/zyno-io/thinpost/internal/postgres"
)

// gate is the manual operator path for the admission gate outside a deploy.
// Every change is a compare-and-swap on the inspected revision. Open only
// reopens a gate after the operator attests the rollout is verified.
func gate(args []string) error {
	if len(args) == 0 {
		return errors.New("missing gate operation")
	}
	operation := args[0]
	flags := flag.NewFlagSet("gate "+operation, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	actor := flags.String("actor", "", "responsible operator")
	revision := flags.Int64("expected-revision", 0, "expected gate revision")
	generation := flags.String("generation", "", "deployment generation")
	schema := flags.Int("expected-schema", 0, "expected applied schema version")
	role := flags.String("role", "", "runtime login role whose privileges are verified")
	verified := flags.Bool("rollout-verified", false, "every runtime replica runs the intended image on this schema")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 {
		return errors.New("invalid gate arguments")
	}
	switch operation {
	case "inspect":
	case "close":
		if *actor == "" || *revision <= 0 || *generation == "" {
			return errors.New("gate close requires --actor, --expected-revision and --generation")
		}
	case "open":
		if *actor == "" || *revision <= 0 || *generation == "" || *schema <= 0 || *role == "" || !*verified {
			return errors.New("gate open requires --actor, --expected-revision, --generation, --expected-schema, --role and --rollout-verified")
		}
	default:
		return errors.New("unknown gate operation")
	}
	db, err := deployDatabase("thinpost-gate")
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch operation {
	case "close":
		_, err = postgres.CloseGate(ctx, db, *generation, *actor, *revision)
	case "open":
		err = postgres.OperatorReopenGate(ctx, db, *generation, *actor, *role, *revision, *schema)
	}
	if err != nil {
		// Database error text can quote data; only the category is returned.
		if category := postgres.DeployErrorCategory(err); category != "" {
			return errors.New("gate " + operation + " failed: " + category)
		}
		return errors.New("gate " + operation + " failed")
	}
	status, err := postgres.InspectGate(ctx, db)
	if err != nil {
		return errors.New("gate inspection failed")
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}
