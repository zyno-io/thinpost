package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/bounceingest"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/webhook"
)

func TestRecoveryGateAndQuarantine(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "recovery_test_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	const first = "00000000000000000000000000000001"
	const second = "00000000000000000000000000000002"
	const initialRun = "00000000000000000000000000000003"
	const restoreRun = "00000000000000000000000000000004"
	const token = "00000000000000000000000000000005"
	initial := &Repository{DB: db, Generation: first}
	if _, err = initial.ClaimPreparation(ctx, token, time.Minute); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("new installation did not start closed", err)
	}
	if err = RecoveryBegin(ctx, db, 1, first, initialRun, "operator"); err != nil {
		t.Fatal(err)
	}
	if err = RecoveryOpen(ctx, db, first, initialRun, "operator", 2); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("opened before quarantine", err)
	}
	if complete, qerr := RecoveryQuarantineBatch(ctx, db, first, initialRun, "operator"); qerr != nil || !complete {
		t.Fatal("empty initial quarantine", complete, qerr)
	}
	if err = RecoveryOpen(ctx, db, first, initialRun, "operator", 3); err != nil {
		t.Fatal(err)
	}
	if err = CheckRuntimeGatePrivileges(ctx, db); err == nil {
		t.Fatal("owner database credentials accepted as runtime")
	}
	role := "recovery_runtime_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'local-recovery-test-password'"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = root.ExecContext(context.Background(), "DROP OWNED BY "+role)
		_, _ = root.ExecContext(context.Background(), "DROP ROLE "+role)
	}()
	if _, err = db.ExecContext(ctx, "GRANT USAGE ON SCHEMA "+schema+" TO "+role); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "GRANT SELECT ON runtime_gate TO "+role); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "GRANT UPDATE (lock_guard) ON runtime_gate TO "+role); err != nil {
		t.Fatal(err)
	}
	impersonated, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	impersonated.SetMaxOpenConns(1)
	defer impersonated.Close()
	if _, err = impersonated.ExecContext(ctx, "SET ROLE "+role); err != nil {
		t.Fatal(err)
	}
	if err = CheckRuntimeGatePrivileges(ctx, impersonated); err == nil {
		t.Fatal("privileged login masked by SET ROLE accepted")
	}
	runtimeURL := *u
	runtimeURL.User = url.UserPassword(role, "local-recovery-test-password")
	runtimeDB, err := sql.Open("pgx", runtimeURL.String())
	if err != nil {
		t.Fatal(err)
	}
	runtimeDB.SetMaxOpenConns(1)
	defer runtimeDB.Close()
	var dbName string
	var publicTemp bool
	if err = db.QueryRowContext(ctx, `SELECT quote_ident(datname), EXISTS (SELECT 1 FROM aclexplode(COALESCE(datacl,acldefault('d',datdba))) a WHERE a.grantee=0 AND a.privilege_type='TEMPORARY') FROM pg_database WHERE datname=current_database()`).Scan(&dbName, &publicTemp); err != nil {
		t.Fatal(err)
	}
	if publicTemp {
		if err = CheckRuntimeGatePrivileges(ctx, runtimeDB); err == nil {
			t.Fatal("PUBLIC temporary-table privilege accepted")
		}
		if _, err = root.ExecContext(ctx, "REVOKE TEMPORARY ON DATABASE "+dbName+" FROM PUBLIC"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = root.ExecContext(context.Background(), "GRANT TEMPORARY ON DATABASE "+dbName+" TO PUBLIC")
		}()
	}
	if err = CheckRuntimeGatePrivileges(ctx, runtimeDB); err != nil {
		t.Fatal("safe runtime role rejected", err)
	}
	if _, err = runtimeDB.ExecContext(ctx, `CREATE TEMP TABLE runtime_gate(generation text,mode text)`); err == nil {
		t.Fatal("runtime role could create a temporary shadow gate")
	}
	if _, err = db.ExecContext(ctx, "GRANT CREATE ON SCHEMA "+schema+" TO "+role); err != nil {
		t.Fatal(err)
	}
	if err = CheckRuntimeGatePrivileges(ctx, runtimeDB); err == nil {
		t.Fatal("runtime role could create objects on its search path")
	}
	if _, err = db.ExecContext(ctx, "REVOKE CREATE ON SCHEMA "+schema+" FROM "+role); err != nil {
		t.Fatal(err)
	}
	runtimeRepo := &Repository{DB: runtimeDB, Generation: first}
	lock, err := runtimeRepo.beginRuntime(ctx)
	if err != nil {
		t.Fatal("runtime role could not lock gate", err)
	}
	if err = lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = runtimeDB.ExecContext(ctx, `UPDATE runtime_gate SET mode='open' WHERE singleton`); err == nil {
		t.Fatal("runtime role could change gate state")
	}
	if _, err = runtimeDB.ExecContext(ctx, `UPDATE runtime_gate SET lock_guard=1 WHERE singleton`); err == nil {
		t.Fatal("runtime role changed lock guard")
	}
	if _, err = runtimeDB.ExecContext(ctx, `DELETE FROM recovery_events`); err == nil {
		t.Fatal("runtime role could delete recovery evidence")
	}
	elevated := "recovery_elevated_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE ROLE "+elevated+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = root.ExecContext(context.Background(), "DROP OWNED BY "+elevated)
		_, _ = root.ExecContext(context.Background(), "DROP ROLE "+elevated)
	}()
	if _, err = db.ExecContext(ctx, "GRANT UPDATE (mode) ON runtime_gate TO "+elevated); err != nil {
		t.Fatal(err)
	}
	if _, err = root.ExecContext(ctx, "GRANT "+elevated+" TO "+role+" WITH INHERIT FALSE, SET TRUE"); err != nil {
		t.Fatal(err)
	}
	if err = CheckRuntimeGatePrivileges(ctx, runtimeDB); err == nil {
		t.Fatal("SET-reachable maintenance role accepted")
	}
	if _, err = root.ExecContext(ctx, "REVOKE "+elevated+" FROM "+role); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "REVOKE UPDATE (mode) ON runtime_gate FROM "+elevated); err != nil {
		t.Fatal(err)
	}
	if err = CheckRuntimeGatePrivileges(ctx, runtimeDB); err != nil {
		t.Fatal("runtime role not restored after maintenance role revoked", err)
	}
	if _, err = root.ExecContext(ctx, "GRANT CREATE ON DATABASE "+dbName+" TO "+elevated); err != nil {
		t.Fatal(err)
	}
	if _, err = root.ExecContext(ctx, "GRANT "+elevated+" TO "+role+" WITH INHERIT FALSE, SET TRUE"); err != nil {
		t.Fatal(err)
	}
	if err = CheckRuntimeGatePrivileges(ctx, runtimeDB); err == nil {
		t.Fatal("SET-reachable database CREATE privilege accepted")
	}
	if _, err = root.ExecContext(ctx, "REVOKE "+elevated+" FROM "+role); err != nil {
		t.Fatal(err)
	}
	if _, err = root.ExecContext(ctx, "REVOKE CREATE ON DATABASE "+dbName+" FROM "+elevated); err != nil {
		t.Fatal(err)
	}
	if err = CheckRuntimeGatePrivileges(ctx, runtimeDB); err != nil {
		t.Fatal("runtime role not restored after database CREATE revoked", err)
	}
	if _, err = root.ExecContext(ctx, "GRANT TEMPORARY ON DATABASE "+dbName+" TO "+elevated); err != nil {
		t.Fatal(err)
	}
	if _, err = root.ExecContext(ctx, "GRANT "+elevated+" TO "+role+" WITH INHERIT FALSE, SET TRUE"); err != nil {
		t.Fatal(err)
	}
	if err = CheckRuntimeGatePrivileges(ctx, runtimeDB); err == nil {
		t.Fatal("SET-reachable temporary-table privilege accepted")
	}
	if _, err = root.ExecContext(ctx, "REVOKE "+elevated+" FROM "+role); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "GRANT UPDATE (mode) ON runtime_gate TO "+role); err != nil {
		t.Fatal(err)
	}
	if err = CheckRuntimeGatePrivileges(ctx, runtimeDB); err == nil {
		t.Fatal("unsafe column grant accepted")
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('app','app');
		INSERT INTO messages(id,app_id,object_key,enhanced_protection,state)
		SELECT md5('restore-m'||n),'app','source/'||n,true,CASE WHEN n%2=0 THEN 'queued' ELSE 'preparing' END FROM generate_series(1,130) n;
		INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,current_attempt_id)
		SELECT md5('restore-d'||n),md5('restore-m'||n),0,'final/'||n,CASE WHEN n%2=0 THEN 'queued' ELSE 'in_flight' END,CASE WHEN n%2=0 THEN NULL ELSE md5('restore-a'||n) END FROM generate_series(1,130) n;
		INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized)
		SELECT md5('restore-a'||n),md5('restore-d'||n),'worker',1,true FROM generate_series(1,130) n WHERE n%2=1;
		INSERT INTO controller_leases(worker_id,controller_id,epoch,expires_at) VALUES ('worker','old',1,clock_timestamp()-interval '1 second');
		INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext) VALUES ('subscription','app','https://example.test','\x01'::bytea);
		INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ('event','app',md5('restore-m'||1),md5('restore-d'||1),md5('restore-a'||1),'uncertain',0);
		INSERT INTO webhook_deliveries(event_id,subscription_id,app_id,state) VALUES ('event','subscription','app','queued');`); err != nil {
		t.Fatal(err)
	}
	// Closure must wait for a runtime transaction that already holds the gate.
	active, err := initial.beginRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- RecoveryBegin(ctx, db, 4, second, restoreRun, "operator") }()
	select {
	case err = <-closed:
		t.Fatal("recovery bypassed an active transaction", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err = active.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = <-closed; err != nil {
		t.Fatal(err)
	}
	if _, err = initial.ClaimPreparation(ctx, token, time.Minute); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("old generation admitted during recovery", err)
	}
	closedLease := ControllerLease{WorkerID: "worker", ControllerID: "old", Epoch: 1}
	if _, err = initial.ClaimDelivery(ctx, closedLease, token, delivery.EdgeCapabilities{}); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("delivery claim admitted during recovery", err)
	}
	if err = initial.AuthorizeDeliveryStart(ctx, closedLease, token, token, token, "0123456789abcdef0123456789abcdef", [32]byte{}, 0); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("SMTP start admitted during recovery", err)
	}
	if err = initial.CommitDeliveryResult(ctx, delivery.Result{AttemptID: token, DeliveryID: token, WorkerID: "worker", Epoch: 1, Outcome: "uncertain"}); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("delivery result admitted during recovery", err)
	}
	if _, err = initial.ClaimWebhook(ctx, token, time.Minute); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("webhook claim admitted during recovery", err)
	}
	if err = initial.CommitWebhook(ctx, webhook.Claim{Token: token, AppID: "app", SubscriptionID: "subscription", Event: webhook.Event{ID: token}}, 204); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("webhook result admitted during recovery", err)
	}
	if _, err = initial.LookupKey(ctx, [32]byte{}); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("submission authentication admitted during recovery", err)
	}
	if _, err = initial.CheckBounce(ctx, [32]byte{}, [32]byte{}); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("bounce preflight admitted during recovery", err)
	}
	if err = initial.CommitBounce(ctx, bounceingest.Record{ID: token, ObjectKey: fixtureObjectKey("bounces", token)}); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("bounce commit admitted during recovery", err)
	}
	if _, err = initial.CleanupExpiredIdempotency(ctx, 1); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("idempotency cleanup admitted during recovery", err)
	}
	if _, err = initial.CreateApp(ctx, "operator", "name"); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("operator mutation admitted during recovery", err)
	}
	if err = RecoveryOpen(ctx, db, second, restoreRun, "operator", 5); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("opened before quarantine", err)
	}
	if _, err = RecoveryReportPage(ctx, db, second, restoreRun, 5, RecoveryCursor{}, 50); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("reported before quarantine completed", err)
	}
	batches := 0
	for {
		complete, qerr := RecoveryQuarantineBatch(ctx, db, second, restoreRun, "operator")
		if qerr != nil {
			t.Fatal(qerr)
		}
		batches++
		if complete {
			break
		}
	}
	if batches < 3 {
		t.Fatal("expected bounded batches", batches)
	}
	var held int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM recovery_holds WHERE recovery_id=$1`, restoreRun).Scan(&held); err != nil || held != 261 {
		t.Fatal("incorrect hold evidence count", held, err)
	}
	if _, err = RecoveryReportPage(ctx, db, second, restoreRun, 5, RecoveryCursor{}, 50); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("report accepted stale revision", err)
	}
	if _, err = RecoveryReportPage(ctx, db, first, restoreRun, 6, RecoveryCursor{}, 50); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("report accepted wrong generation", err)
	}
	for _, invalid := range []struct {
		cursor RecoveryCursor
		limit  int
	}{
		{RecoveryCursor{}, 0}, {RecoveryCursor{}, 501},
		{RecoveryCursor{Kind: "webhook", EntityID: "event"}, 50},
		{RecoveryCursor{Kind: "message", EntityID: "message", SecondaryID: "unexpected"}, 50},
		{RecoveryCursor{Kind: "other", EntityID: "message"}, 50},
	} {
		if _, reportErr := RecoveryReportPage(ctx, db, second, restoreRun, 6, invalid.cursor, invalid.limit); reportErr == nil {
			t.Fatal("report accepted invalid page request")
		}
	}
	seen := make(map[RecoveryCursor]bool)
	var deliveryEvidence, attemptEvidence int
	var cursor RecoveryCursor
	for {
		page, reportErr := RecoveryReportPage(ctx, db, second, restoreRun, 6, cursor, 50)
		if reportErr != nil || len(page.Holds) > 50 || page.Revision != 6 {
			t.Fatal("recovery report page", reportErr)
		}
		for _, hold := range page.Holds {
			if seen[hold.RecoveryCursor] || hold.OriginalState == "" || hold.RecordedAt.IsZero() {
				t.Fatal("duplicate or incomplete recovery hold")
			}
			seen[hold.RecoveryCursor] = true
			if hold.Kind == "delivery" {
				if hold.Delivery == nil || hold.Delivery.MessageID == "" || hold.Delivery.CurrentState != "recovery_hold" {
					t.Fatal("missing delivery evidence", hold)
				}
				deliveryEvidence++
				if hold.OriginalState == "in_flight" {
					attempt := hold.Delivery.CurrentAttempt
					if attempt == nil || attempt.ID != hold.Delivery.CurrentAttemptID || attempt.WorkerID != "worker" || attempt.Epoch != 1 || attempt.State != "in_flight" || !attempt.StartAuthorized || attempt.JournalID != "" {
						t.Fatal("incorrect restored attempt evidence", hold)
					}
					attemptEvidence++
				} else if hold.Delivery.CurrentAttemptID != "" || hold.Delivery.CurrentAttempt != nil {
					t.Fatal("queued hold gained an attempt", hold)
				}
			} else if hold.Delivery != nil {
				t.Fatal("non-delivery hold gained delivery evidence", hold)
			}
		}
		if page.Next == nil {
			break
		}
		cursor = *page.Next
	}
	if len(seen) != 261 || deliveryEvidence != 130 || attemptEvidence != 65 {
		t.Fatal("recovery report omitted evidence", len(seen), deliveryEvidence, attemptEvidence)
	}
	// A dangling attempt pointer must remain visible without inventing matching
	// journal evidence. This simulates inconsistency in restored data.
	var danglingDelivery, previousAttempt string
	if err = db.QueryRowContext(ctx, `SELECT id,current_attempt_id FROM deliveries WHERE state='recovery_hold' AND current_attempt_id IS NOT NULL ORDER BY id LIMIT 1`).Scan(&danglingDelivery, &previousAttempt); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE deliveries SET current_attempt_id='missing-attempt' WHERE id=$1`, danglingDelivery); err != nil {
		t.Fatal(err)
	}
	page, err := RecoveryReportPage(ctx, db, second, restoreRun, 6, RecoveryCursor{}, 500)
	if err != nil {
		t.Fatal(err)
	}
	foundDangling := false
	for _, hold := range page.Holds {
		if hold.Kind == "delivery" && hold.EntityID == danglingDelivery {
			foundDangling = hold.Delivery != nil && hold.Delivery.CurrentAttemptID == "missing-attempt" && hold.Delivery.CurrentAttempt == nil
		}
	}
	if !foundDangling {
		t.Fatal("dangling attempt pointer was concealed or matched incorrectly")
	}
	if _, err = db.ExecContext(ctx, `UPDATE deliveries SET current_attempt_id=$2 WHERE id=$1`, danglingDelivery, previousAttempt); err != nil {
		t.Fatal(err)
	}
	if err = RecoveryOpen(ctx, db, second, restoreRun, "operator", 5); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("stale revision opened gate", err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('late-message','app','source/late',true,'queued')`); err != nil {
		t.Fatal(err)
	}
	if _, err = RecoveryReportPage(ctx, db, second, restoreRun, 6, RecoveryCursor{}, 50); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("report accepted newly pending work", err)
	}
	if err = RecoveryOpen(ctx, db, second, restoreRun, "operator", 6); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("opened with newly discovered work", err)
	}
	if complete, qerr := RecoveryQuarantineBatch(ctx, db, second, restoreRun, "operator"); qerr != nil || complete {
		t.Fatal("late quarantine batch", complete, qerr)
	}
	if complete, qerr := RecoveryQuarantineBatch(ctx, db, second, restoreRun, "operator"); qerr != nil || !complete {
		t.Fatal("late quarantine completion", complete, qerr)
	}
	if _, err = RecoveryReportPage(ctx, db, second, restoreRun, 6, cursor, 50); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("report accepted cursor from prior quarantine revision", err)
	}
	if err = RecoveryOpen(ctx, db, second, restoreRun, "operator", 8); err != nil {
		t.Fatal(err)
	}
	if err = RecoveryOpen(ctx, db, second, restoreRun, "operator", 8); err != nil {
		t.Fatal("identical reopen retry failed", err)
	}
	if _, err = RecoveryReportPage(ctx, db, second, restoreRun, 8, RecoveryCursor{}, 50); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("report accepted open gate", err)
	}
	if err = RecoveryBegin(ctx, db, 9, first, "00000000000000000000000000000006", "operator"); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("historical deployment generation reused", err)
	}
	if _, err = initial.ClaimPreparation(ctx, token, time.Minute); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("stale deployment generation admitted", err)
	}
	restored := &Repository{DB: db, Generation: second}
	if _, err = restored.ClaimPreparation(ctx, token, time.Minute); !errors.Is(err, preparation.ErrNoWork) {
		t.Fatal("restored message was made sendable", err)
	}
	lease, err := restored.AcquireControllerLease(ctx, "worker", "new", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if complete, qerr := restored.ReconcileOrphanedAttempts(ctx, lease); qerr != nil || !complete {
		t.Fatal("normal failover touched recovery-held attempts", complete, qerr)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE state='recovery_hold'`).Scan(&held); err != nil || held != 130 {
		t.Fatal("delivery holds changed after reopening", held, err)
	}

	// A second restore must report unresolved work carried from the first
	// recovery, without discarding its original hold evidence or releasing it.
	const third = "00000000000000000000000000000007"
	const secondRestore = "00000000000000000000000000000008"
	if err = RecoveryBegin(ctx, db, 9, third, secondRestore, "operator"); err != nil {
		t.Fatal(err)
	}
	for {
		complete, qerr := RecoveryQuarantineBatch(ctx, db, third, secondRestore, "operator")
		if qerr != nil {
			t.Fatal("second recovery quarantine", qerr)
		}
		if complete {
			break
		}
	}
	page, err = RecoveryReportPage(ctx, db, third, secondRestore, 11, RecoveryCursor{}, 500)
	if err != nil || len(page.Holds) != 262 || page.Next != nil {
		t.Fatal("second recovery omitted inherited holds", len(page.Holds), err)
	}
	kinds := map[string]int{}
	for _, hold := range page.Holds {
		if hold.OriginalState != "recovery_hold" {
			t.Fatal("inherited hold lost its previous state", hold)
		}
		kinds[hold.Kind]++
	}
	if kinds["message"] != 131 || kinds["delivery"] != 130 || kinds["webhook"] != 1 {
		t.Fatal("inherited hold kinds missing", kinds)
	}
	entity := RecoveryCursor{Kind: "message", EntityID: "late-message"}
	history, historyErr := RecoveryHoldHistoryPage(ctx, db, third, secondRestore, 11, entity, "", 1)
	if historyErr != nil || len(history.Entries) != 1 || history.Entries[0].RecoveryID != restoreRun || history.Entries[0].OriginalState != "queued" || history.Next != restoreRun {
		t.Fatal("first recovery hold history page", history, historyErr)
	}
	history, historyErr = RecoveryHoldHistoryPage(ctx, db, third, secondRestore, 11, entity, history.Next, 1)
	if historyErr != nil || len(history.Entries) != 1 || history.Entries[0].RecoveryID != secondRestore || history.Entries[0].OriginalState != "recovery_hold" || history.Next != "" {
		t.Fatal("second recovery hold history page", history, historyErr)
	}
	webhookEntity := RecoveryCursor{Kind: "webhook", EntityID: "event", SecondaryID: "subscription"}
	history, historyErr = RecoveryHoldHistoryPage(ctx, db, third, secondRestore, 11, webhookEntity, "", 100)
	if historyErr != nil || len(history.Entries) != 2 || history.Entries[0].OriginalState != "queued" || history.Entries[1].OriginalState != "recovery_hold" {
		t.Fatal("webhook hold history", history, historyErr)
	}
	if _, historyErr = RecoveryHoldHistoryPage(ctx, db, third, secondRestore, 10, entity, "", 1); !errors.Is(historyErr, ErrRecoveryConflict) {
		t.Fatal("hold history accepted stale revision", historyErr)
	}
	if _, historyErr = RecoveryHoldHistoryPage(ctx, db, third, secondRestore, 11, RecoveryCursor{Kind: "message", EntityID: "missing"}, "", 1); !errors.Is(historyErr, ErrRecoveryConflict) {
		t.Fatal("hold history accepted an entity outside this recovery", historyErr)
	}
	for _, invalid := range []struct {
		entity RecoveryCursor
		after  string
		limit  int
	}{
		{RecoveryCursor{}, "", 1}, {RecoveryCursor{Kind: "webhook", EntityID: "event"}, "", 1},
		{entity, "not-a-recovery-id", 1}, {entity, "", 0}, {entity, "", 101},
	} {
		if _, historyErr = RecoveryHoldHistoryPage(ctx, db, third, secondRestore, 11, invalid.entity, invalid.after, invalid.limit); historyErr == nil {
			t.Fatal("accepted invalid hold history request", invalid)
		}
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM recovery_holds WHERE recovery_id=$1`, restoreRun).Scan(&held); err != nil || held != 262 {
		t.Fatal("original recovery evidence changed", held, err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('orphan-held','app','source/orphan-held',true,'recovery_hold')`); err != nil {
		t.Fatal(err)
	}
	if _, err = RecoveryReportPage(ctx, db, third, secondRestore, 11, RecoveryCursor{}, 500); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("reported while an inherited hold was missing", err)
	}
	if _, err = RecoveryObjectInventoryPage(ctx, db, third, secondRestore, 11, RecoveryObjectCursor{}, 1, func(context.Context, string) error { return nil }); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("inventoried while an inherited hold was missing", err)
	}
	if _, err = RecoveryHoldHistoryPage(ctx, db, third, secondRestore, 11, entity, "", 1); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("hold history accepted newly unrecorded work", err)
	}
	if err = RecoveryOpen(ctx, db, third, secondRestore, "operator", 11); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("opened while an inherited hold was missing", err)
	}
	if complete, qerr := RecoveryQuarantineBatch(ctx, db, third, secondRestore, "operator"); qerr != nil || complete {
		t.Fatal("late inherited hold was not quarantined", complete, qerr)
	}
	if complete, qerr := RecoveryQuarantineBatch(ctx, db, third, secondRestore, "operator"); qerr != nil || !complete {
		t.Fatal("late inherited hold did not complete", complete, qerr)
	}
	if _, err = db.ExecContext(ctx, `UPDATE messages SET state='queued' WHERE id='orphan-held'`); err != nil {
		t.Fatal(err)
	}
	if _, err = RecoveryQuarantineBatch(ctx, db, third, secondRestore, "operator"); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("accepted a current hold that was made active", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE messages SET state='recovery_hold' WHERE id='orphan-held'`); err != nil {
		t.Fatal(err)
	}
	if err = RecoveryOpen(ctx, db, third, secondRestore, "operator", 13); err != nil {
		t.Fatal("could not open after reconciling inherited holds", err)
	}
}
