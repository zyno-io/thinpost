package admin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOperatorTestTagIsOmittedUnlessSet(t *testing.T) {
	plain, err := json.Marshal(MessageSummary{ID: "m"})
	if err != nil || strings.Contains(string(plain), "operator_test") {
		t.Fatalf("untagged summary: %s %v", plain, err)
	}
	tagged, err := json.Marshal(MessageSummary{ID: "m", OperatorTest: &OperatorTest{BypassLimits: true}})
	if err != nil || !strings.Contains(string(tagged), `"operator_test":{"bypass_limits":true,"bypass_recipient_policy":false}`) {
		t.Fatalf("tagged summary: %s %v", tagged, err)
	}
}

func TestRuntimeFeaturesReportRecipientPolicy(t *testing.T) {
	data, err := json.Marshal(RuntimeFeatures{Submission: true, RecipientPolicy: "allowlist"})
	if err != nil || !strings.Contains(string(data), `"recipient_policy":"allowlist"`) {
		t.Fatalf("%s %v", data, err)
	}
	data, err = json.Marshal(RuntimeFeatures{})
	if err != nil || strings.Contains(string(data), "recipient_policy") {
		t.Fatalf("%s %v", data, err)
	}
}
