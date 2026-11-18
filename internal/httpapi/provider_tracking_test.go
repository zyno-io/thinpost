package httpapi

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

func TestSendGridTrackingSubsetAndGate(t *testing.T) {
	base := `{"from":{"email":"sender@example.com"},"personalizations":[{"to":[{"email":"to@example.net"}]}],"subject":"hello","content":[{"type":"text/html","value":"<b>hello</b>"}]`
	headers := map[string]string{"Authorization": "Bearer valid-key"}
	for _, tc := range []struct {
		name, settings string
		enabled        bool
		status         int
		opens, clicks  bool
	}{
		{"omitted", "}", false, 202, false, false},
		{"explicit off", `,"tracking_settings":{"open_tracking":{"enable":false},"click_tracking":{"enable":false}}}`, false, 202, false, false},
		{"gate closed", `,"tracking_settings":{"open_tracking":{"enable":true}}}`, false, 400, false, false},
		{"both", `,"tracking_settings":{"open_tracking":{"enable":true},"click_tracking":{"enable":true,"enable_text":false}}}`, true, 202, true, true},
		{"opens", `,"tracking_settings":{"open_tracking":{"enable":true}}}`, true, 202, true, false},
		{"HTML clicks", `,"tracking_settings":{"click_tracking":{"enable":true,"enable_text":false}}}`, true, 202, false, true},
		{"ambiguous text default", `,"tracking_settings":{"click_tracking":{"enable":true}}}`, true, 400, false, false},
		{"text clicks", `,"tracking_settings":{"click_tracking":{"enable":true,"enable_text":true}}}`, true, 400, false, false},
		{"null option", `,"tracking_settings":{"click_tracking":{"enable":false,"enable_text":null}}}`, true, 400, false, false},
		{"null settings", `,"tracking_settings":null}`, true, 400, false, false},
		{"null open", `,"tracking_settings":{"open_tracking":null}}`, true, 400, false, false},
		{"unknown open option", `,"tracking_settings":{"open_tracking":{"enable":true,"substitution_tag":"%open%"}}}`, true, 400, false, false},
		{"unsupported subscription", `,"tracking_settings":{"subscription_tracking":{"enable":false}}}`, true, 400, false, false},
		{"bad text type", `,"tracking_settings":{"click_tracking":{"enable":true,"enable_text":"false"}}}`, true, 400, false, false},
		{"duplicate top-level", `,"tracking_settings":{},"tracking_\u0073ettings":{"open_tracking":{"enable":true}}}`, true, 400, false, false},
		{"duplicate nested", `,"tracking_settings":{"open_tracking":{"enable":true,"ENABLE":false}}}`, true, 400, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeService{app: submission.App{ID: "app"}}
			response := request(t, NewWithOptions(service, Options{Tracking: tc.enabled}), "/v3/mail/send", base+tc.settings, headers)
			if response.Code != tc.status {
				t.Fatalf("status %d", response.Code)
			}
			if tc.status != http.StatusAccepted {
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
}

func TestMailgunTrackingSubsetAndGate(t *testing.T) {
	headers := map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("api:valid-key")), "Content-Type": "application/x-www-form-urlencoded"}
	for _, tc := range []struct {
		name, opens, clicks   string
		enabled               bool
		status                int
		wantOpens, wantClicks bool
	}{
		{"off", "no", "no", false, 200, false, false},
		{"gate closed", "yes", "", false, 400, false, false},
		{"both", "yes", "htmlonly", true, 200, true, true},
		{"opens", "yes", "no", true, 200, true, false},
		{"clicks", "no", "htmlonly", true, 200, false, true},
		{"text clicks", "no", "yes", true, 400, false, false},
		{"domain default", "pass", "", true, 400, false, false},
		{"invalid", "true", "", true, 400, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeService{app: submission.App{ID: "app"}}
			values := url.Values{"from": {"sender@example.com"}, "to": {"to@example.net"}, "subject": {"hello"}, "html": {"<b>hello</b>"}}
			if tc.opens != "" {
				values.Set("o:tracking-opens", tc.opens)
			}
			if tc.clicks != "" {
				values.Set("o:tracking-clicks", tc.clicks)
			}
			response := request(t, NewWithOptions(service, Options{Tracking: tc.enabled}), "/v3/example.com/messages", values.Encode(), headers)
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
			if !tc.wantOpens && !tc.wantClicks {
				if got != nil {
					t.Fatal("tracking opt-out changed the message")
				}
			} else if got == nil || got.Opens != tc.wantOpens || got.Clicks != tc.wantClicks {
				t.Fatal("tracking options were not preserved")
			}
		})
	}
	service := &fakeService{app: submission.App{ID: "app"}}
	values := url.Values{"from": {"sender@example.com"}, "to": {"to@example.net"}, "subject": {"hello"}, "html": {"<b>hello</b>"}, "o:tracking-opens": {"yes", "no"}}
	response := request(t, NewWithOptions(service, Options{Tracking: true}), "/v3/example.com/messages", values.Encode(), headers)
	if response.Code != 400 || len(service.requests) != 0 {
		t.Fatal("duplicate tracking options accepted")
	}
}

