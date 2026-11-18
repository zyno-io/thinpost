package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

func TestProviderJSONRejectsAmbiguousFieldsBeforeAcceptance(t *testing.T) {
	tests := []struct {
		name, path, body, header, credential string
	}{
		{"Postmark recipient casing", "/email", `{"From":"a@example.com","To":"one@example.net","TO":"two@example.net","Subject":"x","TextBody":"body"}`, "X-Postmark-Server-Token", "valid-key"},
		{"Resend escaped recipient", "/emails", `{"from":"a@example.com","to":"one@example.net","\u0074o":"two@example.net","subject":"x","text":"body"}`, "Authorization", "Bearer valid-key"},
		{"Resend Unicode case fold", "/emails", `{"from":"a@example.com","to":"one@example.net","subject":"x","\u017fubject":"y","text":"body"}`, "Authorization", "Bearer valid-key"},
		{"SendGrid nested sender", "/v3/mail/send", `{"from":{"email":"a@example.com","EMAIL":"b@example.com"},"personalizations":[{"to":[{"email":"one@example.net"}]}],"subject":"x","content":[{"type":"text/plain","value":"body"}]}`, "Authorization", "Bearer valid-key"},
		{"SendGrid nested recipient", "/v3/mail/send", `{"from":{"email":"a@example.com"},"personalizations":[{"to":[{"email":"one@example.net","Email":"two@example.net"}]}],"subject":"x","content":[{"type":"text/plain","value":"body"}]}`, "Authorization", "Bearer valid-key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeService{app: submission.App{ID: "app"}}
			response := request(t, New(service), tc.path, tc.body, map[string]string{tc.header: tc.credential})
			if response.Code != http.StatusBadRequest || len(service.requests) != 0 {
				t.Fatalf("ambiguous message accepted: status %d, submissions %d", response.Code, len(service.requests))
			}
		})
	}
}

func TestAmbiguousJSONBatchSemantics(t *testing.T) {
	postmark := &fakeService{app: submission.App{ID: "app"}}
	body := `[{"From":"a@example.com","To":"one@example.net","to":"two@example.net","Subject":"x","TextBody":"body"},{"From":"a@example.com","To":"three@example.net","Subject":"x","TextBody":"body"}]`
	response := request(t, New(postmark), "/email/batch", body, map[string]string{"X-Postmark-Server-Token": "valid-key"})
	var results []struct{ ErrorCode int }
	if err := json.Unmarshal(response.Body.Bytes(), &results); err != nil || response.Code != http.StatusOK || len(results) != 2 || results[0].ErrorCode == 0 || results[1].ErrorCode != 0 || len(postmark.requests) != 1 {
		t.Fatal("Postmark did not reject only the ambiguous item", response.Code, err)
	}

	resend := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}}
	body = `[{"from":"a@example.com","to":"one@example.net","subject":"x","text":"body"},{"from":"a@example.com","to":"two@example.net","TO":"three@example.net","subject":"x","text":"body"}]`
	response = request(t, New(resend), "/emails/batch", body, map[string]string{"Authorization": "Bearer valid-key"})
	if response.Code != http.StatusBadRequest || resend.batchCalls != 0 || len(resend.requests) != 0 {
		t.Fatal("Resend processed a batch containing an ambiguous item", response.Code)
	}
}

func TestJSONFieldValidationBoundsAndObjectScopes(t *testing.T) {
	if err := rejectAmbiguousJSONFields([]byte(`{"items":[{"email":"one"},{"EMAIL":"two"}]}`)); err != nil {
		t.Fatal("distinct objects must have independent field names", err)
	}
	if err := rejectAmbiguousJSONFields([]byte(strings.Repeat(`[`, 102) + `0` + strings.Repeat(`]`, 102))); err == nil {
		t.Fatal("excessively nested JSON was accepted")
	}
	for _, invalid := range []string{`{"a":1} {}`, `{"a":`, `{"a":[1,]}`} {
		if err := rejectAmbiguousJSONFields([]byte(invalid)); err == nil {
			t.Fatal("malformed or trailing JSON was accepted")
		}
	}
}
