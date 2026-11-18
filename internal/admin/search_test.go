package admin

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSearchRouteValidation(t *testing.T) {
	app := strings.Repeat("a", 32)
	path := "/api/v1/apps/" + app + "/messages/search"
	store := &fakeStore{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
	if got := send(h, "GET", path, "", "valid"); got.Code != 405 || got.Header().Get("Allow") != "POST" {
		t.Fatal("search accepted GET", got.Code)
	}
	if got := send(h, "POST", path+"?q=secret", `{"q":"x@y.z"}`, "valid"); got.Code != 400 {
		t.Fatal("query string accepted on search", got.Code)
	}
	tooLong := strings.Repeat("a", 255)
	for name, body := range map[string]string{
		"empty body":          ``,
		"unknown field":       `{"q":"abc","extra":1}`,
		"missing q":           `{"field":"id"}`,
		"empty q":             `{"q":""}`,
		"padded q":            `{"q":" abc "}`,
		"too long q":          `{"q":"` + tooLong + `"}`,
		"control character":   `{"q":"a\u0000b"}`,
		"newline":             `{"q":"a\nb"}`,
		"bad field":           `{"q":"abc","field":"body"}`,
		"limit zero":          `{"q":"abc","limit":0}`,
		"limit over 100":      `{"q":"abc","limit":101}`,
		"cursor too long":     `{"q":"abc","cursor":"` + strings.Repeat("c", 513) + `"}`,
		"q wrong type":        `{"q":5}`,
		"trailing JSON":       `{"q":"abc"}{}`,
		"invalid UTF-8 bytes": "{\"q\":\"\xff\"}",
	} {
		if got := send(h, "POST", path, body, "valid"); got.Code != 400 {
			t.Fatal("invalid search accepted:", name, got.Code)
		}
	}
	if store.calls != 0 {
		t.Fatal("invalid search reached the store")
	}
	for _, field := range []string{"", "any", "id", "recipient", "recipient_domain", "sender_domain", "subject"} {
		store.calls = 0
		body := `{"q":"example.com","cursor":"abc","limit":7}`
		if field != "" {
			body = `{"q":"example.com","field":"` + field + `","cursor":"abc","limit":7}`
		}
		got := send(h, "POST", path, body, "valid")
		want := field
		if want == "" {
			want = "any"
		}
		if got.Code != 200 || store.calls != 1 || store.query != (MessageQuery{Text: "example.com", Field: want, Cursor: "abc", Limit: 7}) {
			t.Fatal("search not forwarded", field, got.Code, store.query)
		}
		if strings.Contains(got.Body.String(), "example.com") || !strings.Contains(got.Body.String(), `"messages":[]`) {
			t.Fatal("empty page not normalized or term echoed")
		}
	}
	store.calls = 0
	send(h, "POST", path, `{"q":"abc"}`, "valid")
	if store.query.Limit != 50 || store.query.Field != "any" {
		t.Fatal("defaults not applied", store.query)
	}
	store.searchErr = errors.New("db")
	if got := send(h, "POST", path, `{"q":"abc"}`, "valid"); got.Code != 503 {
		t.Fatal("unavailable search not 503", got.Code)
	}
	store.searchErr = ErrInvalidInput
	if got := send(h, "POST", path, `{"q":"abc"}`, "valid"); got.Code != 400 {
		t.Fatal("unsearchable q not 400", got.Code)
	}
	if got := send(&Handler{Authenticator: fakeAuth{admin: false}, Store: store}, "POST", path, `{"q":"abc"}`, "valid"); got.Code != 403 {
		t.Fatal("non-admin searched", got.Code)
	}
}

func TestSubjectRevealRoute(t *testing.T) {
	app, message := strings.Repeat("a", 32), strings.Repeat("c", 32)
	path := "/api/v1/apps/" + app + "/messages/" + message + "/subject"
	store := &fakeStore{subject: "Secret subject"}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
	if got := send(h, "POST", path, ``, "valid"); got.Code != 405 {
		t.Fatal("reveal accepted POST", got.Code)
	}
	if got := send(h, "GET", path+"?x=1", ``, "valid"); got.Code != 400 {
		t.Fatal("reveal accepted a query string", got.Code)
	}
	got := send(h, "GET", path, ``, "valid")
	if got.Code != 200 || got.Header().Get("Cache-Control") != "no-store" || store.key != message || store.app != app {
		t.Fatal("reveal failed", got.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil || body["subject"] != "Secret subject" || len(body) != 1 {
		t.Fatal("unexpected reveal body", got.Body.String())
	}
	for _, tc := range []struct {
		err  error
		code int
	}{{ErrNotFound, 404}, {ErrConflict, 409}, {errors.New("db"), 503}} {
		store.subjectErr = tc.err
		if got := send(h, "GET", path, ``, "valid"); got.Code != tc.code {
			t.Fatal("wrong reveal status", tc.code, got.Code)
		}
	}
}

func TestMessageSummaryOmitsAbsentSubject(t *testing.T) {
	data, err := json.Marshal(MessageSummary{ID: "x", EnhancedProtection: true, SubjectMasked: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"subject"`, `"sender_domain"`, `"recipients"`, `"recipient_count"`} {
		if strings.Contains(string(data), field) {
			t.Fatal("absent field serialized:", field)
		}
	}
	if !strings.Contains(string(data), `"subject_masked":true`) || !strings.Contains(string(data), `"enhanced_protection":true`) {
		t.Fatal("missing flags", string(data))
	}
}

func TestAttachmentDownloadIsAlwaysOpaque(t *testing.T) {
	app, msg := strings.Repeat("a", 32), strings.Repeat("b", 32)
	reader := &fakeBodyReader{attachment: AttachmentContent{Filename: "résumé \"final\".html", ContentType: "text/html", Content: []byte("<script>alert(1)</script>")}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, ContentReader: reader}
	base := "/api/v1/apps/" + app + "/messages/" + msg + "/attachments/"
	got := send(h, "GET", base+"3", "", "valid")
	if got.Code != 200 || reader.index != 3 || got.Body.String() != "<script>alert(1)</script>" {
		t.Fatal("attachment not served", got.Code)
	}
	for name, want := range map[string]string{
		"Content-Type":                 "application/octet-stream",
		"X-Content-Type-Options":       "nosniff",
		"Cache-Control":                "no-store",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'; sandbox",
		"Content-Length":               "25",
	} {
		if got.Header().Get(name) != want {
			t.Fatal("header", name, got.Header().Get(name))
		}
	}
	disposition := got.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment;") || !strings.Contains(disposition, "filename*=utf-8''r%C3%A9sum%C3%A9") || strings.Contains(strings.ToLower(disposition), "inline") {
		t.Fatal("Content-Disposition not RFC 2231", disposition)
	}
	if got := send(h, "GET", base+"0", "", "valid"); got.Code != 200 {
		t.Fatal("index 0 rejected")
	}
	for _, index := range []string{"20", "00", "01", "-1", "+1", "1.0", "a", "", "100", "0x1"} {
		reader.calls = 0
		if got := send(h, "GET", base+index, "", "valid"); got.Code != 404 || reader.calls != 0 {
			t.Fatal("non-canonical index not 404 before the reader:", index, got.Code)
		}
	}
	if got := send(h, "GET", base+"1?x=1", "", "valid"); got.Code != 400 {
		t.Fatal("query accepted", got.Code)
	}
	if got := send(h, "POST", base+"1", "", "valid"); got.Code != 405 {
		t.Fatal("POST accepted", got.Code)
	}
	reader.attachment.Filename = "bad\nname"
	if got := send(h, "GET", base+"7", "", "valid"); strings.ContainsAny(got.Header().Get("Content-Disposition"), "\r\n") || !strings.HasPrefix(got.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatal("unsafe disposition", got.Header().Get("Content-Disposition"))
	}
	reader.err = ErrNotFound
	if got := send(h, "GET", base+"1", "", "valid"); got.Code != 404 || got.Header().Get("Content-Type") == "application/octet-stream" {
		t.Fatal("not found mishandled", got.Code)
	}
	h.ContentReader = nil
	if got := send(h, "GET", base+"1", "", "valid"); got.Code != 503 {
		t.Fatal("missing storage must fail closed", got.Code)
	}
}

func TestDeliveryHeadersRoute(t *testing.T) {
	app, msg, delivery := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	reader := &fakeBodyReader{headers: []HeaderField{{Name: "Subject", Value: "=?utf-8?q?caf=C3=A9?=", DecodedValue: "café"}, {Name: "From", Value: "a@example.com"}}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, ContentReader: reader}
	path := "/api/v1/apps/" + app + "/messages/" + msg + "/deliveries/" + delivery + "/headers"
	got := send(h, "GET", path, "", "valid")
	if got.Code != 200 || got.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("headers not served", got.Code)
	}
	var body struct {
		Headers []HeaderField `json:"headers"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil || len(body.Headers) != 2 || body.Headers[0].DecodedValue != "café" || body.Headers[1].DecodedValue != "" {
		t.Fatal("unexpected body", got.Body.String())
	}
	if got := send(h, "POST", path, "", "valid"); got.Code != 405 {
		t.Fatal("POST accepted")
	}
	if got := send(h, "GET", path+"?x=1", "", "valid"); got.Code != 400 {
		t.Fatal("query accepted")
	}
	reader.err = errors.New("db")
	if got := send(h, "GET", path, "", "valid"); got.Code != 503 {
		t.Fatal("storage failure not 503", got.Code)
	}
	reader.err, reader.headers = nil, nil
	if got := send(h, "GET", path, "", "valid"); !strings.Contains(got.Body.String(), `"headers":[]`) {
		t.Fatal("empty headers not an array", got.Body.String())
	}
	if got := send(&Handler{Authenticator: fakeAuth{admin: false}, Store: &fakeStore{}, ContentReader: reader}, "GET", path, "", "valid"); got.Code != 403 {
		t.Fatal("non-admin read headers")
	}
}
