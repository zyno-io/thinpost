package deployhook

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/postgres"
)

// staleClosure is the oldest deploy-owned closure a later attempt may adopt.
const staleClosure = 24 * time.Hour

// PreConfig configures the pre-upgrade hook.
type PreConfig struct {
	// Actor is "deploy:<namespace>/<release>:r<revision>".
	Actor string
	// Generation is the configured deployment generation.
	Generation  string
	RuntimeRole string
	// DrainTimeout bounds the wait for queued or preparing messages before
	// the gate is closed for a schema that cannot read their source objects.
	DrainTimeout time.Duration
	// Check reports the plan without changing anything.
	Check bool
	// Poll is the drain polling interval; it defaults to two seconds.
	Poll time.Duration
	// FailbackTimeout bounds the gate restoration after a failed migration.
	FailbackTimeout time.Duration
	Log             func(format string, args ...any)
}

// PreResult is the JSON summary a hook prints. It holds only versions,
// revisions and fixed action names.
type PreResult struct {
	Action   string `json:"action"`
	From     int    `json:"from"`
	To       int    `json:"to"`
	Gate     string `json:"gate"`
	Revision int64  `json:"revision"`
}

// RunPre migrates the database to the schema this image expects. A deploy that
// needs no schema change never closes the gate.
func RunPre(ctx context.Context, db Database, cfg PreConfig) (PreResult, error) {
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if !strings.HasPrefix(cfg.Actor, postgres.DeployActorPrefix) || ActorPrefix(cfg.Actor) == "" || cfg.Generation == "" || cfg.RuntimeRole == "" {
		return PreResult{}, &Failure{Reason: "invalid_configuration"}
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 2 * time.Second
	}
	if cfg.FailbackTimeout <= 0 {
		cfg.FailbackTimeout = 30 * time.Second
	}
	prefix := ActorPrefix(cfg.Actor)
	target, err := db.Target()
	if err != nil {
		return PreResult{}, fail("migrations_not_reviewed", err)
	}
	exists, err := db.LedgerExists(ctx)
	if err != nil {
		return PreResult{}, fail("database_unavailable", err)
	}
	if !exists {
		return PreResult{}, &Failure{Reason: "ledger_missing"}
	}
	current, err := db.SchemaVersion(ctx)
	switch {
	case errors.Is(err, postgres.ErrUnknownMigration):
		return PreResult{}, &Failure{Reason: "database_newer_or_unknown"}
	case errors.Is(err, postgres.ErrMigrationChecksum):
		return PreResult{}, &Failure{Reason: "migration_checksum_mismatch"}
	case err != nil:
		return PreResult{}, fail("schema_check_failed", err)
	}
	status, err := db.Inspect(ctx)
	if err != nil {
		return PreResult{}, fail("gate_inspect_failed", err)
	}
	result := PreResult{From: current, To: target, Gate: status.Mode, Revision: status.Revision}
	owned := status.Mode == "closed" && strings.HasPrefix(status.ChangedBy, prefix)

	if current == target {
		result.Action = "current"
		if owned {
			if status.Generation == "" || status.Generation != cfg.Generation {
				return result, &Failure{Reason: "generation_mismatch"}
			}
			result.Action = "readopt_closure"
			if cfg.Check {
				return result, nil
			}
			revision, adoptErr := db.Adopt(ctx, prefix, cfg.Actor, status.Revision, staleClosure)
			if adoptErr != nil {
				return result, fail("stale_deploy_closure", adoptErr)
			}
			result.Revision = revision
			logf("schema current; adopted an earlier deploy's closed gate so this release reopens it")
		}
		return result, nil
	}
	if current > target {
		return result, &Failure{Reason: "database_newer_or_unknown"}
	}

	// current < target.
	if status.Mode == "recovery" {
		return result, &Failure{Reason: "recovery_in_progress"}
	}
	if current < postgres.DeployMigrationFloor {
		return result, &Failure{Reason: "below_migration_floor"}
	}
	if _, manual := db.ManualBetween(current, target); manual {
		return result, &Failure{Reason: "manual_migration_required"}
	}
	if status.Generation != "" && status.Generation != cfg.Generation {
		return result, &Failure{Reason: "generation_mismatch"}
	}
	crossing := current < postgres.MessageMetadataSchema && target >= postgres.MessageMetadataSchema
	switch {
	case status.Mode == "open":
		result.Action = "close_and_migrate"
	case owned:
		result.Action = "adopt_and_migrate"
	case status.Mode == "closed":
		result.Action = "migrate_operator_closed"
	default:
		return result, &Failure{Reason: "unexpected_gate_mode"}
	}
	if cfg.Check {
		return result, nil
	}

	closedByRun := false
	revision := status.Revision
	switch result.Action {
	case "close_and_migrate":
		if status.Generation == "" {
			return result, &Failure{Reason: "generation_missing"}
		}
		if crossing {
			if drainErr := drain(ctx, db, cfg); drainErr != nil {
				return result, drainErr
			}
		}
		revision, err = db.Close(ctx, cfg.Generation, cfg.Actor, revision)
		if err != nil {
			return result, fail("gate_close_failed", err)
		}
		closedByRun = true
		logf("admission gate closed for schema %d to %d", current, target)
	case "adopt_and_migrate":
		revision, err = db.Adopt(ctx, prefix, cfg.Actor, revision, staleClosure)
		if err != nil {
			return result, fail("stale_deploy_closure", err)
		}
		logf("adopted a closed gate left by an earlier deploy")
	default:
		logf("gate was closed by an operator; it will not be reopened by this deploy")
	}

	failback := func(reason string, cause error) error {
		return failBack(db, cfg, logf, closedByRun, revision, current, reason, cause)
	}
	if crossing {
		pending, countErr := db.Unprepared(ctx)
		if countErr != nil {
			return result, failback("unprepared_check_failed", countErr)
		}
		if pending != 0 {
			return result, failback("unprepared_messages", nil)
		}
	}
	from, to, newRevision, err := db.Migrate(ctx, cfg.RuntimeRole, cfg.Actor, revision, result.Action == "migrate_operator_closed")
	if err != nil {
		return result, failback("migrate_failed", err)
	}
	result.From, result.To, result.Revision, result.Gate = from, to, newRevision, "closed"
	logf("schema migrated from %d to %d with runtime grants applied", from, to)
	return result, nil
}

