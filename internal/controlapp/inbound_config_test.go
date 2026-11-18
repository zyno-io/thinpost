package controlapp

import "testing"

func TestConfigInboundMXHost(t *testing.T) {
	values := env()
	if config, err := load(values); err != nil || config.InboundMXHost != "" {
		t.Fatal("the MX host is optional", config.InboundMXHost, err)
	}
	values["THINPOST_INBOUND_MX_HOST"] = "smtp.mx.example.net"
	if config, err := load(values); err != nil || config.InboundMXHost != "smtp.mx.example.net" {
		t.Fatal("valid MX host rejected", err)
	}
	for _, bad := range []string{"SMTP.example.net", "smtp.example.net.", "nodots", "-bad.example.net", "10.0.0.1", "smtp .example.net", "smtp.example.invalid"} {
		values["THINPOST_INBOUND_MX_HOST"] = bad
		if _, err := load(values); err == nil {
			t.Fatal("accepted MX host", bad)
		}
	}
}
