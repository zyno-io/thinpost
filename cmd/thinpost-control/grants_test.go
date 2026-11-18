package main

import (
	"strings"
	"testing"
)

func TestGrantRuntimeRequiresRoleBeforeConnecting(t *testing.T) {
	t.Setenv("THINPOST_DATABASE_URL", "")
	for _, args := range [][]string{nil, {"--role"}, {"--role", "name", "extra"}, {"--unknown"}} {
		if err := grantRuntime(args); err == nil || !strings.Contains(err.Error(), "requires --role") {
			t.Fatal("unexpected grant command response", args, err)
		}
	}
}

func TestGrantCollectorRequiresRoleBeforeConnecting(t *testing.T) {
	t.Setenv("THINPOST_DATABASE_URL", "")
	for _, args := range [][]string{nil, {"--role"}, {"--role", "name", "extra"}, {"--unknown"}} {
		if err := grantCollector(args); err == nil || !strings.Contains(err.Error(), "requires --role") {
			t.Fatal("unexpected grant command response", args, err)
		}
	}
}

func TestGrantTrackingRetirementRequiresRoleBeforeConnecting(t *testing.T) {
	t.Setenv("THINPOST_DATABASE_URL", "")
	for _, args := range [][]string{nil, {"--role"}, {"--role", "name", "extra"}, {"--unknown"}} {
		if err := grantTrackingRetirement(args); err == nil || !strings.Contains(err.Error(), "requires --role") {
			t.Fatal("unexpected grant command response", args, err)
		}
	}
}

func TestRetireTrackingRequiresBoundedArgumentsBeforeConnecting(t *testing.T) {
	t.Setenv("THINPOST_DATABASE_URL", "")
	for _, args := range [][]string{nil, {"--actor", "operator"}, {"--actor", "operator", "--generation", "gen", "--limit", "0"}, {"--actor", "operator", "--generation", "gen", "--limit", "101"}, {"--unknown"}} {
		if err := retireTracking(args); err == nil || !strings.Contains(err.Error(), "requires --actor") {
			t.Fatal("unexpected retirement command response", args, err)
		}
	}
}
