package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

func TestPostmarkTrackingOptionsAreExplicitAndGated(t *testing.T) {
	base := `{"From":"sender@example.com","To":"to@example.net","Subject":"hello","HtmlBody":"<a href=\"https://example.net\">link</a>"`
	headers := map[string]string{"X-Postmark-Server-Token": "valid-key"}
	for _, tc := range []struct {
		name, fields  string
		enabled       bool
		status        int
		opens, clicks bool
	}{
		{"omitted", "}", false, 200, false, false},
		{"explicit off", `,"TrackOpens":false,"TrackLinks":"None"}`, false, 200, false, false},
		{"gate closed", `,"TrackOpens":true}`, false, 400, false, false},
		{"opens", `,"TrackOpens":true}`, true, 200, true, false},
		{"HTML clicks", `,"TrackLinks":"HtmlOnly"}`, true, 200, false, true},
		{"both", `,"TrackOpens":true,"TrackLinks":"HtmlOnly"}`, true, 200, true, true},
		{"text clicks", `,"TrackLinks":"TextOnly"}`, true, 400, false, false},
		{"both body types", `,"TrackLinks":"HtmlAndText"}`, true, 400, false, false},
		{"null opens", `,"TrackOpens":null}`, true, 400, false, false},
		{"null links", `,"TrackLinks":null}`, true, 400, false, false},
		{"bad opens type", `,"TrackOpens":"true"}`, true, 400, false, false},
		{"invalid mode", `,"TrackLinks":"htmlonly"}`, true, 400, false, false},
		{"duplicate opens", `,"TrackOpens":true,"trackopens":false}`, true, 400, false, false},
		{"duplicate links", `,"TrackLinks":"HtmlOnly","Track\u004cinks":"None"}`, true, 400, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeService{app: submission.App{ID: "app"}}
			server := NewWithOptions(service, Options{Tracking: tc.enabled})
			response := request(t, server, "/email", base+tc.fields, headers)
			if response.Code != tc.status {
				t.Fatalf("status %d", response.Code)
			}
			if tc.status != http.StatusOK {
				if len(service.requests) != 0 {
					t.Fatal("invalid tracking request was queued")
				}
				return
			}
			if len(service.requests) != 1 {
				t.Fatal("accepted request not submitted")
			}
			got := service.requests[0].Message.Tracking
			if !tc.opens && !tc.clicks {
				if got != nil {
					t.Fatal("tracking opt-out changed the message")
				}
			} else if got == nil || got.Opens != tc.opens || got.Clicks != tc.clicks {
				t.Fatal("tracking options were not preserved")
			}
		})
	}
	service := &fakeService{app: submission.App{ID: "app"}}
	response := request(t, NewWithOptions(service, Options{Tracking: true}), "/email", `{"From":"sender@example.com","To":"to@example.net","Subject":"hello","TextBody":"text","TrackOpens":true}`, headers)
	if response.Code != 400 || len(service.requests) != 0 {
		t.Fatal("tracking was accepted without HTML")
	}
}

func TestPostmarkBatchTrackingUsesPerItemGate(t *testing.T) {
	const batch = `[{"From":"a@example.com","To":"one@example.net","Subject":"one","HtmlBody":"<b>x</b>","TrackOpens":true},{"From":"a@example.com","To":"two@example.net","Subject":"two","TextBody":"x","TrackLinks":"None"}]`
	headers := map[string]string{"X-Postmark-Server-Token": "valid-key"}
	for _, enabled := range []bool{false, true} {
		service := &fakeService{app: submission.App{ID: "app"}}
		response := request(t, NewWithOptions(service, Options{Tracking: enabled}), "/email/batch", batch, headers)
		var items []struct{ ErrorCode int }
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &items) != nil || len(items) != 2 {
			t.Fatal("invalid batch response")
		}
		if enabled {
			if items[0].ErrorCode != 0 || items[1].ErrorCode != 0 || len(service.requests) != 2 || service.requests[0].Message.Tracking == nil || !service.requests[0].Message.Tracking.Opens {
				t.Fatal("tracked item was not submitted")
			}
		} else if items[0].ErrorCode == 0 || items[1].ErrorCode != 0 || len(service.requests) != 1 || service.requests[0].Message.Tracking != nil {
			t.Fatal("closed gate did not reject only the tracked item")
		}
	}
}
