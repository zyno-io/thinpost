package controlbridge

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBounceKeyringRejectsDuplicateVersionsAndBadSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	value := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"v1":"` + value + `"}`, true},
		{`{"v1":"` + value + `","v1":"` + value + `"}`, false},
		{`{"v1":"c2hvcnQ="}`, false},
		{`{"v1":"` + value + `"} {}`, false},
		{strings.Repeat(" ", 16385), false},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		keys, err := readKeys(path)
		if (err == nil) != tc.valid {
			t.Fatalf("unexpected keyring validation: %v", err)
		}
		if tc.valid && len(keys["v1"]) != 32 {
			t.Fatal("key not decoded")
		}
	}
}

func TestBridgeIsDisabledOnlyWithoutPartialConfiguration(t *testing.T) {
	env := map[string]string{}
	get := func(key string) string { return env[key] }
	if config, err := FromEnvironment(get); err != nil || config != nil {
		t.Fatal("unexpected bridge configuration", err)
	}
	env["THINPOST_CONTROL_WORKER_ID"] = "worker"
	if _, err := FromEnvironment(get); err == nil {
		t.Fatal("partial bridge configuration accepted")
	}
	delete(env, "THINPOST_CONTROL_WORKER_ID")
	env["THINPOST_CONTROL_JOURNAL_ID"] = "0123456789abcdef0123456789abcdef"
	if _, err := FromEnvironment(get); err == nil {
		t.Fatal("journal identity silently accepted with bridge disabled")
	}
	delete(env, "THINPOST_CONTROL_JOURNAL_ID")
	env["THINPOST_CONTROL_ROUTE_EXCEPTIONS_FILE"] = "exceptions"
	if _, err := FromEnvironment(get); err == nil {
		t.Fatal("route exceptions silently accepted with bridge disabled")
	}
}
