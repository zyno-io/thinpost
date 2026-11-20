package recipientpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllowlistIsExactAndFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipients")
	if err := os.WriteFile(path, []byte("# staging test mailboxes\nAlice@Example.COM\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"THINPOST_RECIPIENT_POLICY": "allowlist", "THINPOST_RECIPIENT_ALLOWLIST_FILE": path}
	get := func(key string) string { return env[key] }
	policy, err := Load(get)
	if err != nil {
		t.Fatal(err)
	}
	for address, want := range map[string]bool{"Alice@example.com": true, "Example <Alice@EXAMPLE.COM>": true, "alice@example.com": false, "Alice+extra@example.com": false, "Alice@example.com.evil.test": false, "Alice@example.com\r\nBcc: b@example.com": false} {
		if got := policy.Allows(address); got != want {
			t.Errorf("Allows(%q) = %v", address, got)
		}
	}
	for _, body := range []string{"", "# no addresses\n", "not an address\n", strings.Repeat("x", 65537)} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(get); err == nil {
			t.Fatalf("accepted invalid allowlist length %d", len(body))
		}
	}
	env["THINPOST_RECIPIENT_POLICY"] = ""
	if _, err := Load(get); err == nil {
		t.Fatal("accepted missing mode")
	}
	env["THINPOST_RECIPIENT_POLICY"] = "unrestricted"
	if _, err := Load(get); err == nil {
		t.Fatal("accepted contradictory allowlist")
	}
	env["THINPOST_RECIPIENT_ALLOWLIST_FILE"] = ""
	policy, err = Load(get)
	if err != nil || !policy.Allows("user@example.net") || policy.Allows("invalid") {
		t.Fatal("bad unrestricted policy", err)
	}
}

func TestModeNamesThePolicy(t *testing.T) {
	var none *Policy
	if none.Mode() != "" || none.Unrestricted() {
		t.Fatal("nil policy")
	}
	open, err := Load(func(key string) string {
		if key == "THINPOST_RECIPIENT_POLICY" {
			return "unrestricted"
		}
		return ""
	})
	if err != nil || open.Mode() != "unrestricted" || !open.Unrestricted() {
		t.Fatal("unrestricted", err)
	}
	path := filepath.Join(t.TempDir(), "recipients")
	if err = os.WriteFile(path, []byte("alice@example.com\n"), 0600); err != nil {
		t.Fatal(err)
	}
	listed, err := Load(func(key string) string {
		return map[string]string{"THINPOST_RECIPIENT_POLICY": "allowlist", "THINPOST_RECIPIENT_ALLOWLIST_FILE": path}[key]
	})
	if err != nil || listed.Mode() != "allowlist" || listed.Unrestricted() {
		t.Fatal("allowlist", err)
	}
}
