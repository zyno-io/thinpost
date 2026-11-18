package controlapp

import (
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/delivery"
)

func TestDeliveryRetryConfiguration(t *testing.T) {
	values := env()
	get := func(name string) string { return values[name] }
	policy, err := DeliveryRetryPolicy(get)
	if err != nil || policy != delivery.DefaultRetryPolicy() {
		t.Fatalf("default retry policy: %+v, %v", policy, err)
	}
	values["THINPOST_DELIVERY_MAX_ATTEMPTS"] = "5"
	values["THINPOST_DELIVERY_RETRY_INITIAL_DELAY"] = "2m"
	values["THINPOST_DELIVERY_RETRY_MAX_DELAY"] = "1h"
	values["THINPOST_DELIVERY_RETRY_MAX_AGE"] = "48h"
	policy, err = DeliveryRetryPolicy(get)
	if err != nil || policy != (delivery.RetryPolicy{MaxAttempts: 5, InitialDelay: 2 * time.Minute, MaxDelay: time.Hour, MaxAge: 48 * time.Hour}) {
		t.Fatalf("custom retry policy: %+v, %v", policy, err)
	}
	if _, err = load(values); err == nil {
		t.Fatal("runtime accepted obsolete per-process policy")
	}
	for name, value := range map[string]string{
		"THINPOST_DELIVERY_MAX_ATTEMPTS":        "0",
		"THINPOST_DELIVERY_RETRY_INITIAL_DELAY": "nonsense",
		"THINPOST_DELIVERY_RETRY_MAX_DELAY":     "1m",
		"THINPOST_DELIVERY_RETRY_MAX_AGE":       "30m",
	} {
		original := values[name]
		values[name] = value
		if _, err = DeliveryRetryPolicy(get); err == nil {
			t.Errorf("accepted invalid %s", name)
		}
		values[name] = original
	}
}
