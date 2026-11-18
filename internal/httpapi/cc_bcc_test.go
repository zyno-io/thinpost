package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

// ccRequest sends one message with Cc and Bcc through each provider's own
// fields.
func ccRequest(t *testing.T, provider, cc, bcc string) (*httptest.ResponseRecorder, *fakeService) {
	t.Helper()
	var path, body, contentType string
	headers := map[string]string{"Authorization": "Bearer valid-key"}
	switch provider {
	case "postmark":
		path, contentType = "/email", "application/json"
		headers = map[string]string{"X-Postmark-Server-Token": "valid-key"}
		body = `{"From":"sender@example.com","To":"to@example.net","Cc":"` + cc + `","Bcc":"` + bcc + `","Subject":"hello","TextBody":"body"}`
	case "resend":
		path, contentType = "/emails", "application/json"
		body = `{"from":"sender@example.com","to":"to@example.net","cc":["` + cc + `"],"bcc":"` + bcc + `","subject":"hello","text":"body"}`
	case "sendgrid":
		path, contentType = "/v3/mail/send", "application/json"
		body = `{"from":{"email":"sender@example.com"},"personalizations":[{"to":[{"email":"to@example.net"}],"cc":[{"email":"` + cc + `"}],"bcc":[{"email":"` + bcc + `"}]}],"subject":"hello","content":[{"type":"text/plain","value":"body"}]}`
	default:
		path, contentType = "/v3/example.com/messages", "application/x-www-form-urlencoded"
		headers = map[string]string{"Authorization": "Basic YXBpOnZhbGlkLWtleQ=="}
		body = url.Values{"from": {"sender@example.com"}, "to": {"to@example.net"}, "cc": {cc}, "bcc": {bcc}, "subject": {"hello"}, "text": {"body"}}.Encode()
	}
	f := &fakeService{app: submission.App{ID: "app-a"}}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	NewWithOptions(f, Options{}).ServeHTTP(w, r)
	return w, f
}

// bareList reduces addresses to their bare form, which adapters render with or
// without angle brackets.
func bareList(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		if a, err := mail.ParseAddress(value); err == nil {
			out[i] = a.Address
		}
	}
	return out
}

func TestCcAndBccThroughProviderFields(t *testing.T) {
	for _, provider := range []string{"postmark", "resend", "sendgrid", "mailgun"} {
		t.Run(provider, func(t *testing.T) {
			w, f := ccRequest(t, provider, "copy@example.net", "blind@example.net")
			if w.Code < 200 || w.Code >= 300 || len(f.requests) != 1 {
				t.Fatalf("status %d, requests %d", w.Code, len(f.requests))
			}
			m := f.requests[0].Message
			if !reflect.DeepEqual(bareList(m.Cc), []string{"copy@example.net"}) || !reflect.DeepEqual(bareList(m.Bcc), []string{"blind@example.net"}) {
				t.Fatalf("cc %v bcc %v", m.Cc, m.Bcc)
			}
			if !reflect.DeepEqual(bareList(m.Envelope()), []string{"to@example.net", "copy@example.net", "blind@example.net"}) || m.RecipientKind(0) != "to" || m.RecipientKind(1) != "cc" || m.RecipientKind(2) != "bcc" {
				t.Fatalf("envelope %v", m.Envelope())
			}
		})
	}
}

func TestMessagesWithoutCcOrBccStoreNilLists(t *testing.T) {
	f := &fakeService{app: submission.App{ID: "app"}}
	w := request(t, New(f), "/emails", `{"from":"sender@example.com","to":"to@example.net","cc":[],"bcc":[],"subject":"hello","text":"body"}`, map[string]string{"Authorization": "Bearer valid-key"})
	if w.Code != http.StatusOK || len(f.requests) != 1 || f.requests[0].Message.Cc != nil || f.requests[0].Message.Bcc != nil {
		t.Fatalf("status %d; empty cc/bcc must be nil", w.Code)
	}
}

func TestSendGridEmptyCcAndBccAreRejected(t *testing.T) {
	for _, field := range []string{"cc", "bcc"} {
		f := &fakeService{app: submission.App{ID: "app"}}
		body := `{"from":{"email":"sender@example.com"},"personalizations":[{"to":[{"email":"to@example.net"}],"` + field + `":[]}],"subject":"hello","content":[{"type":"text/plain","value":"body"}]}`
		if w := request(t, New(f), "/v3/mail/send", body, map[string]string{"Authorization": "Bearer valid-key"}); w.Code != http.StatusBadRequest || len(f.requests) != 0 {
			t.Fatalf("%s: status %d", field, w.Code)
		}
		body = strings.Replace(body, `[]}`, `null}`, 1)
		if w := request(t, New(f), "/v3/mail/send", body, map[string]string{"Authorization": "Bearer valid-key"}); w.Code != http.StatusAccepted {
			t.Fatalf("%s null: status %d", field, w.Code)
		}
	}
}

