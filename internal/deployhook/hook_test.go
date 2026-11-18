package deployhook

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/postgres"
)

const (
	testGeneration = "0123456789abcdef0123456789abcdef"
	testActor      = "deploy:thinpost-control/thinpost:r7"
	testPrefix     = "deploy:thinpost-control/thinpost:"
)

type fakeDB struct {
	mu          sync.Mutex
	ledger      bool
	version     int
	target      int
	status      postgres.GateStatus
	unprepared  []int64
	migrateErr  error
	closeErr    error
	reopenErr   error
	roleErr     error
	versionErr  error
	calls       []string
	keepOwner   bool
	reopenedAt  int
	manualAt    int
	failbackRev int64
}

func (f *fakeDB) note(name string) { f.calls = append(f.calls, name) }
func (f *fakeDB) called(name string) bool {
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}
func (f *fakeDB) LedgerExists(context.Context) (bool, error) { return f.ledger, nil }
func (f *fakeDB) SchemaVersion(context.Context) (int, error) { return f.version, f.versionErr }
func (f *fakeDB) Target() (int, error)                       { return f.target, nil }
func (f *fakeDB) ManualBetween(from, to int) (int, bool) {
	if f.manualAt > from && f.manualAt <= to {
		return f.manualAt, true
	}
	return 0, false
}
func (f *fakeDB) Inspect(context.Context) (postgres.GateStatus, error) { return f.status, nil }
func (f *fakeDB) Unprepared(context.Context) (int64, error) {
	f.note("unprepared")
	if len(f.unprepared) == 0 {
		return 0, nil
	}
	n := f.unprepared[0]
	if len(f.unprepared) > 1 {
		f.unprepared = f.unprepared[1:]
	}
	return n, nil
}
func (f *fakeDB) Close(_ context.Context, _, actor string, revision int64) (int64, error) {
	f.note("close")
	if f.closeErr != nil {
		return 0, f.closeErr
	}
	f.status.Mode, f.status.ChangedBy, f.status.Revision = "closed", actor, revision+1
	return revision + 1, nil
}
func (f *fakeDB) Adopt(_ context.Context, _, actor string, revision int64, _ time.Duration) (int64, error) {
	f.note("adopt")
	f.status.ChangedBy, f.status.Revision = actor, revision+1
	return revision + 1, nil
}
func (f *fakeDB) Migrate(_ context.Context, _, actor string, revision int64, keep bool) (int, int, int64, error) {
	f.note("migrate")
	f.keepOwner = keep
	if f.migrateErr != nil {
		return 0, 0, 0, f.migrateErr
	}
	from := f.version
	f.version = f.target
	return from, f.target, revision + 1, nil
}
func (f *fakeDB) Reopen(_ context.Context, _, actor, _ string, revision int64, schema int) error {
	f.note("reopen")
	f.reopenedAt, f.failbackRev = schema, revision
	if f.reopenErr != nil {
		return f.reopenErr
	}
	f.status.Mode = "open"
	return nil
}
func (f *fakeDB) CheckRole(context.Context, string) error { f.note("checkrole"); return f.roleErr }

func newFake() *fakeDB {
	return &fakeDB{ledger: true, version: 27, target: 28, status: postgres.GateStatus{Mode: "open", Generation: testGeneration, Revision: 5}}
}

func preConfig() PreConfig {
	return PreConfig{Actor: testActor, Generation: testGeneration, RuntimeRole: "runtime", DrainTimeout: 50 * time.Millisecond, Poll: time.Millisecond}
}

func reason(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Reason
	}
	return "NOT-A-FAILURE: " + err.Error()
}

func TestPreHappyPathClosesAndMigrates(t *testing.T) {
	f := newFake()
	result, err := RunPre(context.Background(), f, preConfig())
	if err != nil || result.From != 27 || result.To != 28 || result.Revision != 7 || result.Gate != "closed" {
		t.Fatal(result, err)
	}
	if strings.Join(f.calls, ",") != "unprepared,close,unprepared,migrate" || f.keepOwner {
		t.Fatal(f.calls, f.keepOwner)
	}
}

func TestPreNoSchemaChangeNeverTouchesGate(t *testing.T) {
	f := newFake()
	f.version = 28
	result, err := RunPre(context.Background(), f, preConfig())
	if err != nil || result.Action != "current" || len(f.calls) != 0 {
		t.Fatal(result, err, f.calls)
	}
}

