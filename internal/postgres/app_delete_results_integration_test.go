package postgres

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/preparation"
)

// inFlight inserts a prepared, pending message with one in-flight delivery and
// its attempt. An authorized attempt carries the journal identity an SMTP
// result must repeat.
func (f *holdFixture) inFlight(app, worker string, epoch uint64, authorized bool) (message, deliveryID, attempt string) {
	f.t.Helper()
	message, deliveryID, attempt = f.id(), f.id(), f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'k'||$1,false,'prepared')`, message, app)
	f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,current_attempt_id) VALUES ($1,$2,0,'f'||$1,'in_flight',$3)`, deliveryID, message, attempt)
	if authorized {
		f.exec(`INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized,start_journal_id,start_session_id,start_descriptor_hash) VALUES ($1,$2,$3,$4,true,$5,'session',decode(repeat('ab',32),'hex'))`, attempt, deliveryID, worker, epoch, journalIdentity)
	} else {
		f.exec(`INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized) VALUES ($1,$2,$3,$4,false)`, attempt, deliveryID, worker, epoch)
	}
	return message, deliveryID, attempt
}

const journalIdentity = "0123456789abcdef0123456789abcdef"

func (f *holdFixture) deliveryState(id string) string {
	f.t.Helper()
	var s string
	if err := f.owner.QueryRowContext(f.ctx, `SELECT state FROM deliveries WHERE id=$1`, id).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// Results that arrive after the app is deleted end in a terminal state and
// clear pending: every outcome but an acceptance is failed.
func TestResultsAfterDeleteAreTerminal(t *testing.T) {
	f := newHoldFixture(t)
	ctx := f.ctx
	cases := []struct {
		name       string
		authorized bool
		outcome    string
		code       int
		want       string
	}{
		{"uncertain", false, "uncertain", 0, "failed"},
		{"policy denied", false, delivery.OutcomeRecipientPolicyDenied, 0, "failed"},
		{"tracking unavailable", false, delivery.OutcomeTrackingUnavailable, 0, "failed"},
		{"temporary failure", true, "rejected", 451, "failed"},
		{"route rejected", true, "route_rejected", 421, "failed"},
		{"accepted", true, "accepted", 250, "accepted"},
	}
	type placed struct{ message, delivery, attempt string }
	var all []placed
	for _, c := range cases {
		m, d, a := f.inFlight(f.app, "worker-1", 1, c.authorized)
		all = append(all, placed{m, d, a})
	}
	if err := f.repo.DeleteApp(ctx, "operator", f.app); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		p := all[i]
		if f.deliveryState(p.delivery) != "in_flight" {
			t.Fatal(c.name, "delete touched an in-flight delivery")
		}
		result := delivery.Result{AttemptID: p.attempt, DeliveryID: p.delivery, WorkerID: "worker-1", Epoch: 1, Outcome: c.outcome, SMTPCode: c.code}
		if c.authorized {
			result.JournalID = journalIdentity
		}
		committed, err := f.repo.CommitDeliveryResultIdentified(ctx, result)
		if err != nil || committed.AppID != f.app {
			t.Fatal(c.name, "commit", committed, err)
		}
		if got := f.deliveryState(p.delivery); got != c.want {
			t.Fatal(c.name, "state", got, "want", c.want)
		}
		if _, pending := f.state(p.message); pending {
			t.Fatal(c.name, "message still pending")
		}
		// An exact replay changes nothing.
		if _, err = f.repo.CommitDeliveryResultIdentified(ctx, result); err != nil {
			t.Fatal(c.name, "replay", err)
		}
	}
	if f.count(`SELECT count(*) FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE m.app_id=$1 AND d.state NOT IN ('accepted','failed')`, f.app) != 0 {
		t.Fatal("work left")
	}
}

