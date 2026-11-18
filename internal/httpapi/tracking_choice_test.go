package httpapi

import (
	"encoding/base64"
	"net/url"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

// kinds renders a *TrackingOptions as "-" (nil) or "o"/"c" letters.
func kinds(t *submission.TrackingOptions) string {
	if t == nil {
		return "-"
	}
	out := ""
	if t.Opens {
		out += "o"
	}
	if t.Clicks {
		out += "c"
	}
	return out
}

// Absent leaves both fields nil (app default), an explicit off lands in
// TrackingOff only, and an explicit off passes with the tracking runtime off.
func TestExplicitTrackingChoicesAreSeparatedFromAbsent(t *testing.T) {
	type want struct{ on, off string }
	postmark := `{"From":"sender@example.com","To":"to@example.net","Subject":"hello","HtmlBody":"<b>x</b>"`
	sendgrid := `{"from":{"email":"sender@example.com"},"personalizations":[{"to":[{"email":"to@example.net"}]}],"subject":"hello","content":[{"type":"text/html","value":"<b>x</b>"}]`
	for _, tc := range []struct {
		name, path, body string
		headers          map[string]string
		runtime          bool
		want             want
	}{
		{"postmark absent", "/email", postmark + "}", map[string]string{"X-Postmark-Server-Token": "valid-key"}, false, want{"-", "-"}},
		{"postmark off", "/email", postmark + `,"TrackOpens":false,"TrackLinks":"None"}`, map[string]string{"X-Postmark-Server-Token": "valid-key"}, false, want{"-", "oc"}},
		{"postmark mixed", "/email", postmark + `,"TrackOpens":false,"TrackLinks":"HtmlOnly"}`, map[string]string{"X-Postmark-Server-Token": "valid-key"}, true, want{"c", "o"}},
		{"postmark one kind", "/email", postmark + `,"TrackOpens":true}`, map[string]string{"X-Postmark-Server-Token": "valid-key"}, true, want{"o", "-"}},
		{"sendgrid absent", "/v3/mail/send", sendgrid + "}", map[string]string{"Authorization": "Bearer valid-key"}, false, want{"-", "-"}},
		{"sendgrid off", "/v3/mail/send", sendgrid + `,"tracking_settings":{"open_tracking":{"enable":false},"click_tracking":{"enable":false}}}`, map[string]string{"Authorization": "Bearer valid-key"}, false, want{"-", "oc"}},
		{"sendgrid mixed", "/v3/mail/send", sendgrid + `,"tracking_settings":{"open_tracking":{"enable":true},"click_tracking":{"enable":false}}}`, map[string]string{"Authorization": "Bearer valid-key"}, true, want{"o", "c"}},
		{"resend absent", "/emails", `{"from":"sender@example.com","to":"to@example.net","subject":"hello","html":"<b>x</b>"}`, map[string]string{"Authorization": "Bearer valid-key"}, false, want{"-", "-"}},
		{"resend off", "/emails", `{"from":"sender@example.com","to":"to@example.net","subject":"hello","html":"<b>x</b>"}`, map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Track-Opens": "false", "X-Thinpost-Track-Clicks": "false"}, false, want{"-", "oc"}},
		{"resend mixed", "/emails", `{"from":"sender@example.com","to":"to@example.net","subject":"hello","html":"<b>x</b>"}`, map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Track-Opens": "true", "X-Thinpost-Track-Clicks": "false"}, true, want{"o", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeService{app: submission.App{ID: "app"}}
			response := request(t, NewWithOptions(service, Options{Tracking: tc.runtime}), tc.path, tc.body, tc.headers)
			if response.Code != 200 && response.Code != 202 || len(service.requests) != 1 {
				t.Fatal("request not accepted", response.Code)
			}
			m := service.requests[0].Message
			if kinds(m.Tracking) != tc.want.on || kinds(m.TrackingOff) != tc.want.off {
				t.Fatalf("tracking %q off %q", kinds(m.Tracking), kinds(m.TrackingOff))
			}
			if m.Validate() != nil {
				t.Fatal("message does not validate")
			}
		})
	}
	// Mailgun forms.
	mailgun := map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("api:valid-key")), "Content-Type": "application/x-www-form-urlencoded"}
	for _, tc := range []struct {
		name, opens, clicks string
		runtime             bool
		want                want
	}{
		{"absent", "", "", false, want{"-", "-"}},
		{"off", "no", "no", false, want{"-", "oc"}},
		{"mixed", "yes", "no", true, want{"o", "c"}},
		{"clicks only", "", "htmlonly", true, want{"c", "-"}},
	} {
		t.Run("mailgun "+tc.name, func(t *testing.T) {
			values := url.Values{"from": {"sender@example.com"}, "to": {"to@example.net"}, "subject": {"hello"}, "html": {"<b>x</b>"}}
			if tc.opens != "" {
				values.Set("o:tracking-opens", tc.opens)
			}
			if tc.clicks != "" {
				values.Set("o:tracking-clicks", tc.clicks)
			}
			service := &fakeService{app: submission.App{ID: "app"}}
			if response := request(t, NewWithOptions(service, Options{Tracking: tc.runtime}), "/v3/example.com/messages", values.Encode(), mailgun); response.Code != 200 || len(service.requests) != 1 {
				t.Fatal("request not accepted", response.Code)
			}
			m := service.requests[0].Message
			if kinds(m.Tracking) != tc.want.on || kinds(m.TrackingOff) != tc.want.off {
				t.Fatalf("tracking %q off %q", kinds(m.Tracking), kinds(m.TrackingOff))
			}
		})
	}
}

func TestResendBatchTrackingChoicesDoNotAlias(t *testing.T) {
	body := `{"from":"sender@example.com","to":"to@example.net","subject":"hello","html":"<b>x</b>"}`
	batch := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"one", "two"}}}
	headers := map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Track-Opens": "true", "X-Thinpost-Track-Clicks": "false"}
	// Explicit opens with the runtime on.
	if response := request(t, NewWithOptions(batch, Options{Tracking: true}), "/emails/batch", "["+body+","+body+"]", headers); response.Code != 200 || len(batch.batch.Messages) != 2 {
		t.Fatal("batch rejected", response.Code)
	}
	a, b := batch.batch.Messages[0], batch.batch.Messages[1]
	if kinds(a.Tracking) != "o" || kinds(a.TrackingOff) != "c" || a.Tracking == b.Tracking || a.TrackingOff == b.TrackingOff {
		t.Fatal("items share tracking options or lost a choice")
	}
	// An explicit off only is accepted with the runtime off; an opt-in is not.
	off := map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Track-Opens": "false"}
	batch.batchCalls = 0
	if response := request(t, New(batch), "/emails/batch", "["+body+","+body+"]", off); response.Code != 200 || batch.batchCalls != 1 || kinds(batch.batch.Messages[0].TrackingOff) != "o" {
		t.Fatal("explicit off rejected with the runtime off", response.Code)
	}
	if response := request(t, New(batch), "/emails/batch", "["+body+"]", headers); response.Code != 400 || batch.batchCalls != 1 {
		t.Fatal("opt-in accepted with the runtime off")
	}
}
