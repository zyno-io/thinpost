package edgeapp

import (
	"fmt"
	"strings"
	"testing"
)

func sourcesEnv() map[string]string {
	return map[string]string{
		"THINPOST_EDGE_WORKER_ID": "worker-1", "THINPOST_EDGE_SMTP_HOSTNAME": "smtp.example.com", "THINPOST_EDGE_BOUNCE_DOMAIN": "bounce.example.com",
		"THINPOST_EDGE_CONTROL_LISTEN": "10.2.3.4:7443", "THINPOST_EDGE_SMTP_LISTEN": "45.0.1.22:25", "THINPOST_EDGE_INFRASTRUCTURE_CIDRS": "45.0.1.0/24,45.0.0.0/24,10.0.0.0/8",
		"THINPOST_EDGE_CONTROLLER_IDENTITIES": "spiffe://thinpost.test/controller/a", "THINPOST_EDGE_CONTROL_CA": "ca.pem", "THINPOST_EDGE_CONTROL_CERT": "control.pem", "THINPOST_EDGE_CONTROL_KEY": "control.key",
		"THINPOST_EDGE_SMTP_CERT": "smtp.pem", "THINPOST_EDGE_SMTP_KEY": "smtp.key", "THINPOST_EDGE_JOURNAL_PATH": "/data/journal", "THINPOST_EDGE_JOURNAL_CAPACITY": "100", "THINPOST_EDGE_CLOCK_MARGIN": "5s",
		"THINPOST_EDGE_JOURNAL_RESERVE_BYTES": "67108864",
	}
}

// withSources sets the additional sources with a valid hostname for each and a
// certificate root, so a test exercises only the rule it names.
func withSources(env map[string]string, sources string) {
	env["THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES"] = sources
	var names []string
	for i := range strings.Split(sources, ",") {
		names = append(names, fmt.Sprintf("smtp%d.example.net", i+1))
	}
	env["THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES"] = strings.Join(names, ",")
	env["THINPOST_EDGE_SMTP_ADDITIONAL_CERT_ROOT"] = "/run/thinpost/smtp-sources"
}

func TestEdgeConfigAdditionalSourcesAndBounceDomains(t *testing.T) {
	env := sourcesEnv()
	get := func(key string) string { return env[key] }
	c, err := Load(get)
	if err != nil || len(c.AdditionalSources) != 0 || len(c.Sources()) != 1 || len(c.BounceDomains) != 2 || c.BounceDomains[0] != "bounce.example.com" || c.BounceDomains[1] != "smtp.example.com" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	withSources(env, "45.0.0.10, 45.0.1.23")
	env["THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS"] = "bounce.other.example, b2.example.org"
	c, err = Load(get)
	if err != nil || len(c.Sources()) != 3 || c.Sources()[0].String() != "45.0.1.22" || c.Sources()[1].String() != "45.0.0.10" {
		t.Fatalf("configured sources: %v %v", c.Sources(), err)
	}
	// Global, then the primary and both additional listener names, then the explicit extras.
	if len(c.BounceDomains) != 6 || c.BounceDomains[0] != "bounce.example.com" || c.BounceDomains[1] != "smtp.example.com" || c.BounceDomains[2] != "smtp1.example.net" || c.BounceDomains[3] != "smtp2.example.net" || c.BounceDomains[5] != "b2.example.org" {
		t.Fatalf("configured domains: %v", c.BounceDomains)
	}
	var many []string
	// 22 extras, the global domain and the primary name are exactly the cap of 24.
	env["THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS"] = manyDomains(22)
	delete(env, "THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES")
	delete(env, "THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES")
	if c, err = Load(get); err != nil || len(c.BounceDomains) != MaxBounceDomains {
		t.Fatalf("a full list of bounce domains: %v %v", len(c.BounceDomains), err)
	}
	env["THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS"] = ""
	for i := 1; i <= 16; i++ {
		many = append(many, fmt.Sprintf("45.0.0.%d", i+10))
	}
	for name, tc := range map[string][2]string{
		"duplicate of the primary": {"THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES", "45.0.1.22"},
		"duplicate additional":     {"THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES", "45.0.0.10,45.0.0.10"},
		"outside infrastructure":   {"THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES", "8.8.8.8"},
		"private":                  {"THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES", "10.1.1.1"},
		"IPv6":                     {"THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES", "2607:f8b0:4000::1"},
		"not an address":           {"THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES", "mail.example.com"},
		"empty entry":              {"THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES", "45.0.0.10,"},
		"sixteen additional":       {"THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES", strings.Join(many, ",")},
		"uppercase bounce domain":  {"THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS", "Bounce.Other.example"},
		"duplicate of global":      {"THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS", "bounce.example.com"},
		"duplicate bounce domain":  {"THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS", "a.example.org,a.example.org"},
		"not a domain":             {"THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS", "not a domain"},
		"too long bounce domain":   {"THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS", strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 40) + ".example"},
		"twenty-three extras plus global and primary": {"THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS", manyDomains(23)},
	} {
		env := sourcesEnv()
		if tc[0] == "THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES" {
			withSources(env, tc[1])
		} else {
			env[tc[0]] = tc[1]
		}
		if _, err := Load(func(key string) string { return env[key] }); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEdgeConfigRequiresAnIPv4Primary(t *testing.T) {
	env := sourcesEnv()
	env["THINPOST_EDGE_SMTP_LISTEN"] = "[2607:f8b0:4000::5]:25"
	env["THINPOST_EDGE_INFRASTRUCTURE_CIDRS"] = "2607:f8b0:4000::/48,10.0.0.0/8"
	if _, err := Load(func(key string) string { return env[key] }); err == nil {
		t.Fatal("accepted an IPv6 primary, which control would reject in Ready")
	}
}

func manyDomains(n int) string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("a%d.example.org", i))
	}
	return strings.Join(out, ",")
}