func TestPreReadoptsEarlierDeployClosureWhenCurrent(t *testing.T) {
	f := newFake()
	f.version = 28
	f.status = postgres.GateStatus{Mode: "closed", Generation: testGeneration, ChangedBy: testPrefix + "r6", Revision: 9}
	result, err := RunPre(context.Background(), f, preConfig())
	if err != nil || result.Action != "readopt_closure" || !f.called("adopt") || f.status.ChangedBy != testActor {
		t.Fatal(result, err, f.calls)
	}
	// An operator's closure, or another release's, is left alone.
	for _, owner := range []string{"alice", "deploy:other/release:r1"} {
		g := newFake()
		g.version = 28
		g.status = postgres.GateStatus{Mode: "closed", Generation: testGeneration, ChangedBy: owner, Revision: 9}
		if _, err = RunPre(context.Background(), g, preConfig()); err != nil || g.called("adopt") {
			t.Fatal(owner, err, g.calls)
		}
	}
}

func TestPreRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeDB)
		want   string
	}{
		{"ledger missing", func(f *fakeDB) { f.ledger = false }, "ledger_missing"},
		{"newer", func(f *fakeDB) { f.versionErr = postgres.ErrUnknownMigration }, "database_newer_or_unknown"},
		{"checksum", func(f *fakeDB) { f.versionErr = postgres.ErrMigrationChecksum }, "migration_checksum_mismatch"},
		{"ahead", func(f *fakeDB) { f.version = 29 }, "database_newer_or_unknown"},
		{"recovery", func(f *fakeDB) { f.status.Mode = "recovery" }, "recovery_in_progress"},
		{"below floor", func(f *fakeDB) { f.version = 26 }, "below_migration_floor"},
		{"manual", func(f *fakeDB) { f.manualAt = 28 }, "manual_migration_required"},
		{"generation", func(f *fakeDB) { f.status.Generation = "ffffffffffffffffffffffffffffffff" }, "generation_mismatch"},
		{"missing generation", func(f *fakeDB) { f.status.Generation = "" }, "generation_missing"},
		{"close fails", func(f *fakeDB) {
			f.closeErr = &postgres.DeployError{Category: postgres.DeployCategoryLock, SQLState: "55P03"}
		}, "gate_close_failed"},
	}
	for _, c := range cases {
		f := newFake()
		c.mutate(f)
		_, err := RunPre(context.Background(), f, preConfig())
		if reason(err) != c.want {
			t.Errorf("%s: %v", c.name, err)
		}
		if f.called("migrate") {
			t.Errorf("%s: migrated despite refusal", c.name)
		}
	}
}

func TestPreDrainTimeoutLeavesGateOpen(t *testing.T) {
	f := newFake()
	f.unprepared = []int64{3}
	_, err := RunPre(context.Background(), f, preConfig())
	if reason(err) != "drain_timeout" || f.called("close") {
		t.Fatal(err, f.calls)
	}
}

func TestPreDrainWaitsThenProceeds(t *testing.T) {
	f := newFake()
	f.unprepared = []int64{2, 1, 0}
	if _, err := RunPre(context.Background(), f, preConfig()); err != nil || !f.called("migrate") {
		t.Fatal(err, f.calls)
	}
}

func TestPreUnpreparedUnderClosureFailsBack(t *testing.T) {
	f := newFake()
	f.unprepared = []int64{0, 4}
	_, err := RunPre(context.Background(), f, preConfig())
	if reason(err) != "unprepared_messages" || f.called("migrate") || !f.called("reopen") || f.reopenedAt != 27 || f.failbackRev != 6 {
		t.Fatal(err, f.calls, f.reopenedAt, f.failbackRev)
	}
}

func TestPreMigrateFailureFailsBackOnlyWhenSchemaUnchangedAndUnambiguous(t *testing.T) {
	f := newFake()
	f.migrateErr = &postgres.DeployError{Category: postgres.DeployCategoryMigration, Migration: 28, SQLState: "23505"}
	_, err := RunPre(context.Background(), f, preConfig())
	var failure *Failure
	if !errors.As(err, &failure) || failure.Reason != "migrate_failed" || !strings.Contains(failure.Detail, "migration=28") || !strings.Contains(failure.Detail, "23505") || !f.called("reopen") {
		t.Fatal(err, f.calls)
	}

	g := newFake()
	g.migrateErr = &postgres.DeployError{Category: postgres.DeployCategoryAmbiguous}
	if _, err = RunPre(context.Background(), g, preConfig()); reason(err) != "migrate_failed" || g.called("reopen") {
		t.Fatal("reopened after an ambiguous commit", err, g.calls)
	}

	h := newFake()
	h.migrateErr = errors.New("boom")
	h.versionErr = errors.New("cannot read")
	// The first SchemaVersion call (preflight) must succeed, so fail it late.
	h.versionErr = nil
	calls := 0
	wrapped := &lateVersionFail{fakeDB: h, failAfter: 1, calls: &calls}
	if _, err = RunPre(context.Background(), wrapped, preConfig()); reason(err) != "migrate_failed" || h.called("reopen") {
		t.Fatal("reopened without confirming the schema", err, h.calls)
	}
}