func drain(ctx context.Context, db Database, cfg PreConfig) error {
	deadline := time.Now().Add(cfg.DrainTimeout)
	for {
		pending, err := db.Unprepared(ctx)
		if err != nil {
			return fail("unprepared_check_failed", err)
		}
		if pending == 0 {
			return nil
		}
		if !time.Now().Add(cfg.Poll).Before(deadline) {
			return &Failure{Reason: "drain_timeout"}
		}
		if sleepContext(ctx, cfg.Poll) != nil {
			return &Failure{Reason: "interrupted"}
		}
	}
}

// failBack restores the gate this run closed when the schema is provably
// unchanged. It uses a fresh context so an interrupt cannot strand the gate.
// An ambiguous or unverifiable outcome leaves the gate closed.
func failBack(db Database, cfg PreConfig, logf func(string, ...any), closedByRun bool, revision int64, from int, reason string, cause error) error {
	failure := fail(reason, cause)
	if !closedByRun {
		return failure
	}
	if postgres.DeployErrorCategory(cause) == postgres.DeployCategoryAmbiguous {
		logf("migration outcome is ambiguous; the gate is left closed")
		return failure
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.FailbackTimeout)
	defer cancel()
	version, err := db.SchemaVersion(ctx)
	if err != nil || version != from {
		logf("schema state could not be confirmed unchanged; the gate is left closed")
		return failure
	}
	if err = db.Reopen(ctx, cfg.Generation, cfg.Actor, cfg.RuntimeRole, revision, from); err != nil {
		logf("the gate could not be reopened after the failure; it is left closed")
		return failure
	}
	logf("migration failed before commit; the gate was reopened")
	return failure
}