func TestEdgeConfigListenersAndHostnames(t *testing.T) {
	env := sourcesEnv()
	withSources(env, "45.0.0.10,45.0.1.23")
	env["THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES"] = "smtp.hosting.example,smtp.other.example"
	env["THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS"] = "smtp.hosting.example"
	if _, err := Load(func(key string) string { return env[key] }); err == nil {
		t.Fatal("an extra bounce domain repeating a listener name was accepted")
	}
	env["THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS"] = "old.example.org"
	c, err := Load(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	listeners := c.Listeners()
	if len(listeners) != 3 || listeners[0].Address.String() != "45.0.1.22" || listeners[0].Hostname != "smtp.example.com" || listeners[0].Cert != "smtp.pem" || listeners[0].Key != "smtp.key" {
		t.Fatalf("primary listener: %+v", listeners)
	}
	if listeners[1].Hostname != "smtp.hosting.example" || listeners[1].Cert != "/run/thinpost/smtp-sources/0/tls.crt" || listeners[2].Key != "/run/thinpost/smtp-sources/1/tls.key" {
		t.Fatalf("additional listeners: %+v", listeners)
	}
	want := []string{"bounce.example.com", "smtp.example.com", "smtp.hosting.example", "smtp.other.example", "old.example.org"}
	if strings.Join(c.BounceDomains, ",") != strings.Join(want, ",") {
		t.Fatalf("bounce domains: %v", c.BounceDomains)
	}
	// A listener name equal to the global bounce domain is not repeated.
	env["THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES"] = "bounce.example.com,smtp.other.example"
	if c, err = Load(func(key string) string { return env[key] }); err != nil || strings.Join(c.BounceDomains, ",") != "bounce.example.com,smtp.example.com,smtp.other.example,old.example.org" {
		t.Fatalf("deduplicated domains: %v %v", c.BounceDomains, err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"missing hostnames": func(e map[string]string) { delete(e, "THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES") },
		"too few hostnames": func(e map[string]string) { e["THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES"] = "smtp.a.example" },
		"too many hostnames": func(e map[string]string) {
			e["THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES"] = "a.example,b.example,c.example"
		},
		"hostnames without sources": func(e map[string]string) { delete(e, "THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES") },
		"uppercase hostname":        func(e map[string]string) { e["THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES"] = "SMTP.a.example,b.example" },
		"duplicate hostname":        func(e map[string]string) { e["THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES"] = "a.example,a.example" },
		"primary hostname":          func(e map[string]string) { e["THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES"] = "smtp.example.com,b.example" },
		"not a hostname":            func(e map[string]string) { e["THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES"] = "not a name,b.example" },
		"hostname too long": func(e map[string]string) {
			e["THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES"] = strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 40) + ".example,b.example"
		},
		"missing certificate root":  func(e map[string]string) { delete(e, "THINPOST_EDGE_SMTP_ADDITIONAL_CERT_ROOT") },
		"relative certificate root": func(e map[string]string) { e["THINPOST_EDGE_SMTP_ADDITIONAL_CERT_ROOT"] = "smtp-sources" },
		"unclean certificate root":  func(e map[string]string) { e["THINPOST_EDGE_SMTP_ADDITIONAL_CERT_ROOT"] = "/run/thinpost/../smtp" },
	} {
		e := sourcesEnv()
		withSources(e, "45.0.0.10,45.0.1.23")
		mutate(e)
		if _, err := Load(func(key string) string { return e[key] }); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Names without any additional source are a mistake, not a no-op.
	e := sourcesEnv()
	e["THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES"] = "smtp.a.example"
	if _, err := Load(func(key string) string { return e[key] }); err == nil {
		t.Error("hostnames without sources accepted")
	}
}

func TestEdgeConfigSourceGateways(t *testing.T) {
	env := sourcesEnv()
	c, err := Load(func(k string) string { return env[k] })
	if err != nil || len(c.SourceGateways) != 0 {
		t.Fatal("gateways are optional", err)
	}
	env["THINPOST_EDGE_SOURCE_GATEWAYS"] = "45.0.1.1"
	if c, err = Load(func(k string) string { return env[k] }); err != nil || len(c.SourceGateways) != 1 {
		t.Fatal("one gateway for the primary", err)
	}
	withSources(env, "45.0.0.10, 45.0.1.23")
	for name, value := range map[string]string{"too few": "45.0.1.1", "too many": "1.1.1.1,45.0.0.2,45.0.0.3,45.0.0.4", "IPv6": "2607:f8b0::1,1.1.1.1,45.0.0.2", "name": "gw,1.1.1.1,45.0.0.2", "empty entry": "1.1.1.1,,45.0.0.2"} {
		env["THINPOST_EDGE_SOURCE_GATEWAYS"] = value
		if _, err = Load(func(k string) string { return env[k] }); err == nil {
			t.Fatal("accepted", name)
		}
	}
	env["THINPOST_EDGE_SOURCE_GATEWAYS"] = "45.0.1.1, 45.0.0.1, 45.0.1.1"
	if c, err = Load(func(k string) string { return env[k] }); err != nil || len(c.SourceGateways) != 3 || c.SourceGateways[1].String() != "45.0.0.1" {
		t.Fatal("three gateways", err)
	}
}
