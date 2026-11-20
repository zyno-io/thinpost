package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/bounceaddress"
	"github.com/zyno-io/thinpost/internal/bounceingest"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/webhook"
)

type objectVerifierFunc func(context.Context, string) error

func (f objectVerifierFunc) VerifyPresent(ctx context.Context, key string) error { return f(ctx, key) }

func fixtureObjectKey(kind, id string) string {
	key, err := objectkey.New(kind, id, time.Now())
	if err != nil {
		panic(err)
	}
	return key
}

func TestPostgresAcceptanceIntegration(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set THINPOST_TEST_DATABASE_URL to a disposable empty PostgreSQL database")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	const testGeneration = "00000000000000000000000000000aaa"
	if _, err = db.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, testGeneration); err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte("test-key"))
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name,recipient_daily_limit,pending_message_limit) VALUES ('a','A',1000,1000),('b','B',1000,1000); INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ('a','example.com',true,now()),('b','example.com',true,now());`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO api_keys (id, digest, app_id) VALUES ('key-a',$1,'a')`, key[:]); err != nil {
		t.Fatal(err)
	}
	runtimeDB, cleanupRuntime := restrictedRuntimeForTest(t, ctx, db, dsn)
	defer cleanupRuntime()
	collectorDB, cleanupCollector := restrictedCollectorForTest(t, ctx, db, dsn)
	defer cleanupCollector()
	present := objectVerifierFunc(func(context.Context, string) error { return nil })
	repo := &Repository{DB: runtimeDB, Generation: testGeneration, Objects: present}
	collectorRepo := &Repository{DB: collectorDB, Generation: testGeneration}
	t.Run("admission readiness does not require a worker", func(t *testing.T) {
		if err := repo.CheckAdmission(ctx); err != nil {
			t.Fatal("open admission unavailable without a worker", err)
		}
		other := &Repository{DB: runtimeDB, Generation: "00000000000000000000000000000aab"}
		if err := other.CheckAdmission(ctx); !errors.Is(err, ErrRuntimeClosed) {
			t.Fatal("wrong deployment generation reported ready", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE runtime_gate SET mode='closed' WHERE singleton`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := db.ExecContext(ctx, `UPDATE runtime_gate SET mode='open' WHERE singleton`); err != nil {
				t.Error(err)
			}
		}()
		if err := repo.CheckAdmission(ctx); !errors.Is(err, ErrRuntimeClosed) {
			t.Fatal("closed admission reported ready", err)
		}
	})
	t.Run("retry policy is console-editable, revisioned and audited", func(t *testing.T) {
		status, err := InspectRetryPolicy(ctx, db)
		if err != nil || status.Policy != delivery.DefaultRetryPolicy() || status.Revision != 1 || status.ChangedBy != "migration:033" {
			t.Fatal("initial policy", status, err)
		}
		defer func() {
			_, restoreErr := db.ExecContext(context.Background(), `UPDATE delivery_retry_policy SET max_attempts=128,initial_ms=1800000,max_delay_ms=10800000,max_age_ms=432000000,revision=1,changed_by='migration:033' WHERE singleton;
				DELETE FROM audit_events WHERE action='retry_policy.updated'`)
			if restoreErr != nil {
				t.Error(restoreErr)
			}
		}()
		update := admin.RetryPolicyUpdate{MaxAttempts: 20, InitialDelayMS: 60000, MaxDelayMS: 3600000, MaxAgeMS: 86400000, ExpectedRevision: 1}
		if _, err = repo.SetRetryPolicy(ctx, "operator", admin.RetryPolicyUpdate{MaxAttempts: 0, InitialDelayMS: 60000, MaxDelayMS: 3600000, MaxAgeMS: 86400000, ExpectedRevision: 1}); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatal("invalid policy accepted", err)
		}
		// A transaction that holds the old policy keeps it; the update does not wait for it.
		reader, err := repo.beginRuntime(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Rollback()
		got, err := repo.SetRetryPolicy(ctx, "operator", update)
		if err != nil || got.Revision != 2 || got.MaxAttempts != 20 || got.ChangedBy != "operator" {
			t.Fatal("update failed", got, err)
		}
		if old, err := deliveryRetryPolicy(ctx, reader); err != nil || old.MaxAttempts != 20 {
			t.Fatal("read committed reader did not see the committed policy", old, err)
		}
		if err = reader.Rollback(); err != nil {
			t.Fatal(err)
		}
		if _, err = repo.SetRetryPolicy(ctx, "operator", update); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("stale revision accepted", err)
		}
		same := update
		same.ExpectedRevision = 2
		if got, err = repo.SetRetryPolicy(ctx, "operator", same); err != nil || got.Revision != 2 {
			t.Fatal("identical values were not a no-op", got, err)
		}
		results := make(chan error, 2)
		for _, attempts := range []int{21, 22} {
			next := same
			next.MaxAttempts = attempts
			go func() { _, err := repo.SetRetryPolicy(ctx, "operator", next); results <- err }()
		}
		first, second := <-results, <-results
		if !((first == nil && errors.Is(second, admin.ErrConflict)) || (second == nil && errors.Is(first, admin.ErrConflict))) {
			t.Fatal("concurrent updates not serialized", first, second)
		}
		var events int
		var leaked bool
		if err = db.QueryRowContext(ctx, `SELECT count(*),COALESCE(bool_or(app_id IS NOT NULL OR resource_id<>'delivery_retry_policy' OR details->'old'->>'revision' IS NULL),false) FROM audit_events WHERE action='retry_policy.updated'`).Scan(&events, &leaked); err != nil || events != 2 || leaked {
			t.Fatal("incorrect policy audit", events, leaked, err)
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM delivery_retry_policy_events`).Scan(&events); err != nil || events != 0 {
			t.Fatal("historical events table received writes", events, err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE runtime_gate SET mode='closed' WHERE singleton`); err != nil {
			t.Fatal(err)
		}
		_, err = repo.SetRetryPolicy(ctx, "operator", admin.RetryPolicyUpdate{MaxAttempts: 30, InitialDelayMS: 60000, MaxDelayMS: 3600000, MaxAgeMS: 86400000, ExpectedRevision: 3})
		if _, restore := db.ExecContext(ctx, `UPDATE runtime_gate SET mode='open' WHERE singleton`); restore != nil {
			t.Fatal(restore)
		}
		if !errors.Is(err, ErrRuntimeClosed) {
			t.Fatal("policy changed under a closed gate", err)
		}
	})
	t.Run("submission burst is shared across replicas", func(t *testing.T) {
		testSubmissionBurst(t, ctx, db, repo)
	})
	t.Run("object reference lock fences collection and missing objects roll back", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		key := fixtureObjectKey("submissions", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeee1")
		defer func() {
			_, err := db.ExecContext(context.Background(), `DELETE FROM idempotency_keys WHERE app_id='a' AND key='object-fence'`)
			if err != nil {
				t.Error("remove object fence fixture:", err)
			}
			_, err = db.ExecContext(context.Background(), `WITH gone AS (DELETE FROM message_recipients WHERE message_id IN (SELECT id FROM messages WHERE object_key=$1)), removed AS (DELETE FROM messages WHERE object_key=$1 RETURNING 1) UPDATE apps SET recipient_quota_used=recipient_quota_used-(SELECT count(*) FROM removed) WHERE id='a'`, key)
			if err != nil {
				t.Error("remove object fence message:", err)
			}
		}()
		record := withMetadata(acceptance.Record{AppID: "a", KeyID: "key-a", ObjectKey: key, From: "sender@example.com", Adapter: "resend", IdempotencyKey: "object-fence", RecipientCount: 1})
		entered := make(chan struct{})
		release := make(chan struct{})
		writer := &Repository{DB: runtimeDB, Generation: testGeneration, Objects: objectVerifierFunc(func(ctx context.Context, _ string) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})}
		result := make(chan error, 1)
		go func() { _, err := writer.Commit(ctx, record); result <- err }()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("writer did not reach object verification")
		}
		lock, err := managedObjectLock(key)
		if err != nil {
			t.Fatal(err)
		}
		collector, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer collector.Rollback()
		var acquired bool
		if err = collector.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1::integer,$2::integer)`, lock.first, lock.second).Scan(&acquired); err != nil || acquired {
			t.Fatal("collector acquired writer's object lock", acquired, err)
		}
		close(release)
		if err = <-result; err != nil {
			t.Fatal("writer failed", err)
		}
		if err = collector.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1::integer,$2::integer)`, lock.first, lock.second).Scan(&acquired); err != nil || !acquired {
			t.Fatal("collector could not acquire released lock", acquired, err)
		}
		var referenced bool
		if err = collector.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE object_key=$1)`, key).Scan(&referenced); err != nil || !referenced {
			t.Fatal("collector did not observe committed reference", referenced, err)
		}
		if err = collector.Commit(); err != nil {
			t.Fatal(err)
		}
		// Model the collector winning the lock and deleting an unreferenced
		// object while a writer is waiting to publish its reference.
		record.ObjectKey = fixtureObjectKey("submissions", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeee2")
		record.IdempotencyKey = "missing-object"
		lock, err = managedObjectLock(record.ObjectKey)
		if err != nil {
			t.Fatal(err)
		}
		collector, err = db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer collector.Rollback()
		if _, err = collector.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1::integer,$2::integer)`, lock.first, lock.second); err != nil {
			t.Fatal(err)
		}
		writer.Objects = objectVerifierFunc(func(context.Context, string) error { return errors.New("missing") })
		go func() { _, err := writer.Commit(ctx, record); result <- err }()
		for {
			var waiting bool
			err = db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory' AND classid=$1::bigint::oid AND objid=$2::bigint::oid AND objsubid=2 AND NOT granted)`, int64(uint32(lock.first)), int64(uint32(lock.second))).Scan(&waiting)
			if err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("writer did not wait for collector")
			case <-time.After(10 * time.Millisecond):
			}
		}
		var before int
		if err = db.QueryRowContext(ctx, `SELECT recipient_quota_used FROM apps WHERE id='a'`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err = collector.Commit(); err != nil {
			t.Fatal(err)
		}
		if err = <-result; err == nil {
			t.Fatal("missing source object committed")
		}
		var after, references, receipts int
		if err = db.QueryRowContext(ctx, `SELECT recipient_quota_used, (SELECT count(*) FROM messages WHERE object_key=$1), (SELECT count(*) FROM idempotency_keys WHERE app_id='a' AND key='missing-object') FROM apps WHERE id='a'`, record.ObjectKey).Scan(&after, &references, &receipts); err != nil || after != before || references != 0 || receipts != 0 {
			t.Fatal("missing object did not roll back transaction", before, after, references, receipts, err)
		}
		// A failed DELETE may finish after its lock is released. Even if HEAD
		// still sees the old object, an expired key must never gain a reference.
		record.ObjectKey, err = objectkey.New("submissions", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeee3", time.Now().Add(-2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		record.IdempotencyKey = "expired-object"
		writer.Objects = present
		if _, err = writer.Commit(ctx, record); err == nil {
			t.Fatal("expired object gained a reference after an ambiguous delete")
		}
		if err = db.QueryRowContext(ctx, `SELECT recipient_quota_used, (SELECT count(*) FROM messages WHERE object_key=$1), (SELECT count(*) FROM idempotency_keys WHERE app_id='a' AND key='expired-object') FROM apps WHERE id='a'`, record.ObjectKey).Scan(&after, &references, &receipts); err != nil || after != before || references != 0 || receipts != 0 {
			t.Fatal("expired object did not roll back transaction", before, after, references, receipts, err)
		}
	})
	t.Run("runtime forces read committed snapshots", func(t *testing.T) {
		isolated, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer isolated.Close()
		isolated.SetMaxOpenConns(1)
		if _, err = isolated.ExecContext(ctx, `SET default_transaction_isolation='repeatable read'`); err != nil {
			t.Fatal(err)
		}
		tx, err := (&Repository{DB: isolated, Generation: testGeneration}).beginRuntime(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var isolation string
		if err = tx.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil || isolation != "read committed" {
			t.Fatal("runtime inherited an incompatible isolation level", isolation, err)
		}
	})
	t.Run("recipient quotas are atomic and idempotent", func(t *testing.T) {
		const id = "ffffffffffffffffffffffffffffff01"
		if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'quota')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, id); err != nil {
			t.Fatal(err)
		}
		key, err := repo.IssueKey(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		record := withMetadata(acceptance.Record{AppID: id, KeyID: key.ID, ObjectKey: fixtureObjectKey("submissions", "ffffffffffffffffffffffffffffff02"), From: "sender@example.com", Adapter: "resend", IdempotencyKey: "same", RequestHash: sha256.Sum256([]byte("quota")), RecipientCount: 2})
		if _, err = repo.Commit(ctx, record); !errors.Is(err, submission.ErrQuotaExceeded) {
			t.Fatal("new app quota should be closed", err)
		}
		quota, err := repo.SetRecipientQuota(ctx, "operator", id, 5)
		if err != nil || quota.DailyLimit != 5 || quota.Used != 0 {
			t.Fatal("quota setup failed", quota, err)
		}
		if _, err = repo.SetPendingQuota(ctx, "operator", id, 100); err != nil {
			t.Fatal("pending quota setup failed", err)
		}
		first, err := repo.Commit(ctx, record)
		if err != nil {
			t.Fatal(err)
		}
		record.ObjectKey = fixtureObjectKey("submissions", "ffffffffffffffffffffffffffffff03")
		repeat, err := repo.Commit(ctx, record)
		if err != nil || !repeat.Replayed || repeat.ID != first.ID || !repeat.AcceptedAt.Equal(first.AcceptedAt) {
			t.Fatal("idempotent retry failed", err)
		}
		var wg sync.WaitGroup
		outcomes := make(chan error, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r := record
				r.IdempotencyKey = ""
				r.ObjectKey = fixtureObjectKey("submissions", fmt.Sprintf("%032x", i+10000))
				r.RecipientCount = 1
				r.Recipients = r.Recipients[:1]
				r.RecipientKinds = r.RecipientKinds[:1]
				_, err := repo.Commit(ctx, r)
				outcomes <- err
			}(i)
		}
		wg.Wait()
		close(outcomes)
		accepted, rejected := 0, 0
		for err := range outcomes {
			if err == nil {
				accepted++
			} else if errors.Is(err, submission.ErrQuotaExceeded) {
				rejected++
			} else {
				t.Fatal(err)
			}
		}
		quota, err = repo.GetRecipientQuota(ctx, id)
		if err != nil || accepted != 3 || rejected != 9 || quota.Used != 5 {
			t.Fatal("concurrent quota exceeded", accepted, rejected, quota, err)
		}
		if _, err = repo.Commit(ctx, record); err != nil {
			t.Fatal("idempotent retry blocked at quota", err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE apps SET recipient_quota_day=(statement_timestamp() AT TIME ZONE 'UTC')::date-1 WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		quota, err = repo.GetRecipientQuota(ctx, id)
		if err != nil || quota.Used != 0 {
			t.Fatal("quota did not reset at UTC day boundary", quota, err)
		}
		var audits int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='app.recipient_quota.updated' AND details->>'daily_limit'='5' AND details->>'previous_daily_limit'='0'`, id).Scan(&audits); err != nil || audits != 1 {
			t.Fatal("quota audit missing", err)
		}
		record.IdempotencyKey = ""
		record.ObjectKey = fixtureObjectKey("submissions", "ffffffffffffffffffffffffffffff04")
		if _, err = repo.Commit(ctx, record); err != nil {
			t.Fatal("quota did not reopen for the new UTC day", err)
		}
		quota, err = repo.SetRecipientQuota(ctx, "operator", id, 1)
		if err != nil || quota.Used != 2 {
			t.Fatal("changing limit reset usage", quota, err)
		}
		record.ObjectKey = fixtureObjectKey("submissions", "ffffffffffffffffffffffffffffff05")
		if _, err = repo.Commit(ctx, record); !errors.Is(err, submission.ErrQuotaExceeded) {
			t.Fatal("lower limit did not take effect", err)
		}
		if _, err = db.ExecContext(ctx, `CREATE FUNCTION fail_quota_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$; CREATE TRIGGER fail_quota_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_quota_audit()`); err != nil {
			t.Fatal(err)
		}
		if _, err = repo.SetRecipientQuota(ctx, "operator", id, 10); err == nil {
			t.Fatal("quota changed without audit")
		}
		if _, err = db.ExecContext(ctx, `DROP TRIGGER fail_quota_audit ON audit_events; DROP FUNCTION fail_quota_audit()`); err != nil {
			t.Fatal(err)
		}
		quota, err = repo.GetRecipientQuota(ctx, id)
		if err != nil || quota.DailyLimit != 1 {
			t.Fatal("failed audit did not roll back quota", quota, err)
		}
	})
	t.Run("pending message quota is atomic and holds uncertain work", func(t *testing.T) {
		const appID = "ffffffffffffffffffffffffffffff20"
		if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name,recipient_daily_limit) VALUES ($1,'pending',100)`, appID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, appID); err != nil {
			t.Fatal(err)
		}
		key, err := repo.IssueKey(ctx, appID)
		if err != nil {
			t.Fatal(err)
		}
		record := withMetadata(acceptance.Record{AppID: appID, KeyID: key.ID, ObjectKey: fixtureObjectKey("submissions", "ffffffffffffffffffffffffffffff21"), From: "sender@example.com", Adapter: "resend", IdempotencyKey: "same", RequestHash: sha256.Sum256([]byte("pending")), RecipientCount: 1})
		if _, err = repo.Commit(ctx, record); !errors.Is(err, submission.ErrBacklogExceeded) {
			t.Fatal("new app backlog should be closed", err)
		}
		daily, err := repo.GetRecipientQuota(ctx, appID)
		if err != nil || daily.Used != 0 {
			t.Fatal("rejected backlog consumed daily quota", daily, err)
		}
		quota, err := repo.SetPendingQuota(ctx, "operator", appID, 3)
		if err != nil || quota.Limit != 3 || quota.Pending != 0 || quota.Truncated {
			t.Fatal("pending quota setup failed", quota, err)
		}
		first, err := repo.Commit(ctx, record)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		outcomes := make(chan error, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				candidate := record
				candidate.IdempotencyKey = ""
				candidate.ObjectKey = fixtureObjectKey("submissions", fmt.Sprintf("%032x", i+20000))
				_, err := repo.Commit(ctx, candidate)
				outcomes <- err
			}(i)
		}
		wg.Wait()
		close(outcomes)
		accepted, rejected := 0, 0
		for err := range outcomes {
			if err == nil {
				accepted++
			} else if errors.Is(err, submission.ErrBacklogExceeded) {
				rejected++
			} else {
				t.Fatal(err)
			}
		}
		quota, err = repo.GetPendingQuota(ctx, appID)
		if err != nil || accepted != 2 || rejected != 10 || quota.Pending != 3 {
			t.Fatal("concurrent pending limit exceeded", accepted, rejected, quota, err)
		}
		record.ObjectKey = fixtureObjectKey("submissions", "ffffffffffffffffffffffffffffff22")
		if again, err := repo.Commit(ctx, record); err != nil || !again.Replayed || again.ID != first.ID || !again.AcceptedAt.Equal(first.AcceptedAt) {
			t.Fatal("idempotent retry was blocked at pending limit", err)
		}
		daily, err = repo.GetRecipientQuota(ctx, appID)
		if err != nil || daily.Used != 3 {
			t.Fatal("rejected backlog altered daily quota", daily, err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE messages SET state='prepared' WHERE id=$1`, first.ID); err != nil {
			t.Fatal(err)
		}
		quota, err = repo.GetPendingQuota(ctx, appID)
		if err != nil || quota.Pending != 3 {
			t.Fatal("prepared message without deliveries did not stay pending", quota, err)
		}
		const deliveryID = "ffffffffffffffffffffffffffffff23"
		const attemptID = "ffffffffffffffffffffffffffffff24"
		if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ($2,$1,0,'final/pending-test','uncertain')`, first.ID, deliveryID); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ('pending-test-delivery-2',$1,1,'final/pending-test-2','accepted')`, first.ID); err != nil {
			t.Fatal(err)
		}
		quota, err = repo.GetPendingQuota(ctx, appID)
		if err != nil || quota.Pending != 3 {
			t.Fatal("uncertain delivery did not remain pending", quota, err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE deliveries SET state='in_flight',current_attempt_id=$2 WHERE id=$1`, deliveryID, attemptID); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized) VALUES ($1,$2,'worker',1,false)`, attemptID, deliveryID); err != nil {
			t.Fatal(err)
		}
		result := delivery.Result{AttemptID: attemptID, DeliveryID: deliveryID, WorkerID: "worker", Epoch: 1, Outcome: delivery.OutcomeNullMX}
		if err = repo.CommitDeliveryResult(ctx, result); err != nil {
			t.Fatal(err)
		}
		quota, err = repo.GetPendingQuota(ctx, appID)
		if err != nil || quota.Pending != 2 {
			t.Fatal("terminal delivery did not release pending capacity", quota, err)
		}
		quota, err = repo.SetPendingQuota(ctx, "operator", appID, 1)
		if err != nil || quota.Pending != 2 || quota.Limit != 1 {
			t.Fatal("lowering quota discarded pending work", quota, err)
		}
		record.IdempotencyKey = ""
		if _, err = repo.Commit(ctx, record); !errors.Is(err, submission.ErrBacklogExceeded) {
			t.Fatal("lowered quota did not hold new work", err)
		}
		var audits int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='app.pending_quota.updated'`, appID).Scan(&audits); err != nil || audits != 2 {
			t.Fatal("pending quota updates were not audited", err)
		}
		if _, err = db.ExecContext(ctx, `CREATE FUNCTION fail_pending_quota_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$; CREATE TRIGGER fail_pending_quota_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_pending_quota_audit()`); err != nil {
			t.Fatal(err)
		}
		if _, err = repo.SetPendingQuota(ctx, "operator", appID, 10); err == nil {
			t.Fatal("pending quota changed without audit")
		}
		if _, err = db.ExecContext(ctx, `DROP TRIGGER fail_pending_quota_audit ON audit_events; DROP FUNCTION fail_pending_quota_audit()`); err != nil {
			t.Fatal(err)
		}
		quota, err = repo.GetPendingQuota(ctx, appID)
		if err != nil || quota.Limit != 1 {
			t.Fatal("failed audit did not roll back pending quota", quota, err)
		}
	})
	t.Run("concurrent final deliveries release exactly one pending message", func(t *testing.T) {
		const appID = "ffffffffffffffffffffffffffffff30"
		if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name,pending_message_limit) VALUES ($1,'concurrent finals',100)`, appID); err != nil {
			t.Fatal(err)
		}
		var runtimeUser string
		if err := runtimeDB.QueryRowContext(ctx, `SELECT current_user`).Scan(&runtimeUser); err != nil {
			t.Fatal(err)
		}
		for iteration := 0; iteration < 20; iteration++ {
			messageID := fmt.Sprintf("%032x", 300000+iteration*5)
			firstDelivery := fmt.Sprintf("%032x", 300001+iteration*5)
			secondDelivery := fmt.Sprintf("%032x", 300002+iteration*5)
			firstAttempt := fmt.Sprintf("%032x", 300003+iteration*5)
			secondAttempt := fmt.Sprintf("%032x", 300004+iteration*5)
			if _, err := db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,$3,false,'prepared')`, messageID, appID, "submissions/"+messageID); err != nil {
				t.Fatal(err)
			}
			for index, entry := range []struct{ delivery, attempt string }{{firstDelivery, firstAttempt}, {secondDelivery, secondAttempt}} {
				if _, err := db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,current_attempt_id) VALUES ($1,$2,$3,$4,'in_flight',$5)`, entry.delivery, messageID, index, "final/"+entry.delivery, entry.attempt); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized) VALUES ($1,$2,'worker',1,false)`, entry.attempt, entry.delivery); err != nil {
					t.Fatal(err)
				}
			}
			results := []delivery.Result{{AttemptID: firstAttempt, DeliveryID: firstDelivery, WorkerID: "worker", Epoch: 1, Outcome: delivery.OutcomeNullMX}, {AttemptID: secondAttempt, DeliveryID: secondDelivery, WorkerID: "worker", Epoch: 1, Outcome: delivery.OutcomeNullMX}}
			var parentLock *sql.Tx
			if iteration == 0 {
				parentLock, err = db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer parentLock.Rollback()
				var locked string
				if err = parentLock.QueryRowContext(ctx, `SELECT id FROM messages WHERE id=$1 FOR UPDATE`, messageID).Scan(&locked); err != nil {
					t.Fatal(err)
				}
			}
			start := make(chan struct{})
			done := make(chan error, len(results))
			for _, result := range results {
				go func(result delivery.Result) {
					<-start
					done <- repo.CommitDeliveryResult(ctx, result)
				}(result)
			}
			close(start)
			if parentLock != nil {
				// Both transactions update their own delivery, then wait on the
				// parent. This forces the overlapping-finalization interleaving.
				deadline := time.Now().Add(5 * time.Second)
				blocked := 0
				for time.Now().Before(deadline) {
					if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock' AND query LIKE 'SELECT m.id FROM messages m JOIN deliveries d ON d.message_id=m.id%'`, runtimeUser).Scan(&blocked); err != nil {
						t.Fatal(err)
					}
					if blocked == 2 {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if blocked != 2 {
					t.Fatal("could not force concurrent sibling completion")
				}
				if err = parentLock.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			for range results {
				if err := <-done; err != nil {
					t.Fatal("concurrent result failed", err)
				}
			}
			var pending bool
			if err := db.QueryRowContext(ctx, `SELECT pending FROM messages WHERE id=$1`, messageID).Scan(&pending); err != nil || pending {
				t.Fatal("terminal siblings left the message pending", err)
			}
			if err := repo.CommitDeliveryResult(ctx, results[0]); err != nil {
				t.Fatal("duplicate terminal result failed", err)
			}
		}
		quota, err := repo.GetPendingQuota(ctx, appID)
		if err != nil || quota.Pending != 0 {
			t.Fatal("completed messages still consume pending capacity", quota, err)
		}
		const failedMessage = "ffffffffffffffffffffffffffffff31"
		const failedDelivery = "ffffffffffffffffffffffffffffff32"
		const failedAttempt = "ffffffffffffffffffffffffffffff33"
		if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'source/fail-final',false,'prepared')`, failedMessage, appID); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,current_attempt_id) VALUES ($1,$2,0,'final/fail-final','in_flight',$3)`, failedDelivery, failedMessage, failedAttempt); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized) VALUES ($1,$2,'worker',1,false)`, failedAttempt, failedDelivery); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `CREATE FUNCTION fail_pending_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'event unavailable'; END $$; CREATE TRIGGER fail_pending_event BEFORE INSERT ON webhook_events FOR EACH ROW EXECUTE FUNCTION fail_pending_event()`); err != nil {
			t.Fatal(err)
		}
		result := delivery.Result{AttemptID: failedAttempt, DeliveryID: failedDelivery, WorkerID: "worker", Epoch: 1, Outcome: delivery.OutcomeNullMX}
		if err = repo.CommitDeliveryResult(ctx, result); err == nil {
			t.Fatal("result committed without its event")
		}
		if _, err = db.ExecContext(ctx, `DROP TRIGGER fail_pending_event ON webhook_events; DROP FUNCTION fail_pending_event()`); err != nil {
			t.Fatal(err)
		}
		var pending bool
		var state string
		if err = db.QueryRowContext(ctx, `SELECT m.pending,d.state FROM messages m JOIN deliveries d ON d.message_id=m.id WHERE m.id=$1`, failedMessage).Scan(&pending, &state); err != nil || !pending || state != "in_flight" {
			t.Fatal("failed result released pending capacity", err)
		}
	})
	t.Run("signing domain authorization", func(t *testing.T) {
		const app = "00000000000000000000000000000601"
		if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'signing')`, app); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'signing.example',true,now())`, app); err != nil {
			t.Fatal(err)
		}
		if active, err := repo.SigningDomainActive(ctx, app, "signing.example"); err != nil || !active {
			t.Fatal("verified domain rejected", err)
		}
		if active, err := repo.SigningDomainActive(ctx, "00000000000000000000000000000602", "signing.example"); err != nil || active {
			t.Fatal("cross-app domain allowed", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE apps SET active=false WHERE id=$1`, app); err != nil {
			t.Fatal(err)
		}
		if active, err := repo.SigningDomainActive(ctx, app, "signing.example"); err != nil || active {
			t.Fatal("inactive app allowed", err)
		}
	})
	app, err := repo.LookupKey(ctx, key)
	if err != nil || app.ID != "a" || app.KeyID != "key-a" {
		t.Fatalf("auth %q %v", app.ID, err)
	}
	if _, err = repo.LookupKey(ctx, sha256.Sum256([]byte("wrong"))); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatalf("bad key %v", err)
	}
	record := withMetadata(acceptance.Record{AppID: "a", KeyID: "key-a", ObjectKey: fixtureObjectKey("submissions", "00000000000000000000000000000001"), From: "sender@example.com", Adapter: "resend", IdempotencyKey: "request-1", RequestHash: sha256.Sum256([]byte("body")), EnhancedProtection: true, RecipientCount: 1})
	first, err := repo.Commit(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	record.ObjectKey = fixtureObjectKey("submissions", "00000000000000000000000000000002")
	again, err := repo.Commit(ctx, record)
	if err != nil || !again.Replayed || first.ID != again.ID || !first.AcceptedAt.Equal(again.AcceptedAt) {
		t.Fatalf("retry: %v %+v", err, again)
	}
	oldHash := record.RequestHash
	record.RequestHash = sha256.Sum256([]byte("same body under new key"))
	record.PreviousHashes = [][32]byte{oldHash}
	again, err = repo.Commit(ctx, record)
	if err != nil || !again.Replayed || first.ID != again.ID || !first.AcceptedAt.Equal(again.AcceptedAt) {
		t.Fatalf("retry with previous fingerprint key: %v %+v", err, again)
	}
	record.PreviousHashes = nil
	record.RequestHash = sha256.Sum256([]byte("changed"))
	if _, err = repo.Commit(ctx, record); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatalf("conflict: %v", err)
	}
	secondKey, err := repo.IssueKey(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	record.AppID = "b"
	record.KeyID = secondKey.ID
	if _, err = repo.Commit(ctx, record); err != nil {
		t.Fatalf("app scope: %v", err)
	}
	record.AppID = "a"
	record.KeyID = "key-a"
	record.IdempotencyKey = "request-2"
	record.ObjectKey = fixtureObjectKey("submissions", "00000000000000000000000000000003")
	record.SendingDomain = "unauthorized.example"
	if _, err = repo.Commit(ctx, record); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("domain check: %v", err)
	}
	var enhancedProtection bool
	if err = db.QueryRowContext(ctx, `SELECT enhanced_protection FROM messages WHERE id=$1`, first.ID).Scan(&enhancedProtection); err != nil || !enhancedProtection {
		t.Fatal("TLS policy not stored")
	}
	if _, err = db.ExecContext(ctx, `UPDATE idempotency_keys SET expires_at=now()-interval '1 second' WHERE app_id='a' AND key='request-1'`); err != nil {
		t.Fatal(err)
	}
	record.SendingDomain = ""
	record.IdempotencyKey = "request-1"
	if renewed, err := repo.Commit(ctx, record); err != nil || renewed.ID == first.ID {
		t.Fatalf("expired idempotency key: %v", err)
	}

	t.Run("submission metadata is atomic", func(t *testing.T) {
		var subject, senderDomain, senderRev string
		var count int
		if err := db.QueryRowContext(ctx, `SELECT subject,recipient_count,sender_domain,sender_domain_rev FROM messages WHERE id=$1`, first.ID).Scan(&subject, &count, &senderDomain, &senderRev); err != nil || subject != "Fixture subject" || count != 1 || senderDomain != "example.com" || senderRev != "com.example." {
			t.Fatal("message metadata not recorded", err)
		}
		var sender, senderName string
		if err := db.QueryRowContext(ctx, `SELECT sender_address,sender_name FROM messages WHERE id=$1`, first.ID).Scan(&sender, &senderName); err != nil || sender != "sender@example.com" || senderName != "Fixture Sender" {
			t.Fatal("sender not recorded", err)
		}
		var address, lower, domain, rev string
		var rows int
		if err := db.QueryRowContext(ctx, `SELECT count(*),min(address),min(address_lower),min(domain_lower),min(domain_rev) FROM message_recipients WHERE message_id=$1`, first.ID).Scan(&rows, &address, &lower, &domain, &rev); err != nil || rows != 1 || address != "recipient0@Recipient.example" || lower != "recipient0@recipient.example" || domain != "recipient.example" || rev != "example.recipient." {
			t.Fatal("recipient rows not recorded (idempotent replays must add none)", rows, err)
		}
		before := func() (m, r int) {
			if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM messages),(SELECT count(*) FROM message_recipients)`).Scan(&m, &r); err != nil {
				t.Fatal(err)
			}
			return
		}
		m0, r0 := before()
		base := withMetadata(acceptance.Record{AppID: "a", KeyID: "key-a", ObjectKey: fixtureObjectKey("submissions", "ffffffffffffffffffffffffffffff70"), From: "sender@example.com", Adapter: "resend", IdempotencyKey: "metadata-atomic", RequestHash: sha256.Sum256([]byte("metadata")), RecipientCount: 2})
		for name, mutate := range map[string]func(*acceptance.Record){
			"mismatched sender domain": func(r *acceptance.Record) { r.SenderDomain = "other.example" },
			"empty sender address":     func(r *acceptance.Record) { r.SenderAddress = "" },
			"sender address mismatch":  func(r *acceptance.Record) { r.SenderAddress = "x@other.example" },
			"newline in sender name":   func(r *acceptance.Record) { r.SenderName = "a\nb" },
			"oversized sender name":    func(r *acceptance.Record) { r.SenderName = strings.Repeat("n", 999) },
			"empty subject":            func(r *acceptance.Record) { r.Subject = "" },
			"oversized subject":        func(r *acceptance.Record) { r.Subject = strings.Repeat("s", 999) },
			"recipient count mismatch": func(r *acceptance.Record) { r.Recipients = r.Recipients[:1] },
			"empty recipient":          func(r *acceptance.Record) { r.Recipients[1] = "" },
			"NUL in recipient":         func(r *acceptance.Record) { r.Recipients[1] = "a\x00@example.net" },
		} {
			record := base
			record.Recipients = append([]string(nil), base.Recipients...)
			mutate(&record)
			if _, err := repo.Commit(ctx, record); !errors.Is(err, submission.ErrInvalidMessage) {
				t.Fatal("invalid metadata accepted:", name, err)
			}
		}
		if m, r := before(); m != m0 || r != r0 {
			t.Fatal("rejected metadata left rows")
		}
		// A failing recipient insert rolls back the message and the idempotency key.
		if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_recipient_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$;
			CREATE TRIGGER fail_recipient_insert BEFORE INSERT ON message_recipients FOR EACH ROW EXECUTE FUNCTION fail_recipient_insert()`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fail_recipient_insert ON message_recipients; DROP FUNCTION IF EXISTS fail_recipient_insert()`); err != nil {
				t.Error(err)
			}
		}()
		if _, err := repo.Commit(ctx, base); err == nil {
			t.Fatal("commit succeeded despite failing recipient insert")
		}
		if m, r := before(); m != m0 || r != r0 {
			t.Fatal("failed recipient insert left a message or recipient")
		}
		var keys int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys WHERE app_id='a' AND key='metadata-atomic'`).Scan(&keys); err != nil || keys != 0 {
			t.Fatal("failed recipient insert left an idempotency key", keys, err)
		}
	})

	var wg sync.WaitGroup
	results := make(chan submission.Receipt, 12)
	failures := make(chan error, 12)
	rotationA := sha256.Sum256([]byte("old key fingerprint"))
	rotationB := sha256.Sum256([]byte("new key fingerprint"))
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := record
			r.IdempotencyKey = "concurrent"
			r.ObjectKey = fixtureObjectKey("submissions", fmt.Sprintf("%032x", i+100))
			r.RequestHash, r.PreviousHashes = rotationA, [][32]byte{rotationB}
			if i%2 != 0 {
				r.RequestHash, r.PreviousHashes = rotationB, [][32]byte{rotationA}
			}
			got, err := repo.Commit(ctx, r)
			if err != nil {
				failures <- err
				return
			}
			results <- got
		}(i)
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Errorf("concurrent commit: %v", err)
	}
	var id string
	for got := range results {
		if id == "" {
			id = got.ID
		}
		if got.ID != id {
			t.Error("concurrent idempotency created multiple receipts")
		}
	}
	t.Run("bounded idempotency cleanup", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			if _, err := db.ExecContext(ctx, `INSERT INTO idempotency_keys(app_id,adapter,key,request_hash,message_id,expires_at) VALUES ('a','resend',$1,$2,$3,clock_timestamp()-interval '1 second')`, fmt.Sprintf("expired-%d", i), record.RequestHash[:], first.ID); err != nil {
				t.Fatal(err)
			}
		}
		if count, err := repo.CleanupExpiredIdempotency(ctx, 2); err != nil || count != 2 {
			t.Fatal("cleanup did not respect limit", count, err)
		}
		if count, err := repo.CleanupExpiredIdempotency(ctx, 2); err != nil || count != 1 {
			t.Fatal("cleanup did not remove remaining expired marker", count, err)
		}
		var active int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys WHERE app_id='a' AND key='concurrent'`).Scan(&active); err != nil || active != 1 {
			t.Fatal("active marker removed", err)
		}
	})
	t.Run("preparation claims", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `UPDATE messages SET state='prepared'; INSERT INTO messages(id,app_id,object_key,enhanced_protection,sender_domain,sender_domain_rev,subject,recipient_count) VALUES ('prepare-test','a','submissions/source',true,'example.com','com.example.','s',1)`); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		claims := make(chan preparation.Claim, 8)
		failures := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				claim, err := repo.ClaimPreparation(ctx, fmt.Sprintf("%032x", i+1000), 30*time.Second)
				if err != nil {
					failures <- err
					return
				}
				claims <- claim
			}(i)
		}
		wg.Wait()
		close(claims)
		close(failures)
		var old preparation.Claim
		count := 0
		for claim := range claims {
			old = claim
			count++
		}
		if count != 1 {
			t.Fatalf("%d simultaneous claims", count)
		}
		for err := range failures {
			if !errors.Is(err, preparation.ErrNoWork) {
				t.Error(err)
			}
		}
		if _, err := db.ExecContext(ctx, `UPDATE messages SET prepare_until=clock_timestamp()-interval '1 second' WHERE id='prepare-test'`); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.ClaimPreparation(ctx, "0000000000000000000000000000fffe", 30*time.Second); !errors.Is(err, preparation.ErrNoWork) {
			t.Fatal("expired claim was not rescheduled with backoff", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE messages SET prepare_next_at=clock_timestamp()-interval '1 second' WHERE id='prepare-test'`); err != nil {
			t.Fatal(err)
		}
		current, err := repo.ClaimPreparation(ctx, "0000000000000000000000000000ffff", 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		deliveries := []preparation.Delivery{{ID: "00000000000000000000000000000011", RecipientIndex: 0, ObjectKey: fixtureObjectKey("final", "00000000000000000000000000000011")}, {ID: "00000000000000000000000000000012", RecipientIndex: 1, ObjectKey: fixtureObjectKey("final", "00000000000000000000000000000012")}}
		old.SigningDomain = "example.com"
		if err = repo.CommitPreparation(ctx, old, deliveries); !errors.Is(err, preparation.ErrLostLease) {
			t.Fatalf("stale claim: %v", err)
		}
		current.SigningDomain = "example.com"
		expired := append([]preparation.Delivery(nil), deliveries...)
		expired[0].ObjectKey, err = objectkey.New("final", expired[0].ID, time.Now().Add(-2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err = repo.CommitPreparation(ctx, current, expired); err == nil {
			t.Fatal("expired final object gained a reference")
		}
		repo.Objects = objectVerifierFunc(func(context.Context, string) error { return errors.New("missing") })
		if err = repo.CommitPreparation(ctx, current, deliveries); err == nil {
			t.Fatal("missing final object committed")
		}
		repo.Objects = objectVerifierFunc(func(ctx context.Context, _ string) error {
			_, err := db.ExecContext(ctx, `UPDATE messages SET prepare_until=clock_timestamp()-interval '1 second' WHERE id='prepare-test'`)
			return err
		})
		if err = repo.CommitPreparation(ctx, current, deliveries); !errors.Is(err, preparation.ErrLostLease) {
			t.Fatal("lease expired during object verification but delivery was committed", err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE messages SET prepare_until=clock_timestamp()+interval '30 seconds' WHERE id='prepare-test'`); err != nil {
			t.Fatal(err)
		}
		repo.Objects = present
		if _, err = db.ExecContext(ctx, `UPDATE sender_domains SET active=false WHERE app_id='a' AND domain='example.com'`); err != nil {
			t.Fatal(err)
		}
		if err = repo.CommitPreparation(ctx, current, deliveries); err == nil {
			t.Fatal("revoked domain queued delivery")
		}
		if _, err = db.ExecContext(ctx, `UPDATE sender_domains SET active=true WHERE app_id='a' AND domain='example.com'`); err != nil {
			t.Fatal(err)
		}
		if err = repo.CommitPreparation(ctx, current, deliveries); err != nil {
			t.Fatal(err)
		}
		var countRows int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE message_id='prepare-test'`).Scan(&countRows); err != nil || countRows != 2 {
			t.Fatalf("deliveries %d: %v", countRows, err)
		}
		if err = repo.CommitPreparation(ctx, current, deliveries); !errors.Is(err, preparation.ErrLostLease) {
			t.Fatal("committed same claim twice")
		}
		if _, err = repo.FailPreparation(ctx, current, "queue_commit"); !errors.Is(err, preparation.ErrLostLease) {
			t.Fatal("ambiguous commit was allowed to requeue published deliveries", err)
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE message_id='prepare-test'`).Scan(&countRows); err != nil || countRows != 2 {
			t.Fatal("published deliveries changed after stale failure", countRows, err)
		}
	})
	t.Run("bounded preparation retries and operator recovery", func(t *testing.T) {
		const appID = "00000000000000000000000000000f01"
		const messageID = "00000000000000000000000000000f02"
		const hookID = "00000000000000000000000000000f03"
		if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'preparation retries')`, appID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext,active) VALUES ($1,$2,'https://hooks.example/','\x01'::bytea,true)`, hookID, appID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,sender_domain,sender_domain_rev,subject,recipient_count) VALUES ($1,$2,'submissions/preparation',true,'example.com','com.example.','s',1)`, messageID, appID); err != nil {
			t.Fatal(err)
		}
		var last preparation.Claim
		for i := 1; i <= maxPreparationAttempts; i++ {
			claim, err := repo.ClaimPreparation(ctx, fmt.Sprintf("%032x", 5000+i), 30*time.Second)
			if err != nil || claim.MessageID != messageID {
				t.Fatal("preparation claim", i, claim, err)
			}
			last = claim
			if i == maxPreparationAttempts {
				if _, err = db.ExecContext(ctx, `CREATE FUNCTION pg_temp.reject_preparation_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.outcome='preparation_failed' THEN RAISE EXCEPTION 'test webhook failure'; END IF; RETURN NEW; END $$;
					CREATE TRIGGER reject_preparation_event BEFORE INSERT ON webhook_events FOR EACH ROW EXECUTE FUNCTION pg_temp.reject_preparation_event()`); err != nil {
					t.Fatal(err)
				}
				if _, err = repo.FailPreparation(ctx, claim, "object_write"); err == nil {
					t.Fatal("held state committed without its event")
				}
				var state string
				if err = db.QueryRowContext(ctx, `SELECT state FROM messages WHERE id=$1`, messageID).Scan(&state); err != nil || state != "preparing" {
					t.Fatal("event failure left a partially committed hold", state, err)
				}
				if _, err = db.ExecContext(ctx, `DROP TRIGGER reject_preparation_event ON webhook_events`); err != nil {
					t.Fatal(err)
				}
			}
			terminal, err := repo.FailPreparation(ctx, claim, "object_write")
			if err != nil {
				t.Fatal("record preparation failure", i, err)
			}
			if terminal != (i == maxPreparationAttempts) {
				t.Fatal("unexpected terminal flag", i, terminal)
			}
			if _, err = repo.FailPreparation(ctx, claim, "object_write"); !errors.Is(err, preparation.ErrLostLease) {
				t.Fatal("stale failure changed a completed claim", err)
			}
			var state, reason string
			var attempts int
			var next time.Time
			if err = db.QueryRowContext(ctx, `SELECT state,prepare_failure,prepare_attempts,prepare_next_at FROM messages WHERE id=$1`, messageID).Scan(&state, &reason, &attempts, &next); err != nil || reason != "object_write" || attempts != i {
				t.Fatal("failure metadata", state, reason, attempts, err)
			}
			if i < maxPreparationAttempts {
				if state != "queued" || !next.After(time.Now().Add(40*time.Second)) {
					t.Fatal("retry was not delayed", i, state, next)
				}
				if _, err = db.ExecContext(ctx, `UPDATE messages SET prepare_next_at=clock_timestamp()-interval '1 second' WHERE id=$1`, messageID); err != nil {
					t.Fatal(err)
				}
			} else if state != "preparation_failed" {
				t.Fatal("exhausted preparation was not held", state)
			}
		}
		if _, err := repo.ClaimPreparation(ctx, "00000000000000000000000000000f04", 30*time.Second); !errors.Is(err, preparation.ErrNoWork) {
			t.Fatal("held message was claimed", err)
		}
		detail, err := repo.GetMessage(ctx, "operator", appID, messageID)
		if err != nil || detail.PreparationFailure == nil || detail.PreparationFailure.Attempts != maxPreparationAttempts || detail.PreparationFailure.Reason != "object_write" {
			t.Fatal("held preparation not visible to operator", detail.PreparationFailure, err)
		}
		hook, err := repo.ClaimWebhook(ctx, "00000000000000000000000000000f05", 45*time.Second)
		if err != nil || hook.AppID != appID || hook.Event.MessageID != messageID || hook.Event.DeliveryID != "" || hook.Event.AttemptID != "" || hook.Event.Outcome != "preparation_failed" || hook.Event.SMTPCode != 0 {
			t.Fatal("preparation failure webhook", hook.Event, err)
		}
		if err = repo.CommitWebhook(ctx, hook, 204); err != nil {
			t.Fatal(err)
		}
		if _, err = repo.ClaimWebhook(ctx, "00000000000000000000000000000f06", 45*time.Second); !errors.Is(err, webhook.ErrNoWork) {
			t.Fatal("more than one failure webhook was created", err)
		}
		if err = repo.RetryPreparation(ctx, "operator", "00000000000000000000000000000002", messageID); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("cross-app retry accepted", err)
		}
		if err = repo.RetryPreparation(ctx, "operator", appID, messageID); err != nil {
			t.Fatal("audited retry", err)
		}
		if err = repo.RetryPreparation(ctx, "operator", appID, messageID); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("retry accepted a non-held message", err)
		}
		if _, err = repo.FailPreparation(ctx, last, "object_write"); !errors.Is(err, preparation.ErrLostLease) {
			t.Fatal("old claim changed a retried message", err)
		}
		var auditCount, deliveries int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='message.preparation.retry'`, appID).Scan(&auditCount); err != nil || auditCount != 1 {
			t.Fatal("retry was not audited once", auditCount, err)
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE message_id=$1`, messageID).Scan(&deliveries); err != nil || deliveries != 0 {
			t.Fatal("failure or retry created deliveries", deliveries, err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE messages SET state='prepared' WHERE id=$1`, messageID); err != nil {
			t.Fatal(err)
		}
		const expiredID = "00000000000000000000000000000f07"
		if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,prepare_attempts) VALUES ($1,$2,'submissions/expired',true,11)`, expiredID, appID); err != nil {
			t.Fatal(err)
		}
		expired, err := repo.ClaimPreparation(ctx, "00000000000000000000000000000f08", 30*time.Second)
		if err != nil || expired.MessageID != expiredID {
			t.Fatal("last preparation claim", expired, err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE messages SET prepare_until=clock_timestamp()-interval '1 second' WHERE id=$1`, expiredID); err != nil {
			t.Fatal(err)
		}
		var expiredLines []string
		restoreLog := oplog.SetOutput(func(format string, args ...any) { expiredLines = append(expiredLines, fmt.Sprintf(format, args...)) })
		_, err = repo.ClaimPreparation(ctx, "00000000000000000000000000000f09", 30*time.Second)
		restoreLog()
		if !errors.Is(err, preparation.ErrNoWork) {
			t.Fatal("exhausted crashed claim was retried", err)
		}
		if len(expiredLines) != 1 || !strings.HasSuffix(expiredLines[0], "status=preparation_failed reason=lease_expired") {
			t.Fatal("expired terminal claim was not logged", expiredLines)
		}
		var state, reason string
		if err = db.QueryRowContext(ctx, `SELECT state,prepare_failure FROM messages WHERE id=$1`, expiredID).Scan(&state, &reason); err != nil || state != "preparation_failed" || reason != "lease_expired" {
			t.Fatal("expired claim was not held", state, reason, err)
		}
		expiredHook, err := repo.ClaimWebhook(ctx, "00000000000000000000000000000f0a", 45*time.Second)
		if err != nil || expiredHook.Event.MessageID != expiredID || expiredHook.Event.Outcome != "preparation_failed" {
			t.Fatal("expired final claim did not notify its app", expiredHook.Event, err)
		}
		if err = repo.CommitWebhook(ctx, expiredHook, 204); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE apps SET active=false WHERE id=$1`, appID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("controller leases", func(t *testing.T) {
		lease, err := repo.AcquireControllerLease(ctx, "worker", "controller-a", 30*time.Second)
		if err != nil || lease.Epoch != 1 {
			t.Fatalf("initial lease: %+v %v", lease, err)
		}
		renewed, err := repo.RenewControllerLease(ctx, lease, 30*time.Second)
		if err != nil || renewed.Epoch != lease.Epoch {
			t.Fatalf("renewal: %+v %v", renewed, err)
		}
		reconnected, err := repo.AcquireControllerLease(ctx, "reconnect-worker", "controller-a", 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		newSession, err := repo.AcquireControllerLease(ctx, "reconnect-worker", "controller-a", 30*time.Second)
		if err != nil || newSession.Epoch != reconnected.Epoch+1 {
			t.Fatalf("same-controller reconnect did not advance epoch: %+v %v", newSession, err)
		}
		if _, err = repo.RenewControllerLease(ctx, reconnected, 30*time.Second); !errors.Is(err, ErrLeaseHeld) {
			t.Fatal("stale session renewed", err)
		}
		if _, err = repo.ClaimDelivery(ctx, reconnected, "00000000000000000000000000000abc", delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrLeaseInvalid) {
			t.Fatal("stale session claimed delivery", err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE controller_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE worker_id='reconnect-worker'`); err != nil {
			t.Fatal(err)
		}
		if _, err = repo.RenewControllerLease(ctx, newSession, 30*time.Second); !errors.Is(err, ErrLeaseHeld) {
			t.Fatal("expired session renewed", err)
		}
		lockedLease, err := repo.AcquireControllerLease(ctx, "locked-worker", "controller-a", 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var held bool
		if err = tx.QueryRowContext(ctx, `SELECT true FROM controller_leases WHERE worker_id=$1 AND epoch=$2 FOR SHARE`, lockedLease.WorkerID, lockedLease.Epoch).Scan(&held); err != nil {
			t.Fatal(err)
		}
		type acquiredLease struct {
			lease ControllerLease
			err   error
		}
		acquired := make(chan acquiredLease, 1)
		go func() {
			next, acquireErr := repo.AcquireControllerLease(ctx, "locked-worker", "controller-a", 30*time.Second)
			acquired <- acquiredLease{lease: next, err: acquireErr}
		}()
		select {
		case <-acquired:
			t.Fatal("new epoch bypassed an in-flight claim lock")
		case <-time.After(100 * time.Millisecond):
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		select {
		case result := <-acquired:
			if result.err != nil || result.lease.Epoch != lockedLease.Epoch+1 {
				t.Fatal("new epoch did not follow in-flight claim", result.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("new epoch remained blocked")
		}
		expiring, err := repo.AcquireControllerLease(ctx, "expiring-worker", "controller-a", 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE controller_leases SET expires_at=clock_timestamp()+interval '300 milliseconds' WHERE worker_id=$1`, expiring.WorkerID); err != nil {
			t.Fatal(err)
		}
		blocker, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback()
		if err = blocker.QueryRowContext(ctx, `SELECT true FROM controller_leases WHERE worker_id=$1 FOR SHARE`, expiring.WorkerID).Scan(&held); err != nil {
			t.Fatal(err)
		}
		renewedAfterWait := make(chan error, 1)
		go func() {
			_, renewalErr := repo.RenewControllerLease(ctx, expiring, 30*time.Second)
			renewedAfterWait <- renewalErr
		}()
		time.Sleep(450 * time.Millisecond)
		if err = blocker.Commit(); err != nil {
			t.Fatal(err)
		}
		if err = <-renewedAfterWait; !errors.Is(err, ErrLeaseHeld) {
			t.Fatal("lease renewed after expiring during lock wait", err)
		}
		if _, err = repo.AcquireControllerLease(ctx, "worker", "controller-b", 30*time.Second); !errors.Is(err, ErrLeaseHeld) {
			t.Fatalf("other owner: %v", err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE controller_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE worker_id='worker'`); err != nil {
			t.Fatal(err)
		}
		next, err := repo.AcquireControllerLease(ctx, "worker", "controller-b", 30*time.Second)
		if err != nil || next.Epoch != 2 {
			t.Fatalf("takeover: %+v %v", next, err)
		}
		if released, err := repo.ReleaseControllerLease(ctx, lease); err != nil || released {
			t.Fatal("stale controller released new lease")
		}
		if released, err := repo.ReleaseControllerLease(ctx, next); err != nil || !released {
			t.Fatal("current lease could not be released")
		}
		again, err := repo.AcquireControllerLease(ctx, "worker", "controller-b", 30*time.Second)
		if err != nil || again.Epoch != 3 {
			t.Fatalf("reacquisition: %+v %v", again, err)
		}
		var wg sync.WaitGroup
		results := make(chan error, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := repo.AcquireControllerLease(ctx, "contended", fmt.Sprintf("controller-%d", i), 30*time.Second)
				results <- err
			}(i)
		}
		wg.Wait()
		close(results)
		winners := 0
		for err := range results {
			if err == nil {
				winners++
			} else if !errors.Is(err, ErrLeaseHeld) {
				t.Error(err)
			}
		}
		if winners != 1 {
			t.Fatalf("got %d competing winners", winners)
		}
	})
	t.Run("durable delivery results", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext,active) VALUES ('a-active','a','https://a.example/hook','\x01'::bytea,true),('a-disabled','a','https://a.example/old','\x01'::bytea,false),('b-active','b','https://b.example/hook','\x01'::bytea,true)`); err != nil {
			t.Fatal(err)
		}
		lease, err := repo.AcquireControllerLease(ctx, "worker", "controller-b", 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		stale := lease
		stale.Epoch = 1
		if _, err = repo.ClaimDelivery(ctx, stale, "00000000000000000000000000000101", delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrLeaseInvalid) {
			t.Fatalf("stale lease: %v", err)
		}
		claim, err := repo.ClaimDelivery(ctx, lease, "00000000000000000000000000000101", delivery.EdgeCapabilities{})
		if err != nil || !claim.EnhancedProtection {
			t.Fatalf("delivery claim: %+v %v", claim, err)
		}
		sessionID := "00000000000000000000000000000aaa"
		descriptor := sha256.Sum256([]byte("delivery descriptor"))
		premature := delivery.Result{AttemptID: claim.AttemptID, DeliveryID: claim.DeliveryID, WorkerID: claim.WorkerID, Epoch: claim.Epoch, Outcome: "accepted", SMTPCode: 250}
		if err = repo.CommitDeliveryResult(ctx, premature); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("unpermitted SMTP result accepted", err)
		}
		if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, claim.DeliveryID, sessionID, "0123456789abcdef0123456789abcdef", descriptor, 0); err != nil {
			t.Fatal("start authorization", err)
		}
		if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, claim.DeliveryID, sessionID, "0123456789abcdef0123456789abcdef", descriptor, 0); err != nil {
			t.Fatal("start authorization replay", err)
		}
		var storedJournal string
		if err = db.QueryRowContext(ctx, `SELECT start_journal_id FROM delivery_attempts WHERE id=$1`, claim.AttemptID).Scan(&storedJournal); err != nil || storedJournal != "0123456789abcdef0123456789abcdef" {
			t.Fatal("journal identity was not committed", err)
		}
		if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, claim.DeliveryID, sessionID, "ffffffffffffffffffffffffffffffff", descriptor, 0); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("different journal authorized", err)
		}
		wrongDescriptor := sha256.Sum256([]byte("other descriptor"))
		if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, claim.DeliveryID, sessionID, "0123456789abcdef0123456789abcdef", wrongDescriptor, 0); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("changed descriptor authorized", err)
		}
		if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, claim.DeliveryID, "00000000000000000000000000000aab", "0123456789abcdef0123456789abcdef", descriptor, 0); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("different session authorized", err)
		}
		bounce := &bounceaddress.Service{Repository: repo, Domain: "bounce.example.com", ActiveVersion: "old", Keys: map[string][]byte{"old": []byte("0123456789abcdef0123456789abcdef")}}
		address, err := bounce.ForDelivery(ctx, claim)
		if err != nil || len(address) != len("b+0123456789abcdef0123456789abcdef@bounce.example.com") {
			t.Fatalf("bounce address: %q %v", address, err)
		}
		token := address[2:34]
		tokenHash := sha256.Sum256([]byte(token))
		fingerprint := sha256.Sum256([]byte("untrusted bounce"))
		if state, err := repo.CheckBounce(ctx, tokenHash, fingerprint); err != nil || state != bounceingest.New {
			t.Fatal("bounce token was not persisted", err)
		}
		bounceRecord := bounceingest.Record{ID: "00000000000000000000000000000401", ObjectKey: fixtureObjectKey("bounces", "00000000000000000000000000000401"), TokenHash: tokenHash, Fingerprint: fingerprint}
		expiredBounce := bounceRecord
		expiredBounce.ObjectKey, err = objectkey.New("bounces", bounceRecord.ID, time.Now().Add(-2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err = repo.CommitBounce(ctx, expiredBounce); err == nil {
			t.Fatal("expired bounce object gained a reference")
		}
		repo.Objects = objectVerifierFunc(func(context.Context, string) error { return errors.New("missing") })
		if err = repo.CommitBounce(ctx, bounceRecord); err == nil {
			t.Fatal("missing bounce object committed")
		}
		repo.Objects = present
		if err = repo.CommitBounce(ctx, bounceRecord); err != nil {
			t.Fatal(err)
		}
		if state, err := repo.CheckBounce(ctx, tokenHash, fingerprint); err != nil || state != bounceingest.Duplicate {
			t.Fatal("bounce duplicate not detected", err)
		}
		bounceRecord.ID = "00000000000000000000000000000402"
		bounceRecord.ObjectKey = fixtureObjectKey("bounces", bounceRecord.ID)
		if err = repo.CommitBounce(ctx, bounceRecord); err != nil {
			t.Fatal(err)
		}
		var receipts int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bounce_receipts WHERE delivery_id=$1`, claim.DeliveryID).Scan(&receipts); err != nil || receipts != 1 {
			t.Fatal("bounce replay not deduplicated", err)
		}
		var bounceEvents, bounceHooks int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_events WHERE bounce_receipt_id=$1 AND delivery_id=$2 AND attempt_id IS NULL AND outcome='bounce_received_unverified' AND smtp_code=0`, "00000000000000000000000000000401", claim.DeliveryID).Scan(&bounceEvents); err != nil || bounceEvents != 1 {
			t.Fatal("bounce event missing or duplicated", err)
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_deliveries WHERE event_id=$1 AND subscription_id='a-active' AND app_id='a'`, "00000000000000000000000000000401").Scan(&bounceHooks); err != nil || bounceHooks != 1 {
			t.Fatal("bounce event not queued for owning app", err)
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_deliveries WHERE event_id=$1 AND subscription_id IN ('a-disabled','b-active')`, "00000000000000000000000000000401").Scan(&bounceHooks); err != nil || bounceHooks != 0 {
			t.Fatal("bounce event queued for inactive or foreign subscription", err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,bounce_receipt_id,outcome,smtp_code) VALUES ('bad-event','a',$1,$2,$3,$4,'bounce_received_unverified',0)`, claim.MessageID, claim.DeliveryID, claim.AttemptID, "00000000000000000000000000000401"); err == nil {
			t.Fatal("webhook event accepted both an attempt and a bounce receipt")
		}
		bounceClaim, err := repo.ClaimWebhook(ctx, "00000000000000000000000000000404", 45*time.Second)
		if err != nil || bounceClaim.Event.ID != "00000000000000000000000000000401" || bounceClaim.Event.BounceReceiptID != bounceClaim.Event.ID || bounceClaim.Event.AttemptID != "" || bounceClaim.Event.Outcome != "bounce_received_unverified" || bounceClaim.Event.SMTPCode != 0 {
			t.Fatal("unverified bounce webhook claim", bounceClaim, err)
		}
		if err = repo.CommitWebhook(ctx, bounceClaim, 204); err != nil {
			t.Fatal("bounce webhook delivery", err)
		}
		const collisionID = "00000000000000000000000000000405"
		if _, err = db.ExecContext(ctx, `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ($1,'a',$2,$3,$4,'uncertain',0)`, collisionID, claim.MessageID, claim.DeliveryID, claim.AttemptID); err != nil {
			t.Fatal(err)
		}
		collisionFingerprint := sha256.Sum256([]byte("event insert conflict"))
		collision := bounceingest.Record{ID: collisionID, ObjectKey: fixtureObjectKey("bounces", collisionID), TokenHash: tokenHash, Fingerprint: collisionFingerprint}
		if err = repo.CommitBounce(ctx, collision); err == nil {
			t.Fatal("accepted receipt whose event could not be committed")
		}
		if state, err := repo.CheckBounce(ctx, tokenHash, collisionFingerprint); err != nil || state != bounceingest.New {
			t.Fatal("failed event insertion left a committed receipt", state, err)
		}
		if _, err = db.ExecContext(ctx, `DELETE FROM webhook_events WHERE id=$1`, collisionID); err != nil {
			t.Fatal(err)
		}
		var concurrent sync.WaitGroup
		commitErrors := make(chan error, 8)
		sharedFingerprint := sha256.Sum256([]byte("concurrent duplicate bounce"))
		for i := 0; i < 8; i++ {
			concurrent.Add(1)
			go func(i int) {
				defer concurrent.Done()
				id := fmt.Sprintf("%032x", i+700)
				commitErrors <- repo.CommitBounce(ctx, bounceingest.Record{ID: id, ObjectKey: fixtureObjectKey("bounces", id), TokenHash: tokenHash, Fingerprint: sharedFingerprint})
			}(i)
		}
		concurrent.Wait()
		close(commitErrors)
		for commitErr := range commitErrors {
			if commitErr != nil {
				t.Fatal("concurrent bounce replay", commitErr)
			}
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bounce_receipts r JOIN webhook_events e ON e.bounce_receipt_id=r.id WHERE r.delivery_id=$1 AND r.fingerprint=$2`, claim.DeliveryID, sharedFingerprint[:]).Scan(&bounceEvents); err != nil || bounceEvents != 1 {
			t.Fatal("concurrent duplicate created incorrect receipt or event count", bounceEvents, err)
		}
		for i := 0; i < 30; i++ {
			id := fmt.Sprintf("%032x", i+500)
			record := bounceingest.Record{ID: id, ObjectKey: fixtureObjectKey("bounces", id), TokenHash: tokenHash, Fingerprint: sha256.Sum256([]byte(id))}
			if err = repo.CommitBounce(ctx, record); err != nil {
				t.Fatal("bounce capacity", err)
			}
		}
		nextFingerprint := sha256.Sum256([]byte("extra receipt"))
		if state, err := repo.CheckBounce(ctx, tokenHash, nextFingerprint); err != nil || state != bounceingest.Full {
			t.Fatal("bounce capacity not detected", err)
		}
		if state, err := repo.CheckBounce(ctx, tokenHash, fingerprint); err != nil || state != bounceingest.Duplicate {
			t.Fatal("duplicate rejected at capacity", err)
		}
		if err = repo.CommitBounce(ctx, bounceingest.Record{ID: "00000000000000000000000000000403", ObjectKey: fixtureObjectKey("bounces", "00000000000000000000000000000403"), TokenHash: tokenHash, Fingerprint: nextFingerprint}); err == nil {
			t.Fatal("bounce capacity exceeded")
		}
		// This test's remaining receipt webhooks must not interfere with the
		// independent retry exercise later in the same integration fixture.
		if _, err = db.ExecContext(ctx, `UPDATE webhook_deliveries SET state='delivered' WHERE event_id IN (SELECT id FROM webhook_events WHERE delivery_id=$1 AND bounce_receipt_id IS NOT NULL)`, claim.DeliveryID); err != nil {
			t.Fatal(err)
		}
		var deliveryState string
		if err = db.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE id=$1`, claim.DeliveryID).Scan(&deliveryState); err != nil || deliveryState != "in_flight" {
			t.Fatal("untrusted bounce changed delivery state", err)
		}
		bounce.Keys["new"] = []byte("abcdef0123456789abcdef0123456789")
		bounce.ActiveVersion = "new"
		repeated, err := bounce.ForDelivery(ctx, claim)
		if err != nil || repeated != address {
			t.Fatal("bounce address changed on rotation", err)
		}
		delete(bounce.Keys, "old")
		if _, err = bounce.ForDelivery(ctx, claim); err == nil {
			t.Fatal("lost old bounce key accepted")
		}
		bounce.Keys["old"] = []byte("different-key-0123456789abcdef012345")
		if _, err = bounce.ForDelivery(ctx, claim); err == nil {
			t.Fatal("changed old bounce key accepted")
		}
		if _, err = db.ExecContext(ctx, `UPDATE controller_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE worker_id='worker'`); err != nil {
			t.Fatal(err)
		}
		lease, err = repo.AcquireControllerLease(ctx, "worker", "controller-c", 30*time.Second)
		if err != nil || lease.Epoch != claim.Epoch+1 {
			t.Fatal("lease takeover failed")
		}
		result := delivery.Result{AttemptID: claim.AttemptID, DeliveryID: claim.DeliveryID, WorkerID: claim.WorkerID, JournalID: "0123456789abcdef0123456789abcdef", Epoch: claim.Epoch, Outcome: "accepted", SMTPCode: 250}
		wrong := result
		wrong.WorkerID = "other"
		if err = repo.CommitDeliveryResult(ctx, wrong); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatalf("wrong worker: %v", err)
		}
		wrongJournal := result
		wrongJournal.JournalID = "ffffffffffffffffffffffffffffffff"
		if err = repo.CommitDeliveryResult(ctx, wrongJournal); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("result from a different journal accepted", err)
		}
		wrongJournal.JournalID = ""
		if err = repo.CommitDeliveryResult(ctx, wrongJournal); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("result without a journal identity accepted", err)
		}
		if err = repo.CommitDeliveryResult(ctx, result); err != nil {
			t.Fatal(err)
		}
		wrongJournal.JournalID = "ffffffffffffffffffffffffffffffff"
		if err = repo.CommitDeliveryResult(ctx, wrongJournal); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("completed result replayed from a different journal", err)
		}
		if _, err = bounce.ForDelivery(ctx, claim); !errors.Is(err, delivery.ErrAttemptSuperseded) {
			t.Fatalf("finished claim assigned bounce address: %v", err)
		}
		if err = repo.CommitDeliveryResult(ctx, result); err != nil {
			t.Fatalf("replay: %v", err)
		}
		var events, hooks int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_events WHERE attempt_id=$1`, result.AttemptID).Scan(&events); err != nil || events != 1 {
			t.Fatal("duplicate or missing event")
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_deliveries WHERE event_id=$1 AND subscription_id='a-active' AND app_id='a'`, result.AttemptID).Scan(&hooks); err != nil || hooks != 1 {
			t.Fatal("missing app-scoped webhook")
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO webhook_deliveries(event_id,subscription_id,app_id) VALUES ($1,'b-active','a')`, result.AttemptID); err == nil {
			t.Fatal("cross-app webhook accepted")
		}
		wrong = result
		wrong.Outcome = "rejected"
		wrong.SMTPCode = 550
		if err = repo.CommitDeliveryResult(ctx, wrong); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("conflicting result accepted")
		}
		var state string
		if err = db.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE id=$1`, claim.DeliveryID).Scan(&state); err != nil || state != "accepted" {
			t.Fatal("delivery not accepted")
		}
		second, err := repo.ClaimDelivery(ctx, lease, "00000000000000000000000000000102", delivery.EdgeCapabilities{})
		if err != nil {
			t.Fatal(err)
		}
		unknown := delivery.Result{AttemptID: second.AttemptID, DeliveryID: second.DeliveryID, WorkerID: second.WorkerID, Epoch: second.Epoch, Outcome: "uncertain"}
		invalid := unknown
		invalid.Outcome = "rejected"
		invalid.SMTPCode = 450
		if err = repo.CommitDeliveryResult(ctx, invalid); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("unpermitted SMTP rejection accepted", err)
		}
		if err = repo.CommitDeliveryResult(ctx, unknown); err != nil {
			t.Fatal("recovered pre-permit uncertainty", err)
		}
		if err = repo.CommitDeliveryResult(ctx, unknown); err != nil {
			t.Fatal("recovered uncertainty replay", err)
		}
		if _, err = repo.ClaimDelivery(ctx, lease, "00000000000000000000000000000103", delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrNoWork) {
			t.Fatal("uncertain delivery retried without reconciliation")
		}
	})
	t.Run("reconciliation holds orphaned attempts", func(t *testing.T) {
		const app = "00000000000000000000000000000901"
		const message = "00000000000000000000000000000902"
		if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'reconciliation')`, app); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'submissions/reconciliation',true,'prepared')`, message, app); err != nil {
			t.Fatal(err)
		}
		old, err := repo.AcquireControllerLease(ctx, "reconciliation-worker", "controller", 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,current_attempt_id)
			SELECT md5('reconcile-d'||n),$1,n,'final/reconcile/'||n,'in_flight',md5('reconcile-a'||n) FROM generate_series(1,130) AS n`, message); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized,start_descriptor_hash,start_session_id,start_journal_id)
			SELECT md5('reconcile-a'||n),md5('reconcile-d'||n),$1,$2,n%2=0,
			CASE WHEN n%2=0 THEN decode(repeat('ab',32),'hex') END,
			CASE WHEN n%2=0 THEN '0123456789abcdef0123456789abcdef' END,
			CASE WHEN n%2=0 THEN '0123456789abcdef0123456789abcdef' END
			FROM generate_series(1,130) AS n`, old.WorkerID, old.Epoch); err != nil {
			t.Fatal(err)
		}
		var replay delivery.Result
		err = db.QueryRowContext(ctx, `SELECT md5('reconcile-a'||2),md5('reconcile-d'||2)`).Scan(&replay.AttemptID, &replay.DeliveryID)
		if err != nil {
			t.Fatal(err)
		}
		replay.WorkerID, replay.JournalID, replay.Epoch, replay.Outcome, replay.SMTPCode = old.WorkerID, "0123456789abcdef0123456789abcdef", old.Epoch, "accepted", 250
		if err = repo.CommitDeliveryResult(ctx, replay); err != nil {
			t.Fatal("old journal replay", err)
		}
		current, err := repo.AcquireControllerLease(ctx, old.WorkerID, old.ControllerID, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repo.ReconcileOrphanedAttempts(ctx, old); !errors.Is(err, delivery.ErrLeaseInvalid) {
			t.Fatal("stale reconciler accepted", err)
		}
		complete, err := repo.ReconcileOrphanedAttempts(ctx, current)
		if err != nil || complete {
			t.Fatal("first reconciliation batch", complete, err)
		}
		next, err := repo.AcquireControllerLease(ctx, current.WorkerID, current.ControllerID, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repo.ReconcileOrphanedAttempts(ctx, current); !errors.Is(err, delivery.ErrLeaseInvalid) {
			t.Fatal("superseded batch reconciler accepted", err)
		}
		complete, err = repo.ReconcileOrphanedAttempts(ctx, next)
		if err != nil || !complete {
			t.Fatal("final reconciliation batch", complete, err)
		}
		complete, err = repo.ReconcileOrphanedAttempts(ctx, next)
		if err != nil || !complete {
			t.Fatal("reconciliation replay", complete, err)
		}
		var uncertain, accepted, events int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE state='uncertain'),count(*) FILTER (WHERE state='accepted') FROM deliveries WHERE message_id=$1`, message).Scan(&uncertain, &accepted); err != nil || uncertain != 129 || accepted != 1 {
			t.Fatal("orphan outcome counts", uncertain, accepted, err)
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_events WHERE app_id=$1`, app).Scan(&events); err != nil || events != 130 {
			t.Fatal("reconciliation events", events, err)
		}
		if err = repo.CommitDeliveryResult(ctx, replay); err != nil {
			t.Fatal("accepted replay after reconciliation", err)
		}
		var orphan delivery.Result
		if err = db.QueryRowContext(ctx, `SELECT md5('reconcile-a'||1),md5('reconcile-d'||1)`).Scan(&orphan.AttemptID, &orphan.DeliveryID); err != nil {
			t.Fatal(err)
		}
		orphan.WorkerID, orphan.Epoch, orphan.Outcome = old.WorkerID, old.Epoch, "uncertain"
		if err = repo.CommitDeliveryResult(ctx, orphan); err != nil {
			t.Fatal("uncertain replay", err)
		}
		orphan.Outcome, orphan.SMTPCode = "accepted", 250
		if err = repo.CommitDeliveryResult(ctx, orphan); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("contradictory late result accepted", err)
		}
		const blockedDelivery = "00000000000000000000000000000903"
		const blockedAttempt = "00000000000000000000000000000904"
		if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,current_attempt_id) VALUES ($1,$2,131,'final/reconcile/blocked','in_flight',$3)`, blockedDelivery, message, blockedAttempt); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized) VALUES ($1,$2,$3,$4,false)`, blockedAttempt, blockedDelivery, old.WorkerID, old.Epoch); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE controller_leases SET expires_at=clock_timestamp()+interval '300 milliseconds' WHERE worker_id=$1`, next.WorkerID); err != nil {
			t.Fatal(err)
		}
		blocker, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback()
		var locked bool
		if err = blocker.QueryRowContext(ctx, `SELECT true FROM delivery_attempts WHERE id=$1 FOR UPDATE`, blockedAttempt).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		blockedResult := make(chan error, 1)
		go func() {
			_, reconcileErr := repo.ReconcileOrphanedAttempts(ctx, next)
			blockedResult <- reconcileErr
		}()
		time.Sleep(450 * time.Millisecond)
		if err = blocker.Commit(); err != nil {
			t.Fatal(err)
		}
		if err = <-blockedResult; !errors.Is(err, delivery.ErrLeaseInvalid) {
			t.Fatal("reconciliation committed after lease expiry", err)
		}
		var state string
		if err = db.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE id=$1`, blockedDelivery).Scan(&state); err != nil || state != "in_flight" {
			t.Fatal("expired reconciliation changed delivery", state, err)
		}
	})
	t.Run("webhook retries", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `UPDATE apps SET active=false WHERE id='a'`); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.ClaimWebhook(ctx, "00000000000000000000000000000200", 45*time.Second); !errors.Is(err, webhook.ErrNoWork) {
			t.Fatal("inactive app webhook claimed", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE apps SET active=true WHERE id='a'`); err != nil {
			t.Fatal(err)
		}
		first, err := repo.ClaimWebhook(ctx, "00000000000000000000000000000201", 45*time.Second)
		if err != nil || first.AppID != "a" || first.SubscriptionID != "a-active" {
			t.Fatalf("first claim: %+v %v", first, err)
		}
		second, err := repo.ClaimWebhook(ctx, "00000000000000000000000000000202", 45*time.Second)
		if err != nil || second.Event.ID == first.Event.ID {
			t.Fatalf("second claim: %+v %v", second, err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE webhook_deliveries SET claim_until=clock_timestamp()-interval '1 second' WHERE event_id=$1 AND subscription_id=$2`, first.Event.ID, first.SubscriptionID); err != nil {
			t.Fatal(err)
		}
		if err = repo.CommitWebhook(ctx, first, 204); !errors.Is(err, webhook.ErrLostClaim) {
			t.Fatal("expired webhook claim committed")
		}
		retry, err := repo.ClaimWebhook(ctx, "00000000000000000000000000000203", 45*time.Second)
		if err != nil || retry.Event.ID != first.Event.ID || retry.Attempt != 2 {
			t.Fatalf("reclaim: %+v %v", retry, err)
		}
		var oldState string
		if err = db.QueryRowContext(ctx, `SELECT state FROM webhook_attempts WHERE id=$1`, first.Token).Scan(&oldState); err != nil || oldState != "unknown" {
			t.Fatal("crashed attempt not marked unknown")
		}
		if err = repo.CommitWebhook(ctx, retry, 500); err != nil {
			t.Fatal(err)
		}
		var state string
		if err = db.QueryRowContext(ctx, `SELECT state FROM webhook_deliveries WHERE event_id=$1 AND subscription_id=$2`, first.Event.ID, first.SubscriptionID).Scan(&state); err != nil || state != "queued" {
			t.Fatal("failure not scheduled for retry")
		}
		if _, err = db.ExecContext(ctx, `UPDATE webhook_deliveries SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE event_id=$1 AND subscription_id=$2`, first.Event.ID, first.SubscriptionID); err != nil {
			t.Fatal(err)
		}
		final, err := repo.ClaimWebhook(ctx, "00000000000000000000000000000204", 45*time.Second)
		if err != nil || final.Event.ID != first.Event.ID {
			t.Fatal("retry unavailable")
		}
		if err = repo.CommitWebhook(ctx, final, 204); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRowContext(ctx, `SELECT state FROM webhook_deliveries WHERE event_id=$1 AND subscription_id=$2`, first.Event.ID, first.SubscriptionID).Scan(&state); err != nil || state != "delivered" {
			t.Fatal("successful webhook not recorded")
		}
		if _, err = db.ExecContext(ctx, `UPDATE webhook_deliveries SET attempts=12,claim_until=clock_timestamp()-interval '1 second' WHERE event_id=$1 AND subscription_id=$2`, second.Event.ID, second.SubscriptionID); err != nil {
			t.Fatal(err)
		}
		if _, err = repo.ClaimWebhook(ctx, "00000000000000000000000000000205", 45*time.Second); !errors.Is(err, webhook.ErrNoWork) {
			t.Fatalf("exhausted claim: %v", err)
		}
		if err = db.QueryRowContext(ctx, `SELECT state FROM webhook_deliveries WHERE event_id=$1 AND subscription_id=$2`, second.Event.ID, second.SubscriptionID).Scan(&state); err != nil || state != "failed" {
			t.Fatal("exhausted crashed claim not closed")
		}
	})
	t.Run("bounded delivery retries", func(t *testing.T) {
		const maxDeliveryAttempts = 12
		if _, err := db.ExecContext(ctx, `UPDATE delivery_retry_policy SET max_attempts=12,initial_ms=60000,max_delay_ms=21600000 WHERE singleton`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := db.ExecContext(ctx, `UPDATE delivery_retry_policy SET max_attempts=128,initial_ms=1800000,max_delay_ms=10800000 WHERE singleton`); err != nil {
				t.Error(err)
			}
		}()
		const app = "00000000000000000000000000000801"
		const message = "00000000000000000000000000000802"
		const deliveryID = "00000000000000000000000000000803"
		if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'retry')`, app); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'submissions/retry',true,'prepared')`, message, app); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'final/retry')`, deliveryID, message); err != nil {
			t.Fatal(err)
		}
		lease, err := repo.AcquireControllerLease(ctx, "retry-worker", "retry-controller", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= maxDeliveryAttempts; i++ {
			attempt := fmt.Sprintf("%032x", 3000+i)
			claim, err := repo.ClaimDelivery(ctx, lease, attempt, delivery.EdgeCapabilities{})
			if err != nil {
				t.Fatal("claim", i, err)
			}
			if claim.RouteAttempt != i-1 {
				t.Fatal("incorrect route attempt", i, claim.RouteAttempt)
			}
			if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, claim.DeliveryID, "00000000000000000000000000000aad", "0123456789abcdef0123456789abcdef", sha256.Sum256([]byte(attempt)), 0); err != nil {
				t.Fatal("authorize", i, err)
			}
			result := delivery.Result{AttemptID: claim.AttemptID, DeliveryID: claim.DeliveryID, WorkerID: claim.WorkerID, JournalID: "0123456789abcdef0123456789abcdef", Epoch: claim.Epoch, Outcome: "rejected", SMTPCode: 450}
			if err = repo.CommitDeliveryResult(ctx, result); err != nil {
				t.Fatal("commit", i, err)
			}
			if i == 1 {
				if err = repo.CommitDeliveryResult(ctx, result); err != nil {
					t.Fatal("result replay", err)
				}
			}
			var state string
			var next time.Time
			if err = db.QueryRowContext(ctx, `SELECT state,retry_after FROM deliveries WHERE id=$1`, deliveryID).Scan(&state, &next); err != nil {
				t.Fatal(err)
			}
			if i < maxDeliveryAttempts {
				if state != "queued" || !next.After(time.Now().Add(40*time.Second)) {
					t.Fatal("transient failure was not delayed", i, state)
				}
				if _, err = db.ExecContext(ctx, `UPDATE deliveries SET retry_after=clock_timestamp()-interval '1 second' WHERE id=$1`, deliveryID); err != nil {
					t.Fatal(err)
				}
			} else if state != "failed" {
				t.Fatal("attempt limit did not fail delivery")
			}
		}
		if _, err = repo.ClaimDelivery(ctx, lease, "00000000000000000000000000000ffe", delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrNoWork) {
			t.Fatal("exhausted delivery retried", err)
		}
		const prepDelivery = "00000000000000000000000000000804"
		if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,1,'final/preparation-retry')`, prepDelivery, message); err != nil {
			t.Fatal(err)
		}
		claim, err := repo.ClaimDelivery(ctx, lease, "00000000000000000000000000000805", delivery.EdgeCapabilities{})
		if err != nil || claim.RouteAttempt != 0 {
			t.Fatal("preparation claim", err)
		}
		result := delivery.Result{AttemptID: claim.AttemptID, DeliveryID: claim.DeliveryID, WorkerID: claim.WorkerID, Epoch: claim.Epoch, Outcome: "rejected"}
		if err = repo.CommitDeliveryResult(ctx, result); err != nil {
			t.Fatal("preparation failure", err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE deliveries SET retry_after=clock_timestamp()-interval '1 second' WHERE id=$1`, prepDelivery); err != nil {
			t.Fatal(err)
		}
		retry, err := repo.ClaimDelivery(ctx, lease, "00000000000000000000000000000806", delivery.EdgeCapabilities{})
		if err != nil || retry.RouteAttempt != 0 {
			t.Fatal("preparation failure advanced route", retry.RouteAttempt, err)
		}
	})
	t.Run("delivery retry lifetime expires locally", func(t *testing.T) {
		const app = "cb935c4f9d134210bacdff0a570ab901"
		const message = "cb935c4f9d134210bacdff0a570ab902"
		const deliveryID = "cb935c4f9d134210bacdff0a570ab903"
		const expiredAttempt = "cb935c4f9d134210bacdff0a570ab905"
		if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'retry-lifetime')`, app); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'submissions/retry-lifetime',true,'prepared')`, message, app); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'final/retry-lifetime')`, deliveryID, message); err != nil {
			t.Fatal(err)
		}
		lease, err := repo.AcquireControllerLease(ctx, "lifetime-worker", "lifetime-controller", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		claim, err := repo.ClaimDelivery(ctx, lease, "cb935c4f9d134210bacdff0a570ab904", delivery.EdgeCapabilities{})
		if err != nil || claim.DeliveryID != deliveryID {
			t.Fatal("claim", claim, err)
		}
		result := delivery.Result{AttemptID: claim.AttemptID, DeliveryID: deliveryID, WorkerID: claim.WorkerID, Epoch: claim.Epoch, Outcome: delivery.OutcomeRetryExpired}
		if err = repo.CommitDeliveryResult(ctx, result); err == nil {
			t.Fatal("worker forged local expiry")
		}
		result.Outcome = "rejected"
		if err = repo.CommitDeliveryResult(ctx, result); err != nil {
			t.Fatal(err)
		}
		var state string
		var next time.Time
		if err = db.QueryRowContext(ctx, `SELECT state,retry_after FROM deliveries WHERE id=$1`, deliveryID).Scan(&state, &next); err != nil || state != "queued" || next.Before(time.Now().Add(29*time.Minute)) {
			t.Fatal("default retry delay", state, next, err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE delivery_attempts SET started_at=clock_timestamp()-interval '6 days' WHERE id=$1`, claim.AttemptID); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE deliveries SET retry_after=clock_timestamp()-interval '1 second' WHERE id=$1`, deliveryID); err != nil {
			t.Fatal(err)
		}
		if _, err = repo.ClaimDelivery(ctx, lease, expiredAttempt, delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrNoWork) {
			t.Fatal("expired delivery dispatched", err)
		}
		var outcome string
		var code int
		var authorized bool
		if err = db.QueryRowContext(ctx, `SELECT d.state,a.outcome,a.smtp_code,a.start_authorized FROM deliveries d JOIN delivery_attempts a ON a.delivery_id=d.id WHERE d.id=$1 AND a.id=$2`, deliveryID, expiredAttempt).Scan(&state, &outcome, &code, &authorized); err != nil || state != "failed" || outcome != delivery.OutcomeRetryExpired || code != 0 || authorized {
			t.Fatal("expiry not recorded", state, outcome, code, authorized, err)
		}
		if err = db.QueryRowContext(ctx, `SELECT outcome FROM webhook_events WHERE attempt_id=$1`, expiredAttempt).Scan(&outcome); err != nil || outcome != delivery.OutcomeRetryExpired {
			t.Fatal("expiry webhook", outcome, err)
		}
	})
	t.Run("connection rejection retries another route", func(t *testing.T) {
		const app = "00000000000000000000000000000e01"
		const message = "00000000000000000000000000000e02"
		const deliveryID = "00000000000000000000000000000e03"
		if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'route')`, app); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'submissions/route',true,'prepared')`, message, app); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'final/route')`, deliveryID, message); err != nil {
			t.Fatal(err)
		}
		lease, err := repo.AcquireControllerLease(ctx, "route-worker", "route-controller", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		claim, err := repo.ClaimDelivery(ctx, lease, "00000000000000000000000000000e04", delivery.EdgeCapabilities{})
		if err != nil || claim.DeliveryID != deliveryID || claim.RouteAttempt != 0 {
			t.Fatal("first route claim", claim, err)
		}
		result := delivery.Result{AttemptID: claim.AttemptID, DeliveryID: deliveryID, WorkerID: claim.WorkerID, Epoch: claim.Epoch, Outcome: "route_rejected", SMTPCode: 554}
		if err = repo.CommitDeliveryResult(ctx, result); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("accepted a route reply before SMTP authorization", err)
		}
		journalID := "0123456789abcdef0123456789abcdef"
		if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, deliveryID, "00000000000000000000000000000aad", journalID, sha256.Sum256([]byte("route-1")), 0); err != nil {
			t.Fatal(err)
		}
		result.JournalID = journalID
		if err = repo.CommitDeliveryResult(ctx, result); err != nil {
			t.Fatal(err)
		}
		var state, eventOutcome, eventState string
		var code, routes int
		if err = db.QueryRowContext(ctx, `SELECT d.state,d.route_attempts,e.outcome,e.smtp_code,COALESCE(e.delivery_state,'') FROM deliveries d JOIN webhook_events e ON e.attempt_id=$2 WHERE d.id=$1`, deliveryID, claim.AttemptID).Scan(&state, &routes, &eventOutcome, &code, &eventState); err != nil || state != "queued" || eventState != "queued" || routes != 1 || eventOutcome != "route_rejected" || code != 554 {
			t.Fatal("connection rejection not queued as route-scoped", state, routes, eventOutcome, code, err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE deliveries SET retry_after=clock_timestamp()-interval '1 second' WHERE id=$1`, deliveryID); err != nil {
			t.Fatal(err)
		}
		next, err := repo.ClaimDelivery(ctx, lease, "00000000000000000000000000000e05", delivery.EdgeCapabilities{})
		if err != nil || next.DeliveryID != deliveryID || next.RouteAttempt != 1 {
			t.Fatal("retry did not advance route", next, err)
		}
		if err = repo.AuthorizeDeliveryStart(ctx, lease, next.AttemptID, deliveryID, "00000000000000000000000000000aad", journalID, sha256.Sum256([]byte("route-2")), 0); err != nil {
			t.Fatal(err)
		}
		final := delivery.Result{AttemptID: next.AttemptID, DeliveryID: deliveryID, WorkerID: next.WorkerID, JournalID: journalID, Epoch: next.Epoch, Outcome: "rejected", SMTPCode: 550}
		if err = repo.CommitDeliveryResult(ctx, final); err != nil {
			t.Fatal(err)
		}
		if err = repo.CommitDeliveryResult(ctx, result); err != nil {
			t.Fatal("route result replay was not idempotent", err)
		}
		if err = db.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE id=$1`, deliveryID).Scan(&state); err != nil || state != "failed" {
			t.Fatal("recipient 550 did not fail the delivery", state, err)
		}
	})
	t.Run("local permanent DNS results", func(t *testing.T) {
		const app = "00000000000000000000000000000d01"
		const message = "00000000000000000000000000000d02"
		if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'dns')`, app); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'submissions/dns',true,'prepared')`, message, app); err != nil {
			t.Fatal(err)
		}
		lease, err := repo.AcquireControllerLease(ctx, "dns-worker", "dns-controller", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for i, outcome := range []string{delivery.OutcomeNullMX, delivery.OutcomeDomainNotFound} {
			deliveryID := fmt.Sprintf("%032x", 4000+i)
			attemptID := fmt.Sprintf("%032x", 4100+i)
			if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,$3,$4)`, deliveryID, message, i, "final/"+deliveryID); err != nil {
				t.Fatal(err)
			}
			claim, err := repo.ClaimDelivery(ctx, lease, attemptID, delivery.EdgeCapabilities{})
			if err != nil || claim.DeliveryID != deliveryID {
				t.Fatal("claim", claim.DeliveryID, err)
			}
			result := delivery.Result{AttemptID: claim.AttemptID, DeliveryID: claim.DeliveryID, WorkerID: claim.WorkerID, Epoch: claim.Epoch, Outcome: outcome}
			if err = repo.CommitDeliveryResult(ctx, result); err != nil {
				t.Fatal("commit", err)
			}
			if err = repo.CommitDeliveryResult(ctx, result); err != nil {
				t.Fatal("idempotent replay", err)
			}
			var state, eventOutcome, eventState string
			var code int
			if err = db.QueryRowContext(ctx, `SELECT d.state,e.outcome,e.smtp_code,COALESCE(e.delivery_state,'') FROM deliveries d JOIN webhook_events e ON e.delivery_id=d.id WHERE d.id=$1`, deliveryID).Scan(&state, &eventOutcome, &code, &eventState); err != nil || state != "failed" || eventState != "failed" || eventOutcome != outcome || code != 0 {
				t.Fatal("incorrect durable local failure", state, eventOutcome, code, err)
			}
			if _, err = repo.ClaimDelivery(ctx, lease, fmt.Sprintf("%032x", 4200+i), delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrNoWork) {
				t.Fatal("permanent DNS result was retried", err)
			}
		}
		const sentID = "00000000000000000000000000000d03"
		if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,2,'final/dns-authorized')`, sentID, message); err != nil {
			t.Fatal(err)
		}
		claim, err := repo.ClaimDelivery(ctx, lease, "00000000000000000000000000000d04", delivery.EdgeCapabilities{})
		if err != nil {
			t.Fatal(err)
		}
		journalID := "0123456789abcdef0123456789abcdef"
		if err = repo.AuthorizeDeliveryStart(ctx, lease, claim.AttemptID, claim.DeliveryID, "00000000000000000000000000000aad", journalID, sha256.Sum256([]byte("dns")), 0); err != nil {
			t.Fatal(err)
		}
		result := delivery.Result{AttemptID: claim.AttemptID, DeliveryID: claim.DeliveryID, WorkerID: claim.WorkerID, JournalID: journalID, Epoch: claim.Epoch, Outcome: delivery.OutcomeNullMX}
		if err = repo.CommitDeliveryResult(ctx, result); !errors.Is(err, delivery.ErrResultConflict) {
			t.Fatal("accepted a local DNS failure after SMTP authorization", err)
		}
	})
	t.Run("audited operator actions", func(t *testing.T) {
		app, err := repo.CreateApp(ctx, "operator-1", "Payments")
		if err != nil || app.Name != "Payments" {
			t.Fatalf("create app: %v", err)
		}
		issued, err := repo.IssueKeyForOperator(ctx, "operator-1", app.ID, "Primary")
		if err != nil || issued.Secret == "" {
			t.Fatalf("issue key: %v", err)
		}
		digest := sha256.Sum256([]byte(issued.Secret))
		authenticated, err := repo.LookupKey(ctx, digest)
		if err != nil || authenticated.ID != app.ID {
			t.Fatal("issued credential failed")
		}
		keys, err := repo.ListKeys(ctx, app.ID)
		if err != nil || len(keys) != 1 || keys[0].ID != issued.Key.ID {
			t.Fatal("key listing failed")
		}
		if err = repo.RevokeKeyForOperator(ctx, "operator-1", app.ID, issued.Key.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = repo.LookupKey(ctx, digest); !errors.Is(err, submission.ErrInvalidKey) {
			t.Fatal("revoked credential authenticated")
		}
		var auditCount int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND actor_id='operator-1'`, app.ID).Scan(&auditCount); err != nil || auditCount != 3 {
			t.Fatalf("audit count %d: %v", auditCount, err)
		}
		if _, err = db.ExecContext(ctx, `CREATE FUNCTION fail_test_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$; CREATE TRIGGER fail_test_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_test_audit()`); err != nil {
			t.Fatal(err)
		}
		if _, err = repo.IssueKeyForOperator(ctx, "operator-1", app.ID, "Must rollback"); err == nil {
			t.Fatal("issued key without audit")
		}
		if _, err = db.ExecContext(ctx, `DROP TRIGGER fail_test_audit ON audit_events; DROP FUNCTION fail_test_audit()`); err != nil {
			t.Fatal(err)
		}
		keys, err = repo.ListKeys(ctx, app.ID)
		if err != nil || len(keys) != 1 {
			t.Fatal("failed audit did not roll back key")
		}
		if err = repo.RevokeKeyForOperator(ctx, "operator-1", app.ID, issued.Key.ID); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("revoked key unexpectedly active")
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,accepted_at) VALUES ('00000000000000000000000000001001',$1,'submissions/history-1',true,now()),('00000000000000000000000000001002',$1,'submissions/history-2',false,now()-interval '1 second'),('00000000000000000000000000001003',$1,'submissions/history-3',true,now()-interval '2 seconds')`, app.ID); err != nil {
			t.Fatal(err)
		}
		page, err := repo.ListMessages(ctx, "operator-1", app.ID, "", 2)
		if err != nil || len(page.Messages) != 2 || page.NextCursor == "" || !page.Messages[0].EnhancedProtection {
			t.Fatalf("history first page: %+v %v", page, err)
		}
		next, err := repo.ListMessages(ctx, "operator-1", app.ID, page.NextCursor, 2)
		if err != nil || len(next.Messages) != 1 || next.NextCursor != "" || next.Messages[0].ID == page.Messages[0].ID {
			t.Fatalf("history second page: %+v %v", next, err)
		}
		if _, err = repo.ListMessages(ctx, "operator-1", app.ID, "invalid", 2); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatal("invalid history cursor accepted")
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='message.list'`, app.ID).Scan(&auditCount); err != nil || auditCount != 0 {
			t.Fatal("history access must not be audited")
		}
		// The restricted runtime role can run every search branch.
		for _, field := range []string{"any", "id", "recipient", "recipient_domain", "sender_domain", "subject"} {
			text := "history.example.com"
			if field == "id" {
				text = "00000000"
			}
			if found, searchErr := repo.SearchMessages(ctx, "operator-1", app.ID, admin.MessageQuery{Text: text, Field: field, Limit: 10}); searchErr != nil || (field != "id" && len(found.Messages) != 0) || (field == "id" && len(found.Messages) != 3) {
				t.Fatal("restricted runtime search failed", field, found, searchErr)
			}
		}
		if _, revealErr := repo.RevealSubject(ctx, "operator-1", app.ID, "00000000000000000000000000001001"); !errors.Is(revealErr, admin.ErrConflict) {
			t.Fatal("unrecorded subject reveal", revealErr)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ('00000000000000000000000000002001','00000000000000000000000000001001',0,'final/history-1','accepted')`); err != nil {
			t.Fatal(err)
		}
		detail, err := repo.GetMessage(ctx, "operator-1", app.ID, "00000000000000000000000000001001")
		if err != nil || len(detail.Deliveries) != 1 || detail.Message.AcceptedCount != 1 {
			t.Fatalf("message detail: %+v %v", detail, err)
		}
		other, err := repo.CreateApp(ctx, "operator-1", "Other")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repo.GetMessage(ctx, "operator-1", other.ID, "00000000000000000000000000001001"); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("cross-app message visible")
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='message.view'`, app.ID).Scan(&auditCount); err != nil || auditCount != 0 {
			t.Fatal("message view must not be audited")
		}
		key, required, err := repo.AuthorizeBodyRead(ctx, "operator-1", app.ID, "00000000000000000000000000001001")
		if err != nil || key != "submissions/history-1" || !required {
			t.Fatal("authorized body reference unavailable")
		}
		if _, _, err = repo.AuthorizeBodyRead(ctx, "operator-1", other.ID, "00000000000000000000000000001001"); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("cross-app body reference visible")
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='message.body.read'`, app.ID).Scan(&auditCount); err != nil || auditCount != 1 {
			t.Fatal("body read not audited")
		}
		if key, required, err = repo.AuthorizeDeliveryHeadersRead(ctx, "operator-1", app.ID, "00000000000000000000000000001001", "00000000000000000000000000002001"); err != nil || key != "final/history-1" || !required {
			t.Fatal("restricted runtime cannot authorize delivery headers", err)
		}
		if _, required, err = repo.AuthorizeAttachmentRead(ctx, "operator-1", app.ID, "00000000000000000000000000001001", 0); err != nil || !required {
			t.Fatal("restricted runtime cannot authorize attachments", err)
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND action IN ('message.attachment.read','message.delivery.headers.read')`, app.ID).Scan(&auditCount); err != nil || auditCount != 2 {
			t.Fatal("EP attachment and header reads not audited", auditCount, err)
		}
		if key, required, err = repo.AuthorizeRawRead(ctx, "operator-1", app.ID, "00000000000000000000000000001001"); err != nil || key != "submissions/history-1" || !required {
			t.Fatal("restricted runtime cannot authorize raw reads", err)
		}
		if _, _, err = repo.AuthorizeRawRead(ctx, "operator-1", other.ID, "00000000000000000000000000001001"); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("cross-app raw reference visible")
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='message.raw.read' AND resource_id='00000000000000000000000000001001'`, app.ID).Scan(&auditCount); err != nil || auditCount != 1 {
			t.Fatal("EP raw read not audited", auditCount, err)
		}
		challenge, err := repo.RequestDomain(ctx, "operator-1", app.ID, "mail.example")
		if err != nil || challenge.Domain.Active {
			t.Fatal("domain challenge failed")
		}
		oldHash := sha256.Sum256([]byte(challenge.DNSValue))
		replacement, err := repo.RequestDomain(ctx, "operator-1", app.ID, "mail.example")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repo.VerifyDomain(ctx, "operator-1", app.ID, "mail.example", oldHash); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("stale domain challenge accepted")
		}
		currentHash := sha256.Sum256([]byte(replacement.DNSValue))
		domain, err := repo.VerifyDomain(ctx, "operator-1", app.ID, "mail.example", currentHash)
		if err != nil || !domain.Active || domain.VerifiedAt == nil {
			t.Fatal("verified domain not activated")
		}
		if _, err = repo.RequestDomain(ctx, "operator-1", app.ID, "mail.example"); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("active domain challenge was reset")
		}
		if _, _, err = repo.DomainChallenge(ctx, other.ID, "mail.example"); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("cross-app domain visible")
		}
	})
	t.Run("key lifecycle", func(t *testing.T) {
		key, err := repo.IssueKey(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		app, err := repo.LookupKey(ctx, key.Digest)
		if err != nil || app.ID != "a" || app.KeyID != key.ID {
			t.Fatal("issued key does not authenticate")
		}
		ok, err := repo.RevokeKey(ctx, "b", key.ID)
		if err != nil || ok {
			t.Fatal("another app revoked key")
		}
		ok, err = repo.RevokeKey(ctx, "a", key.ID)
		if err != nil || !ok {
			t.Fatal("revocation failed")
		}
		if _, err = repo.LookupKey(ctx, key.Digest); !errors.Is(err, submission.ErrInvalidKey) {
			t.Fatal("revoked key authenticated")
		}
		record := withMetadata(acceptance.Record{AppID: "a", KeyID: key.ID, ObjectKey: fixtureObjectKey("submissions", "00000000000000000000000000000009"), From: "a@example.com", Adapter: "resend", RecipientCount: 1})
		if _, err = repo.Commit(ctx, record); !errors.Is(err, submission.ErrInvalidKey) {
			t.Fatalf("revoked key accepted at commit: %v", err)
		}
	})
	t.Run("bounded object collection checks every reference", func(t *testing.T) {
		testObjectCollection(t, db, repo, collectorRepo, testGeneration)
	})
	t.Run("collection horizon fences writers and cannot rewind", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		writer, err := repo.beginRuntime(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		maintenance, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = maintenance.ExecContext(ctx, `SET LOCAL lock_timeout='150ms'`); err != nil {
			t.Fatal(err)
		}
		_, err = maintenance.ExecContext(ctx, `UPDATE runtime_gate SET object_gc_horizon=clock_timestamp() WHERE singleton`)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
			t.Fatal("horizon advanced while an admitted writer held the gate", err)
		}
		_ = maintenance.Rollback()
		if err = writer.Rollback(); err != nil {
			t.Fatal(err)
		}
		var horizon time.Time
		if err = db.QueryRowContext(ctx, `UPDATE runtime_gate SET object_gc_horizon=clock_timestamp()+interval '10 minutes' WHERE singleton RETURNING object_gc_horizon`).Scan(&horizon); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{
			`UPDATE runtime_gate SET object_gc_horizon=NULL WHERE singleton`,
			`UPDATE runtime_gate SET object_gc_horizon=object_gc_horizon-interval '1 second' WHERE singleton`,
			`UPDATE runtime_gate SET object_gc_horizon='infinity' WHERE singleton`,
		} {
			if _, err = db.ExecContext(ctx, statement); err == nil {
				t.Fatal("invalid horizon change accepted", statement)
			}
		}
		key := fixtureObjectKey("submissions", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeee4")
		record := withMetadata(acceptance.Record{AppID: "a", KeyID: "key-a", ObjectKey: key, From: "sender@example.com", Adapter: "resend", RecipientCount: 1})
		if _, err = repo.Commit(ctx, record); err == nil || err.Error() != "object reference window expired" {
			t.Fatal("writer referenced a key behind the persisted horizon", err)
		}
		var referenced bool
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE object_key=$1)`, key).Scan(&referenced); err != nil || referenced {
			t.Fatal("horizon fence did not roll back the writer", err)
		}
		var persisted time.Time
		if err = db.QueryRowContext(ctx, `SELECT object_gc_horizon FROM runtime_gate WHERE singleton`).Scan(&persisted); err != nil || !persisted.Equal(horizon) {
			t.Fatal("horizon did not remain monotonic", err)
		}
		if _, err = AdvanceObjectGCHorizon(ctx, db, testGeneration, "gc test", 24*time.Hour); err == nil {
			t.Fatal("collector accepted a horizon ahead of its retention cutoff")
		}
	})
	t.Run("operator delivery retry visibility", func(t *testing.T) {
		const app = "f1000000000000000000000000000001"
		const other = "f1000000000000000000000000000002"
		const message = "f1000000000000000000000000000003"
		const queued = "f1000000000000000000000000000004"
		const finished = "f1000000000000000000000000000005"
		const fresh = "f1000000000000000000000000000006"
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`INSERT INTO apps(id,name) VALUES ($1,'retry visibility'),($2,'other app')`, []any{app, other}},
			{`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'source/retry-visibility',true,'prepared')`, []any{message, app}},
			{`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,retry_after) VALUES
				($1,$4,0,'final/retry-visible','queued','2030-01-01T00:00:00Z'),
				($2,$4,1,'final/retry-finished','accepted',now()),
				($3,$4,2,'final/retry-fresh','queued','2030-01-02T00:00:00Z')`, []any{queued, finished, fresh, message}},
			{`INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,state,outcome,smtp_code,started_at,finished_at) VALUES
				('f1000000000000000000000000000010',$1,'worker',1,'completed','rejected',450,'2026-01-01T00:00:00Z','2026-01-01T00:01:00Z'),
				('f1000000000000000000000000000011',$1,'worker',1,'completed','recipient_policy_denied',0,'2026-01-02T00:00:00Z','2026-01-02T00:01:00Z'),
				('f1000000000000000000000000000012',$1,'worker',1,'in_flight',NULL,NULL,'2026-01-03T00:00:00Z',NULL),
				('f1000000000000000000000000000013',$2,'worker',1,'completed','accepted',250,'2026-01-01T00:00:00Z','2026-01-01T00:01:00Z')`, []any{queued, finished}},
		} {
			if _, err := db.ExecContext(ctx, statement.query, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		detail, err := repo.GetMessage(ctx, "operator", app, message)
		if err != nil || len(detail.Deliveries) != 3 {
			t.Fatalf("delivery detail: %+v: %v", detail, err)
		}
		first, second, third := detail.Deliveries[0], detail.Deliveries[1], detail.Deliveries[2]
		if first.ID != queued || first.RetryAttempts != 2 || first.NextAttemptAt == nil || first.NextAttemptAt.UTC().Format(time.RFC3339) != "2030-01-01T00:00:00Z" || first.LastCompletedOutcome == nil || *first.LastCompletedOutcome != "recipient_policy_denied" || first.LastSMTPCode == nil || *first.LastSMTPCode != 0 {
			t.Fatalf("queued retry detail: %+v", first)
		}
		if second.ID != finished || second.RetryAttempts != 1 || second.NextAttemptAt != nil || second.LastCompletedOutcome == nil || *second.LastCompletedOutcome != "accepted" || second.LastSMTPCode == nil || *second.LastSMTPCode != 250 {
			t.Fatalf("finished detail: %+v", second)
		}
		if third.ID != fresh || third.RetryAttempts != 0 || third.NextAttemptAt == nil || third.LastCompletedOutcome != nil || third.LastSMTPCode != nil {
			t.Fatalf("first-attempt detail: %+v", third)
		}
		if _, err = repo.GetMessage(ctx, "operator", other, message); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("cross-app retry detail visible", err)
		}
		var audits int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='message.view'`, app).Scan(&audits); err != nil || audits != 0 {
			t.Fatal("retry detail access must not be audited", audits, err)
		}
	})
}
