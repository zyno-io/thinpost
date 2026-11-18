package deployhook

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/postgres"
)

// Failure is a hook failure with a fixed reason code, safe to log and to write
// to the container termination message. Detail is limited to the database
// layer's category, migration number and SQLSTATE.
type Failure struct {
	Reason string
	Detail string
}

func (f *Failure) Error() string {
	if f.Detail != "" {
		return "deploy hook failed: " + f.Reason + " (" + f.Detail + ")"
	}
	return "deploy hook failed: " + f.Reason
}

func fail(reason string, cause error) *Failure {
	failure := &Failure{Reason: reason}
	var deploy *postgres.DeployError
	if errors.As(cause, &deploy) {
		failure.Detail = deploy.Error()
	}
	return failure
}

// Database is the set of gate and schema operations the hooks need.
type Database interface {
	LedgerExists(ctx context.Context) (bool, error)
	SchemaVersion(ctx context.Context) (int, error)
	Target() (int, error)
	ManualBetween(from, to int) (int, bool)
	Inspect(ctx context.Context) (postgres.GateStatus, error)
	Unprepared(ctx context.Context) (int64, error)
	Close(ctx context.Context, generation, actor string, revision int64) (int64, error)
	Adopt(ctx context.Context, prefix, actor string, revision int64, maxAge time.Duration) (int64, error)
	Migrate(ctx context.Context, role, actor string, revision int64, keepOwner bool) (from, to int, revision2 int64, err error)
	Reopen(ctx context.Context, generation, actor, role string, revision int64, schema int) error
	CheckRole(ctx context.Context, role string) error
}

// Postgres adapts a maintenance connection to Database.
type Postgres struct{ DB *sql.DB }

func (p Postgres) LedgerExists(ctx context.Context) (bool, error) {
	return postgres.LedgerExists(ctx, p.DB)
}
func (p Postgres) SchemaVersion(ctx context.Context) (int, error) {
	return postgres.SchemaVersion(ctx, p.DB)
}
func (Postgres) Target() (int, error) { return postgres.DeployTarget() }
func (Postgres) ManualBetween(from, to int) (int, bool) {
	return postgres.DeployManualBetween(from, to)
}
func (p Postgres) Inspect(ctx context.Context) (postgres.GateStatus, error) {
	return postgres.InspectGate(ctx, p.DB)
}
func (p Postgres) Unprepared(ctx context.Context) (int64, error) {
	return postgres.UnpreparedMessages(ctx, p.DB)
}
func (p Postgres) Close(ctx context.Context, generation, actor string, revision int64) (int64, error) {
	return postgres.CloseGate(ctx, p.DB, generation, actor, revision)
}
func (p Postgres) Adopt(ctx context.Context, prefix, actor string, revision int64, maxAge time.Duration) (int64, error) {
	return postgres.AdoptDeployClosure(ctx, p.DB, prefix, actor, revision, maxAge)
}
func (p Postgres) Migrate(ctx context.Context, role, actor string, revision int64, keepOwner bool) (int, int, int64, error) {
	return postgres.DeployMigrate(ctx, p.DB, role, actor, revision, keepOwner)
}
func (p Postgres) Reopen(ctx context.Context, generation, actor, role string, revision int64, schema int) error {
	return postgres.ReopenGate(ctx, p.DB, generation, actor, role, revision, schema)
}
func (p Postgres) CheckRole(ctx context.Context, role string) error {
	return postgres.CheckRuntimeRole(ctx, p.DB, role)
}

// ActorPrefix returns the part of a deploy actor shared by every revision of
// one release: "deploy:<namespace>/<release>:".
func ActorPrefix(actor string) string {
	for i := len(actor) - 1; i >= 0; i-- {
		if actor[i] == ':' {
			return actor[:i+1]
		}
	}
	return ""
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
