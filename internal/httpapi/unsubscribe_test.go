package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

const (
	testUnsubscribe = `<https://app.example.com/u/token>, <mailto:unsub@example.com>`
	testPost        = `List-Unsubscribe=One-Click`
)

// unsubscribeRequest builds a provider request carrying the given header pairs
// in that provider's custom-header field.
func unsubscribeRequest(provider string, pairs [][2]string) (path, body, contentType string, headers map[string]string) {
	switch provider {
	case "postmark":
		items := []map[string]string{}
		for _, p := range pairs {
			items = append(items, map[string]string{"Name": p[0], "Value": p[1]})
		}
		data, _ := json.Marshal(map[string]any{"From": "sender@example.com", "To": "to@example.net", "Subject": "hello", "TextBody": "body", "Headers": items})
		return "/email", string(data), "application/json", map[string]string{"X-Postmark-Server-Token": "valid-key"}
	case "resend", "sendgrid":
		object := map[string]string{}
		for _, p := range pairs {
			object[p[0]] = p[1]
		}
		var payload map[string]any
		path := "/emails"
		if provider == "resend" {
			payload = map[string]any{"from": "sender@example.com", "to": "to@example.net", "subject": "hello", "text": "body", "headers": object}
		} else {
			path = "/v3/mail/send"
			payload = map[string]any{"from": map[string]string{"email": "sender@example.com"}, "personalizations": []any{map[string]any{"to": []any{map[string]string{"email": "to@example.net"}}}}, "subject": "hello", "content": []any{map[string]string{"type": "text/plain", "value": "body"}}, "headers": object}
		}
		data, _ := json.Marshal(payload)
		return path, string(data), "application/json", map[string]string{"Authorization": "Bearer valid-key"}
	}
	form := url.Values{"from": {"sender@example.com"}, "to": {"to@example.net"}, "subject": {"hello"}, "text": {"body"}}
	for _, p := range pairs {
		form.Add("h:"+p[0], p[1])
	}
	return "/v3/example.com/messages", form.Encode(), "application/x-www-form-urlencoded", map[string]string{"Authorization": "Basic YXBpOnZhbGlkLWtleQ=="}
}

func sendUnsubscribe(provider string, pairs [][2]string) (*httptest.ResponseRecorder, *fakeService) {
	f := &fakeService{app: submission.App{ID: "app-a"}}
	path, body, contentType, headers := unsubscribeRequest(provider, pairs)
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	New(f).ServeHTTP(w, r)
	return w, f
}

var unsubscribeProviders = []string{"postmark", "resend", "sendgrid", "mailgun"}

func TestListUnsubscribeHeadersPassThroughEveryAdapter(t *testing.T) {
	for _, provider := range unsubscribeProviders {
		t.Run(provider, func(t *testing.T) {
			w, f := sendUnsubscribe(provider, [][2]string{{"list-unsubscribe", testUnsubscribe}, {"List-Unsubscribe-Post", testPost}})
			if w.Code < 200 || w.Code >= 300 || len(f.requests) != 1 {
				t.Fatalf("status %d, requests %d", w.Code, len(f.requests))
			}
			m := f.requests[0].Message
			if m.ListUnsubscribe != testUnsubscribe || !m.ListUnsubscribePost {
				t.Fatal("headers not passed to submission")
			}
			w, f = sendUnsubscribe(provider, [][2]string{{"List-Unsubscribe", "<mailto:unsub@example.com>"}})
			if w.Code >= 300 || len(f.requests) != 1 || f.requests[0].Message.ListUnsubscribePost {
				t.Fatal("mailto-only header without Post was refused")
			}
		})
	}
}

