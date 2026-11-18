package webhook

import (
	"context"
	"errors"
	"testing"
	"time"
)

type runtimeSource struct {
	calls int
	stop  context.CancelFunc
}

func (s *runtimeSource) DispatchNext(context.Context) error {
	s.calls++
	if s.calls == 1 {
		return nil
	}
	if s.calls == 2 {
		return ErrNoWork
	}
	s.stop()
	return errors.New("temporary failure")
}
func TestSchedulerDrainsAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &runtimeSource{stop: cancel}
	if err := Run(ctx, source, time.Millisecond, nil); !errors.Is(err, context.Canceled) || source.calls != 3 {
		t.Fatal("scheduler did not drain and stop", err)
	}
}
