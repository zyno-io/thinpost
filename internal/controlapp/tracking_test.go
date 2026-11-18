package controlapp

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestTrackingConfigurationRequiresExplicitKeysAndSubmission(t *testing.T) {
	values := env()
	config, err := load(values)
	if err != nil || config.Tracking != nil {
		t.Fatal("tracking enabled by default", err)
	}
	path := filepath.Join(t.TempDir(), "tracking.json")
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if err = os.WriteFile(path, []byte(`{"seed":"`+seed+`","previous_public_keys":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{"THINPOST_CONTROL_FINGERPRINT_KEYS_FILE": "/keyring", "THINPOST_TRACKING_ORIGIN": "https://track.example.com/", "THINPOST_TRACKING_LINK_KEYS_FILE": path, "THINPOST_TRACKING_EVENT_LIMIT": "10"}
	for key, value := range base {
		values[key] = value
	}
	config, err = load(values)
	if err != nil || config.Tracking == nil || config.Tracking.Origin != "https://track.example.com" || config.Tracking.Links == nil || config.Tracking.EventLimit != 10 {
		t.Fatal("valid tracking configuration rejected", err)
	}
	for _, tc := range []struct{ key, value string }{
		{"THINPOST_CONTROL_FINGERPRINT_KEYS_FILE", ""}, {"THINPOST_TRACKING_ORIGIN", ""},
		{"THINPOST_TRACKING_ORIGIN", "https://track.example.com/path"},
		{"THINPOST_TRACKING_LINK_KEYS_FILE", "relative.json"}, {"THINPOST_TRACKING_LINK_KEYS_FILE", path + ".missing"},
		{"THINPOST_TRACKING_EVENT_LIMIT", "0"}, {"THINPOST_TRACKING_EVENT_LIMIT", "1001"},
		{"THINPOST_TRACKING_QUEUE_WINDOW", "24h"}, {"THINPOST_TRACKING_POST_SEND_VALIDITY", "2160h"},
	} {
		values = env()
		for key, value := range base {
			values[key] = value
		}
		values[tc.key] = tc.value
		if _, err = load(values); err == nil {
			t.Fatalf("invalid configuration accepted for %s", tc.key)
		}
	}
}
