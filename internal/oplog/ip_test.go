package oplog

import (
	"strings"
	"testing"
)

// The log names a sending IP by its HELO name, never its address, and nothing that is not a
// valid name can pass through.
func TestIPNameNeverEmitsAddressesOrFreeText(t *testing.T) {
	for name, want := range map[string]string{
		"": "primary", "mx.example.net": "mx.example.net", "smtp-2.hosting.example.org": "smtp-2.hosting.example.org", "a": "a", "removed": "removed",
		"45.0.1.22": "other", "045.0." + "1.22": "other", "45.0.1": "other", "45-0-1-22.example": "45-0-1-22.example", "1.2.3.4.5": "other", "45.0.1.22.example": "other", "mx.45.0.1.22.example": "other", "1.2.3.example": "1.2.3.example", "smtp.2.3.4.example": "smtp.2.3.4.example",
		"Mx.example.net": "other", "-x.example.net": "other", "x-.example.net": "other", "a..b": "other", ".a": "other", "a.": "other",
		"two words": "other", "user@example.com": "other", "::1": "other", "x\r\ny": "other",
		strings.Repeat("a", 64) + ".example.net": "other",
		strings.Repeat("a.", 126) + "ab":         "other",
	} {
		if got := IPName(name); got != want {
			t.Errorf("IPName(%q)=%q, want %q", name, got, want)
		}
	}
	if got := IPName(strings.Repeat("a.", 100) + "net"); got == "other" {
		t.Error("a valid long name was rejected")
	}
	lines := capture(t)
	Delivery(app, msg, del, OutcomeAccepted, 250, "45.0.1.22")
	if strings.Contains(strings.Join(*lines, ""), "45.0.1") || !strings.HasSuffix((*lines)[0], "ip=other") {
		t.Fatalf("address reached the log: %v", *lines)
	}
}