type lateVersionFail struct {
	*fakeDB
	failAfter int
	calls     *int
}

func (l *lateVersionFail) SchemaVersion(ctx context.Context) (int, error) {
	*l.calls++
	if *l.calls > l.failAfter {
		return 0, errors.New("unavailable")
	}
	return l.fakeDB.SchemaVersion(ctx)
}

func TestPreFailbackUsesFreshContext(t *testing.T) {
	f := newFake()
	f.migrateErr = &postgres.DeployError{Category: postgres.DeployCategoryDatabase}
	ctx, cancel := context.WithCancel(context.Background())
	f.status.Mode = "open"
	// Unit-level check of the call sequence only; the real cancelled-context
	// behavior is proven by TestIntegrationFailbackSurvivesCancelledContextWhileMigrateIsBlocked.
	wrapped := &cancelOnMigrate{fakeDB: f, cancel: cancel}
	if _, err := RunPre(ctx, wrapped, preConfig()); reason(err) != "migrate_failed" || !f.called("reopen") {
		t.Fatal(err, f.calls)
	}
}

type cancelOnMigrate struct {
	*fakeDB
	cancel context.CancelFunc
}

func (c *cancelOnMigrate) Migrate(ctx context.Context, role, actor string, revision int64, keep bool) (int, int, int64, error) {
	c.cancel()
	return c.fakeDB.Migrate(ctx, role, actor, revision, keep)
}

func TestPreAdoptsDeployClosureWithoutFailback(t *testing.T) {
	f := newFake()
	f.status = postgres.GateStatus{Mode: "closed", Generation: testGeneration, ChangedBy: testPrefix + "r6", Revision: 9}
	f.migrateErr = &postgres.DeployError{Category: postgres.DeployCategoryMigration, Migration: 28}
	if _, err := RunPre(context.Background(), f, preConfig()); reason(err) != "migrate_failed" || !f.called("adopt") || f.called("reopen") {
		t.Fatal(err, f.calls)
	}
}

func TestPreOperatorClosureIsKeptAndNeverFailedBack(t *testing.T) {
	f := newFake()
	f.status = postgres.GateStatus{Mode: "closed", Generation: testGeneration, ChangedBy: "alice", Revision: 9}
	if _, err := RunPre(context.Background(), f, preConfig()); err != nil || !f.keepOwner || f.called("close") || f.called("adopt") {
		t.Fatal(err, f.calls, f.keepOwner)
	}
	g := newFake()
	g.status = f.status
	g.migrateErr = errors.New("x")
	if _, err := RunPre(context.Background(), g, preConfig()); err == nil || g.called("reopen") {
		t.Fatal("reopened an operator's closure", err)
	}
}

func TestPreCheckChangesNothing(t *testing.T) {
	f := newFake()
	cfg := preConfig()
	cfg.Check = true
	result, err := RunPre(context.Background(), f, cfg)
	if err != nil || result.Action != "close_and_migrate" || len(f.calls) != 0 {
		t.Fatal(result, err, f.calls)
	}
}

func TestPreSkipsDrainBelowSchema28Crossing(t *testing.T) {
	f := newFake()
	f.version, f.target = 28, 29
	f.unprepared = []int64{9}
	if _, err := RunPre(context.Background(), f, preConfig()); err != nil || f.called("unprepared") {
		t.Fatal(err, f.calls)
	}
}

type fakeKube struct {
	pods  [][]Pod
	err   error
	calls int
}

func (k *fakeKube) ListPods(context.Context, string, string) ([]Pod, error) {
	k.calls++
	if k.err != nil {
		return nil, k.err
	}
	i := k.calls - 1
	if i >= len(k.pods) {
		i = len(k.pods) - 1
	}
	return k.pods[i], nil
}

