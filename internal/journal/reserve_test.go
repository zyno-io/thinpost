package journal

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type testReserve struct {
	armed       bool
	full        bool
	releaseFail bool
}

func (r *testReserve) arm() error {
	if r.full && !r.armed {
		return syscall.ENOSPC
	}
	r.armed = true
	return nil
}
func (r *testReserve) release() error {
	if r.releaseFail {
		return errors.New("reserve release failed")
	}
	r.armed = false
	return nil
}
func (*testReserve) close() error { return nil }

func TestReserveExhaustionPreservesCommittedOutcomeAndStopsNewAttempts(t *testing.T) {
	j, err := Open(filepath.Join(t.TempDir(), "journal.db"), 3)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	r := &testReserve{armed: true}
	j.reserve = r
	fence := Fence{ControllerID: "controller", Epoch: 1}
	if err = j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("attempt", "delivery", fence, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.full = true
	if err = j.Complete("attempt", Accepted, 250, time.Now()); err != nil {
		t.Fatal("durable result hidden because reserve could not be replenished", err)
	}
	attempts, err := j.List()
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != Accepted {
		t.Fatal("committed result unavailable for reconciliation", attempts, err)
	}
	if remaining, err := j.Remaining(); err != nil || remaining != 0 {
		t.Fatal("advertised capacity without a reserve", remaining, err)
	}
	if err = j.Begin("another", "another-delivery", fence, time.Now()); !errors.Is(err, ErrFull) {
		t.Fatal("accepted a new attempt without a reserve", err)
	}
	if err = j.Acknowledge(attempts[0]); err != nil {
		t.Fatal("could not acknowledge a committed outcome", err)
	}
	r.full = false
	if remaining, err := j.Remaining(); err != nil || remaining != 3 {
		t.Fatal("did not resume after reserve was restored", remaining, err)
	}
}

func TestReserveReleaseFailureLatchesJournal(t *testing.T) {
	j, err := Open(filepath.Join(t.TempDir(), "journal.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	r := &testReserve{armed: true}
	j.reserve = r
	fence := Fence{ControllerID: "controller", Epoch: 1}
	if err = j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("attempt", "delivery", fence, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.releaseFail = true
	if err = j.Complete("attempt", Accepted, 250, time.Now()); err == nil {
		t.Fatal("reserve release failure was ignored")
	}
	if _, err = j.Remaining(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("journal did not fail closed after reserve failure", err)
	}
}
