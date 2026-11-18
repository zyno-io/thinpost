package main

import (
	"strings"
	"testing"
)

func TestRecoveryCommandsRequireAttestationsBeforeConnecting(t *testing.T) {
	t.Setenv("THINPOST_DATABASE_URL", "")
	for _, test := range []struct {
		args []string
		want string
	}{
		{nil, "missing recovery operation"},
		{[]string{"unknown"}, "unknown recovery operation"},
		{[]string{"begin"}, "attestations"},
		{[]string{"begin", "--workers-fenced"}, "attestations"},
		{[]string{"migrate-restored-schema"}, "requires fencing"},
		{[]string{"migrate-restored-schema", "--workers-fenced", "--database-isolated"}, "requires fencing"},
		{[]string{"open", "--workers-fenced", "--database-isolated"}, "attestations"},
		{[]string{"report"}, "requires generation"},
		{[]string{"report", "--generation", "abc", "--recovery-id", "abc", "--expected-revision", "1", "--limit", "501"}, "requires generation"},
		{[]string{"hold-history"}, "requires generation"},
		{[]string{"hold-history", "--generation", "abc", "--recovery-id", "abc", "--expected-revision", "1", "--kind", "message", "--entity-id", "m", "--limit", "101"}, "requires generation"},
		{[]string{"objects"}, "requires fencing"},
		{[]string{"objects", "--workers-fenced", "--database-isolated", "--objects-frozen", "--generation", "abc", "--recovery-id", "abc", "--expected-revision", "1", "--limit", "101"}, "requires fencing"},
	} {
		if err := recovery(test.args); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatal("unexpected recovery command response", test.args, err)
		}
	}
}
