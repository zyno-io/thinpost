package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/deployhook"
)

func TestTerminationLogCarriesOnlyFixedReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termination-log")
	previous := terminationLog
	terminationLog = path
	defer func() { terminationLog = previous }()
	writeTermination(&deployhook.Failure{Reason: "migrate_failed", Detail: "deploy gate: migration_failed migration=28 sqlstate=42P07"})
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "migrate_failed (deploy gate: migration_failed migration=28 sqlstate=42P07)" {
		t.Fatal(string(body), err)
	}
	writeTermination(errors.New("password=hunter2 at 10.0.0.1"))
	body, _ = os.ReadFile(path)
	if string(body) != "internal_error" || strings.Contains(string(body), "hunter2") {
		t.Fatal(string(body))
	}
}

func TestDeployCommandsValidateArguments(t *testing.T) {
	previous := terminationLog
	terminationLog = filepath.Join(t.TempDir(), "termination-log")
	defer func() { terminationLog = previous }()
	t.Setenv("THINPOST_RUNTIME_ROLE", "")
	for _, run := range []func([]string) error{deployMigrate, deployReopen, gate} {
		if run([]string{}) == nil || run([]string{"--unknown"}) == nil {
			t.Fatal("accepted missing or unknown arguments")
		}
	}
	if deployReopen([]string{"--actor", "deploy:a/b:r1", "--namespace", "ns", "--selector", "a=b", "--image", "x", "--replicas", "0"}) == nil {
		t.Fatal("accepted zero replicas")
	}
	if gate([]string{"open", "--actor", "a", "--expected-revision", "1", "--generation", "g", "--expected-schema", "28", "--role", "r"}) == nil {
		t.Fatal("gate open ran without the rollout attestation")
	}
	if gate([]string{"bogus"}) == nil || gate([]string{"close", "--actor", "a"}) == nil {
		t.Fatal("accepted an invalid gate command")
	}
}
