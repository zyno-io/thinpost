package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestRecipientPolicyDenialSurvivesRestartAndRequiresExactAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	j, err := Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	fence := Fence{ControllerID: "controller", Epoch: 1}
	if err = j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("attempt", "delivery", fence, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = j.Complete("attempt", RecipientPolicyDenied, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err = j.Recover(time.Now()); err != nil {
		t.Fatal(err)
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != RecipientPolicyDenied || items[0].SMTPCode != 0 {
		t.Fatal("denial did not survive recovery", err)
	}
	wrong := items[0]
	wrong.Outcome = Rejected
	if err = j.Acknowledge(wrong); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("different result acknowledged", err)
	}
	if err = j.Acknowledge(items[0]); err != nil {
		t.Fatal("exact acknowledgement", err)
	}
}

func TestJournalIdentitySurvivesRestartAndDistinguishesNewVolumes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.db")
	first, err := Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	id, err := first.Identity()
	if err != nil || !ValidIdentity(id) {
		t.Fatal("invalid identity", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.Identity()
	if err != nil || again != id {
		t.Fatal("journal identity changed after restart", err)
	}
	_ = reopened.Close()
	replacement, err := Open(filepath.Join(dir, "replacement.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	other, err := replacement.Identity()
	if err != nil || other == id {
		t.Fatal("replacement journal reused identity", err)
	}
}

func TestLegacyJournalWithEvidenceRequiresRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		control, err := tx.CreateBucket(controlBucket)
		if err != nil {
			return err
		}
		return control.Put(fenceKey, []byte(`{"controller_id":"old","epoch":4}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(path, 2); !errors.Is(err, ErrUnidentified) {
		t.Fatal("legacy evidence received a new identity", err)
	}
	db, err = bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(controlBucket).Get(fenceKey) == nil || tx.Bucket(controlBucket).Get(identityKey) != nil {
			return errors.New("legacy journal changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUnexpectedJournalFailureStopsWorkUntilReopenAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	j, err := Open(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	fence := Fence{ControllerID: "controller", Epoch: 1}
	now := time.Now()
	if err = j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("attempt", "delivery", fence, now); err != nil {
		t.Fatal(err)
	}
	storageFailure := errors.New("injected storage failure")
	if err = j.update(func(*bolt.Tx) error { return storageFailure }); !errors.Is(err, storageFailure) {
		t.Fatal("storage failure was hidden", err)
	}
	if _, err = j.Remaining(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("faulted journal advertised capacity", err)
	}
	if err = j.Begin("another", "other-delivery", fence, now); !errors.Is(err, ErrUnavailable) {
		t.Fatal("faulted journal accepted another attempt", err)
	}
	if err = j.Recover(now); !errors.Is(err, ErrUnavailable) {
		t.Fatal("faulted journal recovered without reopening", err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = Open(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err = j.Recover(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	attempts, err := j.List()
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != Uncertain {
		t.Fatal("recovery did not preserve the ambiguous attempt", attempts, err)
	}
}

func TestFaultClosesAdmissionBeforeWaitingOperation(t *testing.T) {
	j, err := Open(filepath.Join(t.TempDir(), "journal.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	inside := make(chan struct{})
	release := make(chan struct{})
	failed := make(chan error, 1)
	go func() {
		failed <- j.operation(func() error {
			close(inside)
			<-release
			return errors.New("injected storage failure")
		})
	}()
	<-inside
	waiting := make(chan error, 1)
	go func() {
		_, err := j.Remaining()
		waiting <- err
	}()
	close(release)
	if err = <-failed; err == nil {
		t.Fatal("failure disappeared")
	}
	if err = <-waiting; !errors.Is(err, ErrUnavailable) {
		t.Fatal("waiting operation crossed the failure barrier", err)
	}
}

func TestMissingAttemptOnCompletionRequiresRecovery(t *testing.T) {
	j, err := Open(filepath.Join(t.TempDir(), "journal.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err = j.Complete("missing", Accepted, 250, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing result was not rejected", err)
	}
	if _, err = j.Remaining(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing attempt did not stop admission", err)
	}
}

func TestRestartPreservesResultsAndRecoversInterruptedAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempts.db")
	now := time.Now().UTC()
	j, err := Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Activate(Fence{ControllerID: "controller-a", Epoch: 3}); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("attempt-1", "delivery-1", Fence{ControllerID: "controller-a", Epoch: 3}, now); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("attempt-2", "delivery-2", Fence{ControllerID: "controller-a", Epoch: 3}, now); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("attempt-3", "delivery-3", Fence{ControllerID: "controller-a", Epoch: 3}, now); !errors.Is(err, ErrFull) {
		t.Fatalf("capacity: %v", err)
	}
	if err = j.Acknowledge(Attempt{ID: "attempt-1", DeliveryID: "delivery-1", Epoch: 3, Outcome: Accepted, SMTPCode: 250}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("in-flight ack: %v", err)
	}
	if err = j.Complete("attempt-1", Accepted, 250, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err = j.Recover(now.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	attempts, err := j.List()
	if err != nil || len(attempts) != 2 {
		t.Fatalf("list: %v", err)
	}
	if attempts[0].Outcome != Accepted || attempts[0].SMTPCode != 250 || attempts[1].Outcome != Uncertain || attempts[1].Epoch != 3 {
		t.Fatalf("unexpected recovery: %+v", attempts)
	}
	if err = j.Activate(Fence{ControllerID: "controller-b", Epoch: 4}); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("attempt-4", "delivery-1", Fence{ControllerID: "controller-b", Epoch: 4}, now); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate delivery: %v", err)
	}
	if err = j.Acknowledge(Attempt{ID: "attempt-1", DeliveryID: "delivery-1", Epoch: 3, Outcome: Rejected, SMTPCode: 550}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("accepted a mismatched acknowledgement")
	}
	if err = j.Acknowledge(Attempt{ID: "attempt-1", DeliveryID: "delivery-1", Epoch: 3, Outcome: Accepted, SMTPCode: 250}); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("attempt-4", "delivery-1", Fence{ControllerID: "controller-b", Epoch: 4}, now); err != nil {
		t.Fatalf("capacity not released: %v", err)
	}
	if err = j.Complete("attempt-2", Rejected, 550, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("overwrote uncertain result")
	}
}
func TestConcurrentCapacityAndSafePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempts.db")
	j, err := Open(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Activate(Fence{ControllerID: "controller", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errorsCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errorsCh <- j.Begin(fmt.Sprintf("a-%d", i), fmt.Sprintf("d-%d", i), Fence{ControllerID: "controller", Epoch: 1}, time.Now())
		}(i)
	}
	wg.Wait()
	close(errorsCh)
	accepted := 0
	for err := range errorsCh {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrFull) {
			t.Error(err)
		}
	}
	if accepted != 8 {
		t.Fatalf("accepted %d", accepted)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(path, 8); err == nil {
		t.Fatal("opened a journal readable by other users")
	}
}
func TestRejectsInvalidMetadata(t *testing.T) {
	j, err := Open(filepath.Join(t.TempDir(), "journal.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err = j.Activate(Fence{ControllerID: "controller", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("user@example.com", "delivery", Fence{ControllerID: "controller", Epoch: 1}, time.Now()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("accepted non-opaque ID")
	}
	if err = j.Begin("a", "d", Fence{ControllerID: "controller", Epoch: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = j.Complete("a", Accepted, 550, time.Now()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("accepted contradictory SMTP code")
	}
	if err = j.Complete("a", Uncertain, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = j.Acknowledge(Attempt{ID: "a", DeliveryID: "d", Epoch: 1, Outcome: Uncertain}); err != nil {
		t.Fatal(err)
	}
}

func TestFencePersistsAndRejectsStaleControllers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	j, err := Open(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	old := Fence{ControllerID: "old", Epoch: 5}
	newer := Fence{ControllerID: "new", Epoch: 6}
	if err = j.Begin("a", "d", old, time.Now()); !errors.Is(err, ErrStaleController) {
		t.Fatal("started without active fence")
	}
	if err = j.Activate(old); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("a", "d", old, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = j.Activate(Fence{ControllerID: "imposter", Epoch: 5}); !errors.Is(err, ErrStaleController) {
		t.Fatal("different holder reused epoch")
	}
	if err = j.Activate(newer); err != nil {
		t.Fatal(err)
	}
	if current, err := j.IsCurrent(old); err != nil || current {
		t.Fatal("old fence still authorized", err)
	}
	if current, err := j.IsCurrent(newer); err != nil || !current {
		t.Fatal("new fence not authorized", err)
	}
	if err = j.Begin("b", "other", old, time.Now()); !errors.Is(err, ErrStaleController) {
		t.Fatal("old controller started after fence")
	}
	// A result from the old epoch remains valid and reportable.
	if err = j.Complete("a", Accepted, 250, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = Open(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err = j.Activate(old); !errors.Is(err, ErrStaleController) {
		t.Fatal("rollback after restart")
	}
	if err = j.Activate(newer); err != nil {
		t.Fatal("current owner could not renew")
	}
	if err = j.Begin("b", "other", newer, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteReplyRecordsStageAndEnhancedCode(t *testing.T) {
	j, err := Open(filepath.Join(t.TempDir(), "journal.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	fence := Fence{ControllerID: "controller", Epoch: 1}
	if err = j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, id := range []string{"a", "b", "c", "d"} {
		if err = j.Begin(id, "delivery-"+id, fence, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []struct {
		code            int
		stage, enhanced string
	}{{550, "HELO", ""}, {550, "RCPT", "5.1"}, {550, "RCPT", "6.1.1"}, {0, "RCPT", ""}, {250, "", "2.0.0"}, {550, "", "5.1.1 text"}} {
		if err = j.CompleteReply("a", Rejected, bad.code, bad.stage, bad.enhanced, now); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
	if err = j.CompleteReply("a", Rejected, 550, "RCPT", "5.1.1", now); err != nil {
		t.Fatal(err)
	}
	if err = j.CompleteReply("b", Rejected, 550, "RCPT", "", now); err != nil {
		t.Fatal(err)
	}
	if err = j.Complete("c", Rejected, 550, now); err != nil {
		t.Fatal(err)
	}
	items, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Attempt{}
	for _, item := range items {
		got[item.ID] = item
	}
	if got["a"].SMTPStage != "RCPT" || got["a"].SMTPEnhanced != "5.1.1" || got["b"].SMTPStage != "RCPT" || got["b"].SMTPEnhanced != "" || got["c"].SMTPStage != "" {
		t.Fatalf("recorded %+v", got)
	}
	// Acknowledgement must match the stage and code too.
	wrong := got["a"]
	wrong.SMTPEnhanced = "5.1.10"
	if err = j.Acknowledge(wrong); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("mismatched enhanced code acknowledged: %v", err)
	}
	wrong = got["a"]
	wrong.SMTPStage = ""
	if err = j.Acknowledge(wrong); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("mismatched stage acknowledged: %v", err)
	}
	if err = j.Acknowledge(got["a"]); err != nil {
		t.Fatal(err)
	}
}

func TestAttemptJSONIsBackwardCompatible(t *testing.T) {
	// Written by a pre-schema-34 worker: no stage or enhanced fields.
	var old Attempt
	if err := json.Unmarshal([]byte(`{"id":"a","delivery_id":"d","epoch":1,"started_at":"2026-11-01T00:00:00Z","finished_at":"2026-11-01T00:00:01Z","outcome":"rejected","smtp_code":550}`), &old); err != nil || old.SMTPStage != "" || old.SMTPEnhanced != "" || old.SMTPCode != 550 {
		t.Fatalf("old journal entry: %+v %v", old, err)
	}
	// Omitted when empty, so an older worker's journal bytes do not change shape.
	raw, err := json.Marshal(Attempt{ID: "a", DeliveryID: "d", Epoch: 1, Outcome: Rejected, SMTPCode: 550})
	if err != nil || strings.Contains(string(raw), "smtp_stage") || strings.Contains(string(raw), "smtp_enhanced") {
		t.Fatalf("empty fields serialized: %s %v", raw, err)
	}
	// An older worker's Attempt type ignores the new keys.
	type oldAttempt struct {
		ID       string `json:"id"`
		SMTPCode int    `json:"smtp_code,omitempty"`
	}
	raw, _ = json.Marshal(Attempt{ID: "a", SMTPCode: 550, SMTPStage: "RCPT", SMTPEnhanced: "5.1.1"})
	var legacy oldAttempt
	if err = json.Unmarshal(raw, &legacy); err != nil || legacy.SMTPCode != 550 {
		t.Fatalf("legacy decode: %+v %v", legacy, err)
	}
}
