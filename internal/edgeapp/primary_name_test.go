package edgeapp

import (
	"strings"
	"testing"
)

func TestEdgeConfigPrimaryNameIsABounceDomain(t *testing.T) {
	env := sourcesEnv()
	get := func(key string) string { return env[key] }
	// The primary's name is accepted as a bounce domain on its own, and is not
	// repeated when it equals the global bounce domain.
	env["THINPOST_EDGE_BOUNCE_DOMAIN"] = "smtp.example.com"
	c, err := Load(get)
	if err != nil || len(c.BounceDomains) != 1 || c.BounceDomains[0] != "smtp.example.com" {
		t.Fatalf("primary equal to the global domain: %v %v", c.BounceDomains, err)
	}
	// An extra repeating the primary's name is refused, as the chart refuses it.
	env["THINPOST_EDGE_BOUNCE_DOMAIN"] = "bounce.example.com"
	env["THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS"] = "smtp.example.com"
	if _, err = Load(get); err == nil {
		t.Fatal("an extra bounce domain repeating the primary name was accepted")
	}
}

func TestEdgeConfigRejectsALongPrimaryHostname(t *testing.T) {
	env := sourcesEnv()
	env["THINPOST_EDGE_SMTP_HOSTNAME"] = strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 40) + ".example"
	if _, err := Load(func(key string) string { return env[key] }); err == nil {
		t.Fatal("a primary hostname over 219 bytes was accepted")
	}
}
