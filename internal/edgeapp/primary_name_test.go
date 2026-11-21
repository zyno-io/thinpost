package edgeapp

import (
	"strings"
	"testing"
)

func TestEdgeConfigRejectsALongPrimaryHostname(t *testing.T) {
	env := sourcesEnv()
	env["THINPOST_EDGE_SMTP_HOSTNAME"] = strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 40) + ".example"
	if _, err := Load(func(key string) string { return env[key] }); err == nil {
		t.Fatal("a primary hostname over 219 bytes was accepted")
	}
}
