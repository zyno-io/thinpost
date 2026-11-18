package controlapp

import "testing"

func TestConfigWebhookInternalDestinations(t *testing.T) {
	values := env()
	if config, err := load(values); err != nil || config.WebhookDestinations != nil {
		t.Fatal("the exception list must default to empty", err)
	}
	values["THINPOST_WEBHOOK_INTERNAL_DESTINATIONS"] = "http://hooks.mail.svc.cluster.local/events"
	if _, err := load(values); err == nil {
		t.Fatal("destinations without THINPOST_SERVICE_CIDRS must fail closed")
	}
	values["THINPOST_SERVICE_CIDRS"] = "10.96.0.0/12"
	config, err := load(values)
	if err != nil || config.WebhookDestinations == nil {
		t.Fatal("valid configuration rejected", err)
	}
	if _, _, ok := config.WebhookDestinations.NormalizeEndpoint("http://hooks.mail.svc.cluster.local/events"); !ok {
		t.Fatal("listed URL not accepted")
	}
	for _, bad := range []string{"10.0.0.0/7", "0.0.0.0/0", "8.8.8.0/24", "nonsense", "10.0.0.0/8", "fd00::/64", "100.100.100.0/24"} {
		values["THINPOST_SERVICE_CIDRS"] = bad
		if _, err := load(values); err == nil {
			t.Fatal("accepted Service CIDR", bad)
		}
	}
	values["THINPOST_SERVICE_CIDRS"] = "10.96.0.0/12"
	for _, bad := range []string{"http://10.96.0.5/events", "http://hooks.example.com/events", "http://hooks.mail.svc/events"} {
		values["THINPOST_WEBHOOK_INTERNAL_DESTINATIONS"] = bad
		if _, err := load(values); err == nil {
			t.Fatal("accepted destination", bad)
		}
	}
}
