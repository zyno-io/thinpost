package main

import (
	"strings"
	"testing"
)

func TestBootstrapRequiresExplicitInputsBeforeConnecting(t *testing.T) {
	t.Setenv("THINPOST_DATABASE_URL", "")
	for _, args := range [][]string{nil, {"--database", "thinpost"}, {"--unknown"}, {"--database", "x", "extra"}} {
		if err := bootstrapDatabase(args); err == nil || !strings.Contains(err.Error(), "requires --database") {
			t.Fatal("unexpected bootstrap command response", args, err)
		}
	}
}
