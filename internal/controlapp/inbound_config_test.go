package controlapp

import "testing"

// The inbound mail host now comes from the Edge's report, and the Edge-identity
// settings control used to read are gone. Old values must not stop a pod from
// starting while a rollout replaces its neighbours.
func TestRemovedEdgeIdentityEnvIsIgnored(t *testing.T) {
	values := env()
	for _, name := range []string{"THINPOST_INBOUND_MX_HOST", "THINPOST_CONTROL_SMTP_SOURCE", "THINPOST_CONTROL_BOUNCE_DOMAIN"} {
		values[name] = "not even a valid value"
	}
	if _, err := load(values); err != nil {
		t.Fatal("removed Edge identity env should be ignored", err)
	}
}
