package delivery

import (
	"errors"
	"time"
)

// RetryPolicy is stored in the database admission gate and shared by replicas.
type RetryPolicy struct {
	MaxAttempts  int
	InitialDelay time.Duration
	MaxDelay     time.Duration
	MaxAge       time.Duration
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 128, InitialDelay: 30 * time.Minute, MaxDelay: 3 * time.Hour, MaxAge: 5 * 24 * time.Hour}
}

func (p RetryPolicy) Validate() error {
	if p.MaxAttempts < 1 || p.MaxAttempts > 1000 || p.InitialDelay < time.Minute || p.InitialDelay > 24*time.Hour || p.MaxDelay < p.InitialDelay || p.MaxDelay > 24*time.Hour || p.MaxAge < time.Hour || p.MaxAge > 30*24*time.Hour || p.InitialDelay > p.MaxAge || p.InitialDelay%time.Millisecond != 0 || p.MaxDelay%time.Millisecond != 0 || p.MaxAge%time.Millisecond != 0 {
		return errors.New("invalid delivery retry policy")
	}
	return nil
}
