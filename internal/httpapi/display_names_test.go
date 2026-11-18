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

// Display names must survive every route, single and batch: quoted names with
// commas and non-ASCII names included. The stored source keeps the name; only
// the bare address is ever used for policy.
type nameCase struct {
	name, path, body string
	form             bool
	header           map[string]string
	batch            bool
}

func nameCases() []nameCase {
	postmarkMsg := `{"From":"\"Ada Ünï\" <ada@example.com>","To":"\"Lovelace, Ada\" <ada@example.net>, Bob <bob@example.net>","ReplyTo":"\"Help, Desk\" <help@example.com>","Subject":"hi","TextBody":"body"}`
	resendMsg := `{"from":"\"Ada Ünï\" <ada@example.com>","to":["\"Lovelace, Ada\" <ada@example.net>","Bob <bob@example.net>"],"reply_to":"\"Help, Desk\" <help@example.com>","subject":"hi","text":"body"}`
	resendArr := strings.Replace(resendMsg, `"reply_to":"\"Help, Desk\" <help@example.com>"`, `"reply_to":["\"Help, Desk\" <help@example.com>"]`, 1)
	grid := `{"from":{"email":"ada@example.com","name":"Ada Ünï"},"reply_to_list":[{"email":"help@example.com","name":"Help, Desk"}],"personalizations":[{"to":[{"email":"ada@example.net","name":"Lovelace, Ada"},{"email":"bob@example.net","name":"Bob"}]}],"subject":"hi","content":[{"type":"text/plain","value":"body"}]}`
	gridOne := strings.Replace(grid, `"reply_to_list":[{"email":"help@example.com","name":"Help, Desk"}]`, `"reply_to":{"email":"help@example.com","name":"Help, Desk"}`, 1)
	form := url.Values{"from": {`"Ada Ünï" <ada@example.com>`}, "to": {`"Lovelace, Ada" <ada@example.net>, Bob <bob@example.net>`}, "h:Reply-To": {`"Help, Desk" <help@example.com>`}, "subject": {"hi"}, "text": {"body"}}
	formRepeated := url.Values{"from": {`"Ada Ünï" <ada@example.com>`}, "to": {`"Lovelace, Ada" <ada@example.net>`, `Bob <bob@example.net>`}, "h:Reply-To": {`"Help, Desk" <help@example.com>`}, "subject": {"hi"}, "text": {"body"}}
	pm := map[string]string{"X-Postmark-Server-Token": "valid-key"}
	bearer := map[string]string{"Authorization": "Bearer valid-key"}
	return []nameCase{
		{name: "postmark", path: "/email", body: postmarkMsg, header: pm},
		{name: "postmark batch", path: "/email/batch", body: "[" + postmarkMsg + "," + postmarkMsg + "]", header: pm},
		{name: "resend", path: "/emails", body: resendMsg, header: bearer},
		{name: "resend array reply_to", path: "/emails", body: resendArr, header: bearer},
		{name: "resend batch", path: "/emails/batch", body: "[" + resendMsg + "," + resendArr + "]", header: bearer, batch: true},
		{name: "sendgrid", path: "/v3/mail/send", body: grid, header: bearer},
		{name: "sendgrid single reply_to", path: "/v3/mail/send", body: gridOne, header: bearer},
		{name: "mailgun comma list", path: "/v3/example.com/messages", body: form.Encode(), form: true},
		{name: "mailgun repeated to", path: "/v3/example.com/messages", body: formRepeated.Encode(), form: true},
	}
}

func checkAddress(t *testing.T, got, wantName, wantAddress string) {
	t.Helper()
	parsed, err := mail.ParseAddress(got)
	if err != nil || parsed.Name != wantName || parsed.Address != wantAddress {
		t.Fatalf("address %q lost its name or address (want %q <%s>)", got, wantName, wantAddress)
	}
}

func TestDisplayNamesSurviveAcceptanceOnEveryRoute(t *testing.T) {
	for _, tc := range nameCases() {
		t.Run(tc.name, func(t *testing.T) {
			var messages []submission.Message
			var w *httptest.ResponseRecorder
			if tc.batch {
				f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"one", "two"}}}
				w = request(t, New(f), tc.path, tc.body, tc.header)
				messages = f.batch.Messages
			} else {
				f := &fakeService{app: submission.App{ID: "app"}}
				if tc.form {
					r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
					r.SetBasicAuth("api", "valid-key")
					r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					w = httptest.NewRecorder()
					New(f).ServeHTTP(w, r)
				} else {
					w = request(t, New(f), tc.path, tc.body, tc.header)
				}
				for _, req := range f.requests {
					messages = append(messages, req.Message)
				}
			}
			if w.Code < 200 || w.Code >= 300 || len(messages) == 0 {
				t.Fatalf("status %d", w.Code)
			}
			for _, m := range messages {
				checkAddress(t, m.From, "Ada Ünï", "ada@example.com")
				if len(m.To) != 2 {
					t.Fatalf("recipients %d", len(m.To))
				}
				checkAddress(t, m.To[0], "Lovelace, Ada", "ada@example.net")
				checkAddress(t, m.To[1], "Bob", "bob@example.net")
				if len(m.ReplyTo) != 1 {
					t.Fatal("reply address lost")
				}
				checkAddress(t, m.ReplyTo[0], "Help, Desk", "help@example.com")
			}
		})
	}
}

func TestSendGridPersonalizationNamesSurvive(t *testing.T) {
	body := `{"from":{"email":"ada@example.com","name":"Ada"},"personalizations":[{"to":[{"email":"one@example.net","name":"One, First"}],"from":{"email":"other@example.com","name":"Öther"}},{"to":[{"email":"two@example.net","name":"Zoë"}]}],"subject":"hi","content":[{"type":"text/plain","value":"body"}]}`
	f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"one", "two"}}}
	if w := request(t, New(f), "/v3/mail/send", body, map[string]string{"Authorization": "Bearer valid-key"}); w.Code != http.StatusAccepted {
		t.Fatalf("status %d", w.Code)
	}
	m := f.batch.Messages
	checkAddress(t, m[0].From, "Öther", "other@example.com")
	checkAddress(t, m[0].To[0], "One, First", "one@example.net")
	checkAddress(t, m[1].From, "Ada", "ada@example.com")
	checkAddress(t, m[1].To[0], "Zoë", "two@example.net")
}
