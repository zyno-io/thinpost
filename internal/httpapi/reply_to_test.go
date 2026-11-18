package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

func TestReplyAddressesAcceptedOnEveryBatchRoute(t *testing.T) {
	tests := []struct {
		name, path, body string
		header           map[string]string
	}{
		{"postmark", "/email/batch", `[{"From":"sender@example.com","To":"to@example.net","Subject":"a","TextBody":"body","ReplyTo":"help@example.com"},{"From":"sender@example.com","To":"to@example.net","Subject":"b","TextBody":"body","ReplyTo":"help@example.com"}]`, map[string]string{"X-Postmark-Server-Token": "valid-key"}},
		{"resend", "/emails/batch", `[{"from":"sender@example.com","to":"to@example.net","subject":"a","text":"body","reply_to":"help@example.com"},{"from":"sender@example.com","to":"to@example.net","subject":"b","text":"body","reply_to":["help@example.com"]}]`, map[string]string{"Authorization": "Bearer valid-key"}},
		{"sendgrid", "/v3/mail/send", `{"from":{"email":"sender@example.com"},"personalizations":[{"to":[{"email":"one@example.net"}]},{"to":[{"email":"two@example.net"}]}],"subject":"hello","content":[{"type":"text/plain","value":"body"}],"reply_to":{"email":"help@example.com"}}`, map[string]string{"Authorization": "Bearer valid-key"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"one", "two"}}}
			w := request(t, New(f), tc.path, tc.body, tc.header)
			messages := f.batch.Messages
			if tc.name == "postmark" {
				// Postmark batches are accepted item by item.
				messages = nil
				for _, req := range f.requests {
					messages = append(messages, req.Message)
				}
			}
			if w.Code < 200 || w.Code >= 300 || len(messages) != 2 {
				t.Fatalf("status %d, messages %d", w.Code, len(messages))
			}
			for _, message := range messages {
				if len(message.ReplyTo) != 1 {
					t.Fatal("reply address not passed to batch submission")
				}
			}
		})
	}
}

func TestProviderReplyAddresses(t *testing.T) {
	form := url.Values{"from": {"sender@example.com"}, "to": {"to@example.net"}, "subject": {"hello"}, "text": {"body"}, "h:Reply-To": {"Help <help@example.com>, support@example.com"}}
	tests := []struct{ name, path, body, contentType string }{
		{"postmark", "/email", `{"From":"sender@example.com","To":"to@example.net","Subject":"hello","TextBody":"body","ReplyTo":"Help <help@example.com>, support@example.com"}`, "application/json"},
		{"resend", "/emails", `{"from":"sender@example.com","to":"to@example.net","subject":"hello","text":"body","reply_to":["Help <help@example.com>","support@example.com"]}`, "application/json"},
		{"sendgrid", "/v3/mail/send", `{"from":{"email":"sender@example.com"},"personalizations":[{"to":[{"email":"to@example.net"}]}],"subject":"hello","content":[{"type":"text/plain","value":"body"}],"reply_to_list":[{"email":"help@example.com","name":"Help"},{"email":"support@example.com"}]}`, "application/json"},
		{"mailgun", "/v3/example.com/messages", form.Encode(), "application/x-www-form-urlencoded"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeService{app: submission.App{ID: "app-a"}}
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			switch tc.name {
			case "postmark":
				r.Header.Set("X-Postmark-Server-Token", "valid-key")
			case "mailgun":
				r.SetBasicAuth("api", "valid-key")
			default:
				r.Header.Set("Authorization", "Bearer valid-key")
			}
			w := httptest.NewRecorder()
			NewWithOptions(f, Options{}).ServeHTTP(w, r)
			if w.Code < 200 || w.Code >= 300 || len(f.requests) != 1 {
				t.Fatalf("status %d, requests %d", w.Code, len(f.requests))
			}
			replies := f.requests[0].Message.ReplyTo
			if len(replies) != 2 {
				t.Fatal("reply addresses not passed to submission")
			}
			first, err := mail.ParseAddress(replies[0])
			if err != nil || first.Address != "help@example.com" || first.Name != "Help" {
				t.Fatal("first reply address changed", err)
			}
			second, err := mail.ParseAddress(replies[1])
			if err != nil || second.Address != "support@example.com" {
				t.Fatal("second reply address changed", err)
			}
		})
	}
}

func TestReplyAddressValidationPrecedesAcceptance(t *testing.T) {
	for _, value := range []string{
		`"reply_to":"help@example.com\r\nBcc: victim@example.com"`,
		`"reply_to":["not-an-address"]`,
		`"reply_to":["a@example.com","a@example.com","a@example.com","a@example.com","a@example.com","a@example.com","a@example.com","a@example.com","a@example.com","a@example.com","a@example.com"]`,
	} {
		f := &fakeService{app: submission.App{ID: "app-a"}}
		body := `{"from":"sender@example.com","to":"to@example.net","subject":"hello","text":"body",` + value + `}`
		w := request(t, NewWithOptions(f, Options{}), "/emails", body, map[string]string{"Authorization": "Bearer valid-key"})
		if w.Code != http.StatusBadRequest || len(f.requests) != 0 {
			t.Fatalf("invalid reply address accepted: status %d", w.Code)
		}
	}
	f := &fakeService{app: submission.App{ID: "app-a"}}
	body := `{"from":{"email":"sender@example.com"},"personalizations":[{"to":[{"email":"to@example.net"}]}],"subject":"hello","content":[{"type":"text/plain","value":"body"}],"reply_to":{"email":"a@example.com"},"reply_to_list":[{"email":"b@example.com"}]}`
	w := request(t, NewWithOptions(f, Options{}), "/v3/mail/send", body, map[string]string{"Authorization": "Bearer valid-key"})
	if w.Code != http.StatusBadRequest || len(f.requests) != 0 {
		t.Fatal("mutually exclusive SendGrid reply fields were accepted")
	}
}

func TestSingleReplyAddressProviderForms(t *testing.T) {
	tests := []struct{ path, body string }{
		{"/emails", `{"from":"sender@example.com","to":"to@example.net","subject":"hello","text":"body","reply_to":"help@example.com"}`},
		{"/v3/mail/send", `{"from":{"email":"sender@example.com"},"personalizations":[{"to":[{"email":"to@example.net"}]}],"subject":"hello","content":[{"type":"text/plain","value":"body"}],"reply_to":{"email":"help@example.com"}}`},
	}
	for _, tc := range tests {
		f := &fakeService{app: submission.App{ID: "app-a"}}
		w := request(t, NewWithOptions(f, Options{}), tc.path, tc.body, map[string]string{"Authorization": "Bearer valid-key"})
		if w.Code < 200 || w.Code >= 300 || len(f.requests) != 1 || len(f.requests[0].Message.ReplyTo) != 1 {
			t.Fatalf("single reply address rejected on %s", tc.path)
		}
		address, err := mail.ParseAddress(f.requests[0].Message.ReplyTo[0])
		if err != nil || address.Address != "help@example.com" {
			t.Fatal("single reply address changed", err)
		}
	}
}