func TestResendTrackingHeadersAndAtomicBatch(t *testing.T) {
	body := `{"from":"sender@example.com","to":"to@example.net","subject":"hello","html":"<b>hello</b>"}`
	headers := map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Track-Opens": "true", "X-Thinpost-Track-Clicks": "true"}
	service := &fakeService{app: submission.App{ID: "app"}}
	if response := request(t, New(service), "/emails", body, headers); response.Code != 400 || len(service.requests) != 0 {
		t.Fatal("closed tracking gate accepted request")
	}
	response := request(t, NewWithOptions(service, Options{Tracking: true}), "/emails", body, headers)
	if response.Code != 200 || len(service.requests) != 1 || service.requests[0].Message.Tracking == nil || !service.requests[0].Message.Tracking.Opens || !service.requests[0].Message.Tracking.Clicks {
		t.Fatal("Resend tracking was not submitted")
	}
	duplicate := httptest.NewRequest("POST", "/emails", strings.NewReader(body))
	duplicate.Header.Set("Authorization", "Bearer valid-key")
	duplicate.Header.Add("X-Thinpost-Track-Opens", "true")
	duplicate.Header.Add("X-Thinpost-Track-Opens", "false")
	w := httptest.NewRecorder()
	NewWithOptions(service, Options{Tracking: true}).ServeHTTP(w, duplicate)
	if w.Code != 400 || len(service.requests) != 1 {
		t.Fatal("duplicate tracking headers accepted")
	}
	batch := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"one", "two"}}}
	response = request(t, New(batch), "/emails/batch", "["+body+","+body+"]", headers)
	if response.Code != 400 || batch.batchCalls != 0 {
		t.Fatal("closed tracking gate accepted a batch")
	}
	response = request(t, NewWithOptions(batch, Options{Tracking: true}), "/emails/batch", "["+body+","+body+"]", headers)
	if response.Code != 200 || batch.batchCalls != 1 || len(batch.batch.Messages) != 2 || batch.batch.Messages[0].Tracking == nil || !batch.batch.Messages[0].Tracking.Opens || !batch.batch.Messages[1].Tracking.Clicks {
		t.Fatal("batch tracking was not applied atomically")
	}
	bad := `{"from":"sender@example.com","to":"to@example.net","subject":"hello","text":"text"}`
	response = request(t, NewWithOptions(batch, Options{Tracking: true}), "/emails/batch", "["+body+","+bad+"]", headers)
	if response.Code != 400 || batch.batchCalls != 1 {
		t.Fatal("batch with untrackable item was submitted")
	}
	foreignHeaders := map[string]string{"X-Postmark-Server-Token": "valid-key", "X-Thinpost-Track-Opens": "true"}
	response = request(t, NewWithOptions(service, Options{Tracking: true}), "/email", `{"From":"sender@example.com","To":"to@example.net","Subject":"hello","HtmlBody":"<b>hello</b>"}`, foreignHeaders)
	if response.Code != 400 || len(service.requests) != 1 {
		t.Fatal("unsupported provider tracking header was ignored")
	}
}