// Reconciling orphaned attempts after a delete fails them too.
func TestReconcileAfterDeleteFailsTheDelivery(t *testing.T) {
	f := newHoldFixture(t)
	ctx := f.ctx
	lease, err := f.repo.AcquireControllerLease(ctx, "worker-1", "controller-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	message, deliveryID, _ := f.inFlight(f.app, "worker-1", lease.Epoch, true)
	f.exec(`UPDATE controller_leases SET epoch=epoch+1 WHERE worker_id='worker-1'`)
	lease.Epoch++
	if err = f.repo.DeleteApp(ctx, "operator", f.app); err != nil {
		t.Fatal(err)
	}
	complete, err := f.repo.ReconcileOrphanedAttempts(ctx, lease)
	if err != nil || !complete {
		t.Fatal("reconcile", complete, err)
	}
	if got := f.deliveryState(deliveryID); got != "failed" {
		t.Fatal("reconciled delivery is", got)
	}
	if _, pending := f.state(message); pending {
		t.Fatal("message still pending")
	}
}

// A result and a delete racing in either order leave the same terminal state,
// and neither is aborted as a deadlock victim.
func TestResultRacesDelete(t *testing.T) {
	f := newHoldFixture(t)
	ctx := f.ctx
	for i := 0; i < 8; i++ {
		app := f.app_("race")
		_, deliveryID, attempt := f.inFlight(app, "worker-1", 1, false)
		result := delivery.Result{AttemptID: attempt, DeliveryID: deliveryID, WorkerID: "worker-1", Epoch: 1, Outcome: "uncertain"}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errs[0] = f.repo.CommitDeliveryResultIdentified(ctx, result)
		}()
		go func() {
			defer wg.Done()
			<-start
			errs[1] = f.repo.DeleteApp(ctx, "operator", app)
		}()
		close(start)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatal("race", i, errs)
		}
		if got := f.deliveryState(deliveryID); got != "failed" {
			t.Fatal("race", i, "delivery is", got)
		}
	}
}

// A result commit waiting on a share lock for the app cannot deadlock with the
// delete, whichever takes the app first.
func TestResultWaitsForDeleteWithoutDeadlock(t *testing.T) {
	f := newHoldFixture(t)
	ctx := f.ctx
	message, deliveryID, attempt := f.inFlight(f.app, "worker-1", 1, false)
	tx, err := f.owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM apps WHERE id=$1 FOR UPDATE`, f.app); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() {
		_, e := f.repo.CommitDeliveryResultIdentified(ctx, delivery.Result{AttemptID: attempt, DeliveryID: deliveryID, WorkerID: "worker-1", Epoch: 1, Outcome: "uncertain"})
		done <- e
	}()
	go func() { done <- f.repo.DeleteApp(ctx, "operator", f.app) }()
	time.Sleep(300 * time.Millisecond)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := f.deliveryState(deliveryID); got != "failed" {
		t.Fatal("delivery is", got)
	}
	if _, pending := f.state(message); pending {
		t.Fatal("pending")
	}
}

// Preparation that finishes after the delete fails cleanly, and an operator
// retry of a deleted app's message is refused.
func TestPreparationAfterDelete(t *testing.T) {
	f := newHoldFixture(t)
	ctx := f.ctx
	message := f.mustSubmit(false)
	claim, err := f.repo.ClaimPreparation(ctx, f.id(), 5*time.Second)
	if err != nil || claim.MessageID != message {
		t.Fatal("claim", claim.MessageID, err)
	}
	if err = f.repo.DeleteApp(ctx, "operator", f.app); err != nil {
		t.Fatal(err)
	}
	claim.SigningDomain = "example.com"
	deliveryID := f.id()
	if err = f.repo.CommitPreparation(ctx, claim, []preparation.Delivery{{Format: 1, ID: deliveryID, RecipientIndex: 0, ObjectKey: fixtureObjectKey("final", deliveryID)}}); err == nil {
		t.Fatal("preparation committed for a deleted app")
	}
	if state, pending := f.state(message); state != "preparation_failed" || pending {
		t.Fatal("message", state, pending)
	}
	if f.count(`SELECT count(*) FROM deliveries WHERE message_id=$1`, message) != 0 {
		t.Fatal("deliveries were created")
	}
	if err = f.repo.RetryPreparation(ctx, "operator", f.app, message); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("retry was not refused", err)
	}
	if state, _ := f.state(message); state != "preparation_failed" {
		t.Fatal("retry requeued the message", state)
	}
}
