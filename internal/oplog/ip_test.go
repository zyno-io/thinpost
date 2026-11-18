package oplog

import (
	"strings"
	"testing"
)

// The log names a sending IP, never its address, and nothing that is not a
// valid name can pass through.
func TestIPNameNeverEmitsAddressesOrFreeText(t *testing.T) {
	for name, want := range map[string]string{
		"": "primary", "hosting": "hosting", "app-mail-2": "app-mail-2", "a": "a",
		"45.0.1.22": "other", "45-0-1-22.example": "other", "Hosting": "other", "-x": "other", "x-": "other",
		"two words": "other", "user@example.com": "other", strings.Repeat("a", 33): "other", "::1": "other", "x\r\ny": "other",
	} {
		if got := IPName(name); got != want {
			t.Errorf("IPName(%q)=%q, want %q", name, got, want)
		}
	}
	lines := capture(t)
	Delivery(app, msg, del, OutcomeAccepted, 250, "45.0.1.22")
	if strings.Contains(strings.Join(*lines, ""), "45.0.1") || !strings.HasSuffix((*lines)[0], "ip=other") {
		t.Fatalf("address reached the log: %v", *lines)
	}
}