func TestSendGridRootLevelCcStaysRejected(t *testing.T) {
	f := &fakeService{app: submission.App{ID: "app"}}
	body := `{"from":{"email":"sender@example.com"},"cc":[{"email":"c@example.net"}],"personalizations":[{"to":[{"email":"to@example.net"}]}],"subject":"hello","content":[{"type":"text/plain","value":"body"}]}`
	if w := request(t, New(f), "/v3/mail/send", body, map[string]string{"Authorization": "Bearer valid-key"}); w.Code != http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
}

func TestMailgunRepeatedCcAndBcc(t *testing.T) {
	form := url.Values{"from": {"sender@example.com"}, "to": {"to@example.net"}, "cc": {"a@example.net, B <b@example.net>", "c@example.net"}, "bcc": {"d@example.net"}, "subject": {"hello"}, "text": {"body"}}
	f := &fakeService{app: submission.App{ID: "app"}}
	w := request(t, New(f), "/v3/example.com/messages", form.Encode(), map[string]string{"Authorization": "Basic YXBpOnZhbGlkLWtleQ==", "Content-Type": "application/x-www-form-urlencoded"})
	if w.Code != http.StatusOK || len(f.requests) != 1 || len(f.requests[0].Message.Cc) != 3 || len(f.requests[0].Message.Bcc) != 1 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestRecipientTotalCountsToCcAndBcc(t *testing.T) {
	list := func(n int) string {
		addresses := make([]string, n)
		for i := range addresses {
			addresses[i] = "u" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@example.net"
		}
		return strings.Join(addresses, ",")
	}
	f := &fakeService{app: submission.App{ID: "app"}}
	// 50 + 50 + 1 is over the 100 recipient limit; 50 + 50 is accepted.
	over := `{"From":"sender@example.com","To":"` + list(50) + `","Cc":"` + strings.ReplaceAll(list(50), "@example.net", "@example.org") + `","Bcc":"z@example.com","Subject":"hello","TextBody":"body"}`
	if w := request(t, New(f), "/email", over, map[string]string{"X-Postmark-Server-Token": "valid-key"}); w.Code == http.StatusOK || len(f.requests) != 0 {
		t.Fatalf("over the limit: status %d", w.Code)
	}
	at := `{"From":"sender@example.com","To":"` + list(50) + `","Cc":"` + strings.ReplaceAll(list(50), "@example.net", "@example.org") + `","Subject":"hello","TextBody":"body"}`
	if w := request(t, New(f), "/email", at, map[string]string{"X-Postmark-Server-Token": "valid-key"}); w.Code != http.StatusOK {
		t.Fatalf("at the limit: status %d", w.Code)
	}
}

func TestResendBatchItemCapCountsWholeEnvelope(t *testing.T) {
	list := func(n int, domain string) string {
		items := make([]string, n)
		for i := range items {
			items[i] = `"u` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@" + domain + `"`
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	for _, tc := range []struct {
		name string
		to   int
		cc   int
		ok   bool
	}{{"50 total", 25, 25, true}, {"51 total", 25, 26, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"one"}}}
			body := `[{"from":"sender@example.com","to":` + list(tc.to, "example.net") + `,"cc":` + list(tc.cc, "example.org") + `,"subject":"a","text":"body"}]`
			w := request(t, New(f), "/emails/batch", body, map[string]string{"Authorization": "Bearer valid-key"})
			if (w.Code == http.StatusOK) != tc.ok {
				t.Fatalf("status %d", w.Code)
			}
		})
	}
}

func TestSendGridTotalCountsCcAndBcc(t *testing.T) {
	people := func(n int, domain string) string {
		items := make([]string, n)
		for i := range items {
			items[i] = `{"email":"u` + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+i/676)) + "@" + domain + `"}`
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	// Ten personalizations with 100 recipients each is 1,000 (accepted); one
	// more Cc address pushes the total over.
	build := func(extra bool) string {
		var parts []string
		for i := 0; i < 10; i++ {
			part := `{"to":` + people(40, "example.net") + `,"cc":` + people(30, "example.org") + `,"bcc":` + people(30, "example.com")
			if extra && i == 9 {
				part = `{"to":` + people(40, "example.net") + `,"cc":` + people(31, "example.org") + `,"bcc":` + people(30, "example.com")
			}
			parts = append(parts, part+"}")
		}
		return `{"from":{"email":"sender@example.com"},"personalizations":[` + strings.Join(parts, ",") + `],"subject":"hello","content":[{"type":"text/plain","value":"body"}]}`
	}
	for _, extra := range []bool{false, true} {
		f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}}}
		w := request(t, New(f), "/v3/mail/send", build(extra), map[string]string{"Authorization": "Bearer valid-key"})
		if (w.Code < 300) == extra {
			t.Fatalf("extra=%v: status %d", extra, w.Code)
		}
	}
}

func TestReplyToHeaderOnEveryProvider(t *testing.T) {
	for _, provider := range []string{"postmark", "resend", "sendgrid", "mailgun"} {
		t.Run(provider, func(t *testing.T) {
			w, f := sendUnsubscribe(provider, [][2]string{{"Reply-To", "Help <help@example.com>, support@example.com"}})
			if w.Code < 200 || w.Code >= 300 || len(f.requests) != 1 || len(f.requests[0].Message.ReplyTo) != 2 {
				t.Fatalf("status %d", w.Code)
			}
			// Case variants are the same header.
			w, f = sendUnsubscribe(provider, [][2]string{{"reply-to", "help@example.com"}})
			if w.Code < 200 || w.Code >= 300 || len(f.requests) != 1 || len(f.requests[0].Message.ReplyTo) != 1 {
				t.Fatalf("lowercase: status %d", w.Code)
			}
			for _, pairs := range [][][2]string{
				{{"Reply-To", "not an address"}},
				{{"Reply-To", "a@example.com\r\nBcc: x@example.net"}},
			} {
				if w, _ := sendUnsubscribe(provider, pairs); w.Code < 400 {
					t.Fatalf("%q: status %d", pairs[0][1], w.Code)
				}
			}
		})
	}
	// Two spellings of the same header are a duplicate.
	for _, provider := range []string{"postmark", "mailgun"} {
		if w, _ := sendUnsubscribe(provider, [][2]string{{"Reply-To", "a@example.com"}, {"reply-to", "b@example.com"}}); w.Code < 400 {
			t.Fatalf("%s duplicate: status %d", provider, w.Code)
		}
	}
}

func TestNativeReplyFieldWithReplyToHeaderIsRejected(t *testing.T) {
	headers := map[string]string{"Authorization": "Bearer valid-key"}
	for _, tc := range []struct{ name, path, body string }{
		{"resend", "/emails", `{"from":"sender@example.com","to":"to@example.net","subject":"a","text":"body","reply_to":"a@example.com","headers":{"Reply-To":"b@example.com"}}`},
		{"sendgrid", "/v3/mail/send", `{"from":{"email":"sender@example.com"},"personalizations":[{"to":[{"email":"to@example.net"}]}],"subject":"a","content":[{"type":"text/plain","value":"body"}],"reply_to":{"email":"a@example.com"},"headers":{"Reply-To":"b@example.com"}}`},
		{"postmark", "/email", `{"From":"sender@example.com","To":"to@example.net","Subject":"a","TextBody":"body","ReplyTo":"a@example.com","Headers":[{"Name":"Reply-To","Value":"b@example.com"}]}`},
	} {
		h := headers
		if tc.name == "postmark" {
			h = map[string]string{"X-Postmark-Server-Token": "valid-key"}
		}
		f := &fakeService{app: submission.App{ID: "app"}}
		if w := request(t, New(f), tc.path, tc.body, h); w.Code != http.StatusBadRequest || len(f.requests) != 0 {
			t.Fatalf("%s: status %d", tc.name, w.Code)
		}
	}
}

func TestCcAndBccHeadersAreRejected(t *testing.T) {
	for _, provider := range []string{"postmark", "resend", "sendgrid", "mailgun"} {
		for _, name := range []string{"Cc", "Bcc", "bcc", "CC"} {
			if w, f := sendUnsubscribe(provider, [][2]string{{name, "x@example.net"}}); w.Code != http.StatusBadRequest || len(f.requests) != 0 {
				t.Fatalf("%s %s: status %d", provider, name, w.Code)
			}
		}
	}
	f := &fakeService{app: submission.App{ID: "app"}}
	body := `[{"From":"sender@example.com","To":"to@example.net","Subject":"a","TextBody":"body","Headers":[{"Name":"Bcc","Value":"x@example.net"}]}]`
	w := request(t, New(f), "/email/batch", body, map[string]string{"X-Postmark-Server-Token": "valid-key"})
	if !strings.Contains(w.Body.String(), `"ErrorCode":300`) || len(f.requests) != 0 {
		t.Fatalf("postmark batch item: %s", w.Body.String())
	}
}

func TestEmptyReplyToHeaderMeansNoReplyAddress(t *testing.T) {
	for _, provider := range []string{"postmark", "resend", "sendgrid", "mailgun"} {
		t.Run(provider, func(t *testing.T) {
			w, f := sendUnsubscribe(provider, [][2]string{{"Reply-To", ""}})
			if w.Code < 200 || w.Code >= 300 || len(f.requests) != 1 || len(f.requests[0].Message.ReplyTo) != 0 {
				t.Fatalf("status %d", w.Code)
			}
		})
	}
	// An empty value still counts as seen: a second Reply-To is a duplicate.
	if w, _ := sendUnsubscribe("postmark", [][2]string{{"Reply-To", ""}, {"reply-to", "a@example.com"}}); w.Code < 400 {
		t.Fatalf("duplicate after empty: status %d", w.Code)
	}
	// With the native field set, an empty header is harmless.
	f := &fakeService{app: submission.App{ID: "app"}}
	body := `{"from":"sender@example.com","to":"to@example.net","subject":"a","text":"body","reply_to":"a@example.com","headers":{"Reply-To":""}}`
	if w := request(t, New(f), "/emails", body, map[string]string{"Authorization": "Bearer valid-key"}); w.Code != http.StatusOK || len(f.requests[0].Message.ReplyTo) != 1 {
		t.Fatalf("native field with empty header: status %d", w.Code)
	}
}