func postConfig() PostConfig {
	return PostConfig{Actor: testActor, Generation: testGeneration, RuntimeRole: "runtime", Namespace: "thinpost-control", Selector: "app.kubernetes.io/name=thinpost-operator", Container: "control", Image: "img@sha256:new", Replicas: 2, RolloutTimeout: 50 * time.Millisecond, Poll: time.Millisecond}
}

func readyPods(image string, n int) []Pod {
	pods := make([]Pod, n)
	for i := range pods {
		pods[i] = Pod{Phase: "Running", Ready: true, Images: map[string]string{"control": image}}
	}
	return pods
}

func closedByDeploy() *fakeDB {
	f := newFake()
	f.version = 28
	f.status = postgres.GateStatus{Mode: "closed", Generation: testGeneration, ChangedBy: testActor, Revision: 7}
	return f
}

func TestPostReopensAfterRollout(t *testing.T) {
	f := closedByDeploy()
	kube := &fakeKube{pods: [][]Pod{append(readyPods("img@sha256:new", 2), Pod{Phase: "Running", Ready: true, Images: map[string]string{"control": "img@sha256:old"}}), readyPods("img@sha256:new", 2)}}
	result, err := RunPost(context.Background(), f, kube, postConfig())
	if err != nil || result.Action != "reopened" || result.Revision != 8 || kube.calls != 2 || f.reopenedAt != 28 || f.failbackRev != 7 {
		t.Fatal(result, err, kube.calls, f.calls)
	}
	if strings.Join(f.calls, ",") != "checkrole,reopen" {
		t.Fatal(f.calls)
	}
}

func TestPostLeavesUnownedGateAlone(t *testing.T) {
	for _, status := range []postgres.GateStatus{
		{Mode: "closed", Generation: testGeneration, ChangedBy: "alice", Revision: 7},
		{Mode: "closed", Generation: testGeneration, ChangedBy: testPrefix + "r6", Revision: 7},
		{Mode: "open", Generation: testGeneration, ChangedBy: testActor, Revision: 8},
		{Mode: "recovery", Generation: testGeneration, ChangedBy: testActor, Revision: 8},
	} {
		f := closedByDeploy()
		f.status = status
		kube := &fakeKube{err: errors.New("must not be called")}
		result, err := RunPost(context.Background(), f, kube, postConfig())
		if err != nil || result.Action != "not_owned" || kube.calls != 0 || f.called("reopen") {
			t.Fatal(status, result, err)
		}
	}
}

func TestPostFailuresLeaveGateClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeDB, *fakeKube)
		want   string
	}{
		{"schema behind", func(f *fakeDB, _ *fakeKube) { f.version = 27 }, "schema_not_current"},
		{"generation", func(f *fakeDB, _ *fakeKube) { f.status.Generation = "" }, "generation_mismatch"},
		{"old pod stays", func(_ *fakeDB, k *fakeKube) {
			k.pods = [][]Pod{append(readyPods("img@sha256:new", 2), Pod{Phase: "Running", Images: map[string]string{"control": "old"}})}
		}, "rollout_timeout"},
		{"api down", func(_ *fakeDB, k *fakeKube) { k.err = errors.New("down") }, "kubernetes_unavailable"},
		{"privileges", func(f *fakeDB, _ *fakeKube) { f.roleErr = errors.New("bad") }, "runtime_privileges_invalid"},
		{"reopen conflict", func(f *fakeDB, _ *fakeKube) { f.reopenErr = postgres.ErrDeployGateConflict }, "gate_reopen_failed"},
	}
	for _, c := range cases {
		f := closedByDeploy()
		kube := &fakeKube{pods: [][]Pod{readyPods("img@sha256:new", 2)}}
		c.mutate(f, kube)
		_, err := RunPost(context.Background(), f, kube, postConfig())
		if reason(err) != c.want {
			t.Errorf("%s: %v", c.name, err)
		}
		if c.want != "gate_reopen_failed" && f.status.Mode != "closed" {
			t.Errorf("%s: gate opened", c.name)
		}
	}
}

func TestActorPrefix(t *testing.T) {
	if ActorPrefix(testActor) != testPrefix || ActorPrefix("nocolon") != "" {
		t.Fatal(ActorPrefix(testActor))
	}
}

func TestFailureTextCarriesNoCauseText(t *testing.T) {
	err := fail("migrate_failed", errors.New("password=hunter2 subject=secret"))
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}