func TestListUnsubscribeRejectionsEveryAdapter(t *testing.T) {
	long := "<https://example.com/" + strings.Repeat("a", 2048) + ">"
	cases := map[string][][2]string{
		"other header":           {{"X-Custom", "1"}},
		"other with unsubscribe": {{"List-Unsubscribe", testUnsubscribe}, {"X-Custom", "1"}},
		"case duplicate":         {{"List-Unsubscribe", testUnsubscribe}, {"LIST-UNSUBSCRIBE", testUnsubscribe}},
		"post only":              {{"List-Unsubscribe-Post", testPost}},
		"bad post value":         {{"List-Unsubscribe", testUnsubscribe}, {"List-Unsubscribe-Post", "List-Unsubscribe=one-click"}},
		"post needs https":       {{"List-Unsubscribe", "<mailto:u@example.com>"}, {"List-Unsubscribe-Post", testPost}},
		"http scheme":            {{"List-Unsubscribe", "<http://example.com/u>"}},
		"userinfo":               {{"List-Unsubscribe", "<https://user:pw@example.com/u>"}},
		"empty userinfo":         {{"List-Unsubscribe", "<https://@example.com/u>"}},
		"three items":            {{"List-Unsubscribe", "<mailto:a@example.com>, <mailto:b@example.com>, <mailto:c@example.com>"}},
		"no brackets":            {{"List-Unsubscribe", "https://example.com/u"}},
		"https too long":         {{"List-Unsubscribe", long}},
		"injection":              {{"List-Unsubscribe", "<mailto:a@example.com>\r\nBcc: x@example.net"}},
		"nul":                    {{"List-Unsubscribe", "<mailto:a@example.com\x00>"}},
		"line over 998":          {{"List-Unsubscribe", "<mailto:" + strings.Repeat("a", 990) + "@example.com>"}},
		"empty value":            {{"List-Unsubscribe", ""}},
		"javascript scheme":      {{"List-Unsubscribe", "<javascript:alert(1)>"}},
		"non ascii":              {{"List-Unsubscribe", "<https://example.com/é>"}},
		"trailing garbage":       {{"List-Unsubscribe", "<mailto:a@example.com> junk"}},
		"https needs host":       {{"List-Unsubscribe", "<https:///path>"}},
		"unicode folded name":    {{"L\u0130ST-UNSUBSCRIBE", testUnsubscribe}},
		"post duplicate":         {{"List-Unsubscribe", testUnsubscribe}, {"List-Unsubscribe-Post", testPost}, {"list-unsubscribe-post", testPost}},
	}
	for name, pairs := range cases {
		for _, provider := range unsubscribeProviders {
			t.Run(provider+"/"+name, func(t *testing.T) {
				w, f := sendUnsubscribe(provider, pairs)
				if w.Code < 400 || len(f.requests) != 0 {
					t.Fatalf("expected rejection before acceptance, got %d with %d requests", w.Code, len(f.requests))
				}
				if strings.Contains(w.Body.String(), "example.com") {
					t.Fatal("rejection echoed header content")
				}
			})
		}
	}
}

func TestListUnsubscribePostmarkBatchRejectsOnlyBadItem(t *testing.T) {
	f := &fakeService{app: submission.App{ID: "app-a"}}
	batch := `[{"From":"sender@example.com","To":"to@example.net","Subject":"a","TextBody":"b","Headers":[{"Name":"X-Custom","Value":"1"}]},{"From":"sender@example.com","To":"to@example.net","Subject":"ok","TextBody":"b","Headers":[{"Name":"List-Unsubscribe","Value":"<mailto:u@example.com>"}]}]`
	w := request(t, New(f), "/email/batch", batch, map[string]string{"X-Postmark-Server-Token": "valid-key"})
	var results []struct{ ErrorCode int }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &results) != nil || len(results) != 2 || results[0].ErrorCode != 300 || results[1].ErrorCode != 0 || len(f.requests) != 1 || f.requests[0].Message.ListUnsubscribe != "<mailto:u@example.com>" {
		t.Fatal("batch did not reject only the bad header item")
	}
}

func TestListUnsubscribeMailgunCaseInsensitiveNamesAndDuplicates(t *testing.T) {
	form := url.Values{"from": {"sender@example.com"}, "to": {"to@example.net"}, "subject": {"hello"}, "text": {"body"}, "h:list-unsubscribe": {"<mailto:u@example.com>"}}
	f := &fakeService{app: submission.App{ID: "app-a"}}
	r := httptest.NewRequest(http.MethodPost, "/v3/example.com/messages", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth("api", "valid-key")
	w := httptest.NewRecorder()
	New(f).ServeHTTP(w, r)
	if w.Code >= 300 || len(f.requests) != 1 || f.requests[0].Message.ListUnsubscribe != "<mailto:u@example.com>" {
		t.Fatal("lower-case Mailgun name refused")
	}
	form.Add("h:List-Unsubscribe", "<mailto:v@example.com>")
	f = &fakeService{app: submission.App{ID: "app-a"}}
	r = httptest.NewRequest(http.MethodPost, "/v3/example.com/messages", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth("api", "valid-key")
	w = httptest.NewRecorder()
	New(f).ServeHTTP(w, r)
	if w.Code < 400 || len(f.requests) != 0 {
		t.Fatal("case-variant duplicate accepted")
	}
}

func TestListUnsubscribeMailgunRequiresLowerCasePrefix(t *testing.T) {
	form := url.Values{"from": {"sender@example.com"}, "to": {"to@example.net"}, "subject": {"hello"}, "text": {"body"}, "H:List-Unsubscribe": {"<mailto:u@example.com>"}}
	f := &fakeService{app: submission.App{ID: "app-a"}}
	r := httptest.NewRequest(http.MethodPost, "/v3/example.com/messages", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth("api", "valid-key")
	w := httptest.NewRecorder()
	New(f).ServeHTTP(w, r)
	if w.Code < 400 || len(f.requests) != 0 {
		t.Fatal("upper-case h: prefix accepted")
	}
}
