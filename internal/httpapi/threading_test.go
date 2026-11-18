package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

const (
	testReplyTo    = "<parent.1@mail.example.org>"
	testReferences = "<root@mail.example.org> <parent.1@mail.example.org>"
)

func TestThreadingHeadersPassThroughEveryAdapter(t *testing.T) {
	for _, provider := range unsubscribeProviders {
		t.Run(provider, func(t *testing.T) {
			w, f := sendUnsubscribe(provider, [][2]string{{"in-reply-to", "  " + testReplyTo + "\t"}, {"REFERENCES", " <root@mail.example.org> \t <parent.1@mail.example.org>  "}})
			if w.Code < 200 || w.Code >= 300 || len(f.requests) != 1 {
				t.Fatalf("status %d, requests %d", w.Code, len(f.requests))
			}
			m := f.requests[0].Message
			if m.InReplyTo != testReplyTo || m.References != testReferences || m.ListUnsubscribe != "" {
				t.Fatal("threading headers not normalized and passed to submission")
			}
			w, f = sendUnsubscribe(provider, [][2]string{{"References", testReferences}, {"List-Unsubscribe", "<mailto:u@example.com>"}})
			if w.Code >= 300 || len(f.requests) != 1 || f.requests[0].Message.InReplyTo != "" || f.requests[0].Message.References != testReferences || f.requests[0].Message.ListUnsubscribe == "" {
				t.Fatal("mixed passthrough headers refused")
			}
		})
	}
}

func TestThreadingHeaderRejectionsEveryAdapter(t *testing.T) {
	manyIDs := strings.TrimSpace(strings.Repeat("<a@b.example> ", 101))
	cases := map[string][][2]string{
		"client message-id":    {{"Message-ID", "<x@example.org>"}},
		"message-id with ok":   {{"In-Reply-To", testReplyTo}, {"Message-Id", "<x@example.org>"}},
		"other header":         {{"In-Reply-To", testReplyTo}, {"X-Custom", "1"}},
		"reply-to duplicate":   {{"In-Reply-To", testReplyTo}, {"IN-REPLY-TO", testReplyTo}},
		"references duplicate": {{"References", testReferences}, {"references", testReferences}},
		"in-reply-to two ids":  {{"In-Reply-To", testReferences}},
		"in-reply-to empty":    {{"In-Reply-To", ""}},
		"references empty":     {{"References", "   "}},
		"bare id":              {{"In-Reply-To", "parent@mail.example.org"}},
		"comment":              {{"In-Reply-To", "<parent@mail.example.org> (c)"}},
		"obsolete quoted":      {{"In-Reply-To", "<\"a b\"@mail.example.org>"}},
		"folded input":         {{"References", "<a@b.example>\r\n <c@d.example>"}},
		"lf input":             {{"References", "<a@b.example>\n<c@d.example>"}},
		"injection":            {{"In-Reply-To", "<a@b.example>\r\nBcc: x@example.net"}},
		"non ascii":            {{"In-Reply-To", "<é@mail.example.org>"}},
		"too many ids":         {{"References", manyIDs}},
		"too long":             {{"References", strings.Repeat("<"+strings.Repeat("a", 900)+"@b.example> ", 10)}},
		"id over 998":          {{"In-Reply-To", "<" + strings.Repeat("a", 990) + "@mail.example.org>"}},
	}
	for name, pairs := range cases {
		for _, provider := range unsubscribeProviders {
			t.Run(provider+"/"+name, func(t *testing.T) {
				w, f := sendUnsubscribe(provider, pairs)
				if w.Code < 400 || len(f.requests) != 0 {
					t.Fatalf("expected rejection before acceptance, got %d with %d requests", w.Code, len(f.requests))
				}
				if strings.Contains(w.Body.String(), "example") {
					t.Fatal("rejection echoed header content")
				}
			})
		}
	}
}

func TestThreadingPostmarkBatchRejectsOnlyBadItem(t *testing.T) {
	f := &fakeService{app: submission.App{ID: "app-a"}}
	batch := `[{"From":"sender@example.com","To":"to@example.net","Subject":"a","TextBody":"b","Headers":[{"Name":"Message-ID","Value":"<x@example.org>"}]},{"From":"sender@example.com","To":"to@example.net","Subject":"ok","TextBody":"b","Headers":[{"Name":"In-Reply-To","Value":"<p@example.org>"},{"Name":"References","Value":"<r@example.org>  <p@example.org>"}]}]`
	w := request(t, New(f), "/email/batch", batch, map[string]string{"X-Postmark-Server-Token": "valid-key"})
	var results []struct{ ErrorCode int }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &results) != nil || len(results) != 2 || results[0].ErrorCode != 300 || results[1].ErrorCode != 0 || len(f.requests) != 1 ||
		f.requests[0].Message.InReplyTo != "<p@example.org>" || f.requests[0].Message.References != "<r@example.org> <p@example.org>" {
		t.Fatal("batch did not reject only the bad header item")
	}
}
