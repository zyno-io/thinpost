package postgres

import (
	"fmt"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/delivery"
)

func TestDeliveryRetryDelayIsBoundedAndJittered(t *testing.T) {
	policy := delivery.DefaultRetryPolicy()
	first := smtpRetryDelay(1, "attempt-1", policy)
	if first < 30*time.Minute || first > 36*time.Minute || first != smtpRetryDelay(1, "attempt-1", policy) {
		t.Fatal("invalid first retry delay")
	}
	var different bool
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("attempt-%d", i)
		if smtpRetryDelay(1, id, policy) != first {
			different = true
		}
		if delay := smtpRetryDelay(100, id, policy); delay <= 0 || delay > policy.MaxDelay {
			t.Fatal("unbounded retry delay", delay)
		}
	}
	if !different {
		t.Fatal("retry delays have no jitter")
	}
}
