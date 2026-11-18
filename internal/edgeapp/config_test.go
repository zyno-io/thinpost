package edgeapp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/smtptransport"
)

func TestEdgeConfigRequiresExplicitPrivateControlAndOwnedPublicSource(t *testing.T) {
	env := map[string]string{
		"THINPOST_EDGE_WORKER_ID": "worker-1", "THINPOST_EDGE_SMTP_HOSTNAME": "smtp.example.com", "THINPOST_EDGE_BOUNCE_DOMAIN": "bounce.example.com",
		"THINPOST_EDGE_CONTROL_LISTEN": "10.2.3.4:7443", "THINPOST_EDGE_SMTP_LISTEN": "45.0.1.22:25", "THINPOST_EDGE_INFRASTRUCTURE_CIDRS": "45.0.1.0/24,10.0.0.0/8",
		"THINPOST_EDGE_CONTROLLER_IDENTITIES": "spiffe://thinpost.test/controller/a", "THINPOST_EDGE_CONTROL_CA": "ca.pem", "THINPOST_EDGE_CONTROL_CERT": "control.pem", "THINPOST_EDGE_CONTROL_KEY": "control.key",
		"THINPOST_EDGE_SMTP_CERT": "smtp.pem", "THINPOST_EDGE_SMTP_KEY": "smtp.key", "THINPOST_EDGE_JOURNAL_PATH": "/data/journal", "THINPOST_EDGE_JOURNAL_CAPACITY": "100", "THINPOST_EDGE_CLOCK_MARGIN": "5s",
		"THINPOST_EDGE_JOURNAL_RESERVE_BYTES": "67108864",
	}
	get := func(key string) string { return env[key] }
	defaults, err := Load(get)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.SMTPGlobalInterval != 100*time.Millisecond || defaults.SMTPGlobalBurst != 100 || defaults.SMTPSourceInterval != time.Second || defaults.SMTPSourceBurst != 12 {
		t.Fatal("unexpected SMTP connection rate defaults")
	}
	if defaults.OutboundSMTPTimeouts != smtptransport.DefaultTimeouts() {
		t.Fatal("unexpected SMTP stage defaults")
	}
	env["THINPOST_EDGE_SMTP_DATA_FINAL_TIMEOUT"] = "12m"
	customTimeouts, err := Load(get)
	if err != nil || customTimeouts.OutboundSMTPTimeouts.DataFinal != 12*time.Minute {
		t.Fatal("SMTP timeout not configured", err)
	}
	delete(env, "THINPOST_EDGE_SMTP_DATA_FINAL_TIMEOUT")
	env["THINPOST_EDGE_SMTP_GLOBAL_INTERVAL"] = "250ms"
	env["THINPOST_EDGE_SMTP_GLOBAL_BURST"] = "50"
	env["THINPOST_EDGE_SMTP_SOURCE_INTERVAL"] = "2s"
	env["THINPOST_EDGE_SMTP_SOURCE_BURST"] = "7"
	configured, err := Load(get)
	if err != nil || configured.SMTPGlobalInterval != 250*time.Millisecond || configured.SMTPGlobalBurst != 50 || configured.SMTPSourceInterval != 2*time.Second || configured.SMTPSourceBurst != 7 {
		t.Fatal("SMTP connection rates were not configured", err)
	}
	delete(env, "THINPOST_EDGE_SMTP_GLOBAL_INTERVAL")
	delete(env, "THINPOST_EDGE_SMTP_GLOBAL_BURST")
	delete(env, "THINPOST_EDGE_SMTP_SOURCE_INTERVAL")
	delete(env, "THINPOST_EDGE_SMTP_SOURCE_BURST")
	for _, tc := range []struct{ key, value string }{
		{"THINPOST_EDGE_CONTROL_LISTEN", "0.0.0.0:7443"},
		{"THINPOST_EDGE_CONTROL_LISTEN", "45.0.1.22:7443"},
		{"THINPOST_EDGE_SMTP_LISTEN", "45.0.1.22:2525"},
		{"THINPOST_EDGE_SMTP_LISTEN", "127.0.0.1:25"},
		{"THINPOST_EDGE_INFRASTRUCTURE_CIDRS", "10.0.0.0/8"},
		{"THINPOST_EDGE_SMTP_HOSTNAME", "SMTP.example.com"},
		{"THINPOST_EDGE_JOURNAL_CAPACITY", "0"},
		{"THINPOST_EDGE_JOURNAL_RESERVE_BYTES", ""},
		{"THINPOST_EDGE_JOURNAL_RESERVE_BYTES", "1048575"},
		{"THINPOST_EDGE_JOURNAL_RESERVE_BYTES", "1073741825"},
		{"THINPOST_EDGE_CLOCK_MARGIN", "31s"},
		{"THINPOST_EDGE_SMTP_GLOBAL_INTERVAL", "0s"},
		{"THINPOST_EDGE_SMTP_SOURCE_INTERVAL", "1ms"},
		{"THINPOST_EDGE_SMTP_SOURCE_INTERVAL", "2h"},
		{"THINPOST_EDGE_SMTP_GLOBAL_BURST", "0"},
		{"THINPOST_EDGE_SMTP_SOURCE_BURST", "1001"},
		{"THINPOST_EDGE_SMTP_GREETING_TIMEOUT", "0s"},
		{"THINPOST_EDGE_SMTP_CONNECT_TIMEOUT", "31m"},
		{"THINPOST_EDGE_SMTP_COMMAND_TIMEOUT", "500ms"},
		{"THINPOST_EDGE_SMTP_DATA_INIT_TIMEOUT", "31m"},
		{"THINPOST_EDGE_SMTP_DATA_BLOCK_TIMEOUT", "invalid"},
		{"THINPOST_EDGE_SMTP_DATA_FINAL_TIMEOUT", "-1m"},
	} {
		previous := env[tc.key]
		env[tc.key] = tc.value
		if _, err := Load(get); err == nil {
			t.Errorf("accepted %s=%s", tc.key, tc.value)
		}
		env[tc.key] = previous
	}
	path := filepath.Join(t.TempDir(), "routes")
	env["THINPOST_EDGE_ROUTE_EXCEPTIONS_FILE"] = path
	if _, err := Load(get); err == nil {
		t.Fatal("missing configured route exceptions accepted")
	}
	if err := os.WriteFile(path, []byte("test@mailhost.example mail.mailhost.example 45.0.1.21\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(get)
	if err != nil || len(config.RouteExceptions) != 1 {
		t.Fatal("valid route exception not loaded", err)
	}
	if err := os.WriteFile(path, []byte("test@mailhost.example mail.mailhost.example 127.0.0.1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(get); err == nil {
		t.Fatal("special-use route exception accepted")
	}
}
