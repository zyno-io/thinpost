package httpapi

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

func TestProviderAttachmentsPreserveContent(t *testing.T) {
	tests := []struct {
		name, path, body, auth, value string
		status                        int
	}{
		{"postmark", "/email", `{"From":"a@example.com","To":"b@example.net","Subject":"x","HtmlBody":"<img src=\"cid:logo\">","Attachments":[{"Name":"a.txt","Content":"aGVsbG8=","ContentType":"text/plain"},{"Name":"logo.png","Content":"eA==","ContentType":"image/png","ContentID":"cid:logo"}]}`, "X-Postmark-Server-Token", "valid-key", 200},
		{"resend", "/emails", `{"from":"a@example.com","to":"b@example.net","subject":"x","html":"<img src=\"cid:logo\">","attachments":[{"filename":"a.txt","content":"aGVsbG8="},{"filename":"logo.png","content":"eA==","content_type":"image/png","content_id":"logo"}]}`, "Authorization", "Bearer valid-key", 200},
		{"sendgrid", "/v3/mail/send", `{"from":{"email":"a@example.com"},"personalizations":[{"to":[{"email":"b@example.net"}]}],"subject":"x","content":[{"type":"text/html","value":"<img src=\"cid:logo\">"}],"attachments":[{"filename":"a.txt","content":"aGVsbG8=","type":"text/plain"},{"filename":"logo.png","content":"eA==","type":"image/png","disposition":"inline","content_id":"logo"}]}`, "Authorization", "Bearer valid-key", 202},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeService{app: submission.App{ID: "app"}}
			headers := map[string]string{tc.auth: tc.value}
			w := request(t, NewWithOptions(f, Options{}), tc.path, tc.body, headers)
			if w.Code != tc.status || len(f.requests) != 1 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			a := f.requests[0].Message.Attachments
			if len(a) != 2 || string(a[0].Content) != "hello" || a[0].Inline || string(a[1].Content) != "x" || !a[1].Inline || a[1].ContentID != "logo" {
				t.Fatal("attachment data or metadata was lost")
			}
		})
	}
}

func TestAttachmentDecoderRejectsAmbiguousAndUnsafeInputs(t *testing.T) {
	tests := []struct{ provider, data string }{
		{"resend", `null`}, {"resend", `{}`}, {"resend", `[null]`},
		{"resend", `[{"filename":"x","content":null}]`},
		{"resend", `[{"filename":"x","content":"eA==\n"}]`},
		{"resend", `[{"filename":"x","content":"eA==","path":"http://127.0.0.1/"}]`},
		{"resend", `[{"filename":"x","content":"eA==","Content":"eA=="}]`},
		{"postmark", `[{"Name":"x","Content":"eA==","ContentID":"cid:"}]`},
		{"postmark", `[{"Name":"x","Content":"eA==","ContentLength":2}]`},
		{"sendgrid", `[{"filename":"x","content":""}]`},
		{"sendgrid", `[{"filename":"x","content":"eA==","disposition":"inline"}]`},
		{"sendgrid", `[{"filename":"x","content":"eA==","disposition":"attachment","content_id":"x"}]`},
	}
	for _, tc := range tests {
		if _, err := decodeAttachments([]byte(tc.data), tc.provider, submission.DefaultMaxAttachmentBytes); err == nil {
			t.Fatalf("%s accepted %s", tc.provider, tc.data)
		}
	}
	oversized, _ := json.Marshal([]map[string]string{{"filename": "x", "content": strings.Repeat("A", 4*((submission.DefaultMaxAttachmentBytes+2)/3)+4)}})
	if _, err := decodeAttachments(oversized, "resend", submission.DefaultMaxAttachmentBytes); err == nil {
		t.Fatal("oversized encoded content accepted")
	}
}

func TestMailgunAttachmentsAndRawFilenameValidation(t *testing.T) {
	for _, tc := range []struct {
		name, filename string
		want           int
	}{
		{"accepted", "file.txt", 200}, {"path rejected", "../file.txt", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			for k, v := range map[string]string{"from": "a@example.com", "to": "b@example.net", "subject": "x", "html": "<img src=\"cid:logo.png\">"} {
				if err := writer.WriteField(k, v); err != nil {
					t.Fatal(err)
				}
			}
			header := textproto.MIMEHeader{"Content-Disposition": {`form-data; name="attachment"; filename="` + tc.filename + `"`}, "Content-Type": {"text/plain"}}
			part, err := writer.CreatePart(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = part.Write([]byte("hello")); err != nil {
				t.Fatal(err)
			}
			part, err = writer.CreateFormFile("inline", "logo.png")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = part.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if err = writer.Close(); err != nil {
				t.Fatal(err)
			}
			f := &fakeService{app: submission.App{ID: "app"}}
			r := httptest.NewRequest(http.MethodPost, "/v3/example.com/messages", &body)
			r.SetBasicAuth("api", "valid-key")
			r.Header.Set("Content-Type", writer.FormDataContentType())
			w := httptest.NewRecorder()
			NewWithOptions(f, Options{}).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.want == 200 && (len(f.requests) != 1 || len(f.requests[0].Message.Attachments) != 2 || f.requests[0].Message.Attachments[1].ContentID != "logo.png") {
				t.Fatal("attachments lost")
			}
			if tc.want != 200 && len(f.requests) != 0 {
				t.Fatal("invalid request accepted")
			}
		})
	}
}

func TestAttachmentsAcceptedInBatches(t *testing.T) {
	postmark := `[{"From":"a@example.com","To":"b@example.net","Subject":"x","TextBody":"body","Attachments":[]}]`
	f := &fakeService{app: submission.App{ID: "app"}}
	w := request(t, NewWithOptions(f, Options{}), "/email/batch", postmark, map[string]string{"X-Postmark-Server-Token": "valid-key"})
	if w.Code != 200 || len(f.requests) != 1 {
		t.Fatal("postmark batch did not reach service")
	}
	resend := `[{"from":"a@example.com","to":"b@example.net","subject":"x","text":"body","attachments":[{"filename":"x","content":"eA=="}]}]`
	b := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"id"}}}
	w = request(t, NewWithOptions(b, Options{}), "/emails/batch", resend, map[string]string{"Authorization": "Bearer valid-key"})
	if w.Code != 200 || b.batchCalls != 1 || string(b.batch.Messages[0].Attachments[0].Content) != "x" {
		t.Fatal("resend batch attachments lost")
	}
}
