package controlapp

import (
	"errors"
	"strconv"
	"time"

	"github.com/zyno-io/thinpost/internal/delivery"
)

// DeliveryRetryPolicy parses the complete replacement policy for maintenance.
func DeliveryRetryPolicy(getenv func(string) string) (delivery.RetryPolicy, error) {
	policy := delivery.DefaultRetryPolicy()
	if value := getenv("THINPOST_DELIVERY_MAX_ATTEMPTS"); value != "" {
		attempts, err := strconv.Atoi(value)
		if err != nil {
			return delivery.RetryPolicy{}, errors.New("invalid delivery retry policy")
		}
		policy.MaxAttempts = attempts
	}
	for _, item := range []struct {
		name  string
		value *time.Duration
	}{
		{"THINPOST_DELIVERY_RETRY_INITIAL_DELAY", &policy.InitialDelay},
		{"THINPOST_DELIVERY_RETRY_MAX_DELAY", &policy.MaxDelay},
		{"THINPOST_DELIVERY_RETRY_MAX_AGE", &policy.MaxAge},
	} {
		if value := getenv(item.name); value != "" {
			duration, err := time.ParseDuration(value)
			if err != nil {
				return delivery.RetryPolicy{}, errors.New("invalid delivery retry policy")
			}
			*item.value = duration
		}
	}
	if err := policy.Validate(); err != nil {
		return delivery.RetryPolicy{}, err
	}
	return policy, nil
}
