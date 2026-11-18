package submission

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestAttachmentValidationAndLegacyEncoding(t *testing.T) {
	base := Message{From: "sender@example.com", To: []string{"recipient@example.net"}, Subject: "subject", HTML: "<img src=\"cid:logo\">"}
	before, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Attachments = []Attachment{}
	after, err := json.Marshal(base)
	if err != nil || string(before) != string(after) {
		t.Fatal("empty attachment list changed the legacy fingerprint")
	}
	valid := Attachment{Filename: "report ☃.pdf", ContentType: "application/pdf", Content: []byte("private content")}
	inline := Attachment{Filename: "logo.png", ContentType: "image/png", Content: []byte{}, Inline: true, ContentID: "logo@example.com"}
	base.Attachments = []Attachment{valid, inline}
	if err = base.Validate(); err != nil {
		t.Fatal("valid attachments rejected", err)
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Message
	if err = json.Unmarshal(encoded, &decoded); err != nil || decoded.Validate() != nil || decoded.Attachments[1].Content == nil {
		t.Fatal("attachment source round trip failed", err)
	}
	if strings.Contains(fmt.Sprintf("%v %#v", valid, valid), "private content") {
		t.Fatal("attachment formatter exposed content")
	}
	for _, test := range []struct {
		name   string
		mutate func(*Message)
	}{
		{"missing content", func(m *Message) { m.Attachments[0].Content = nil }},
		{"oversized content", func(m *Message) { m.Attachments[0].Content = make([]byte, MaxAttachmentBytes+1) }},
		{"too many", func(m *Message) { m.Attachments = make([]Attachment, MaxAttachments+1) }},
		{"path", func(m *Message) { m.Attachments[0].Filename = "../secret" }},
		{"backslash", func(m *Message) { m.Attachments[0].Filename = `folder\file` }},
		{"header injection", func(m *Message) { m.Attachments[0].Filename = "file\r\nX-Bad: yes" }},
		{"type injection", func(m *Message) { m.Attachments[0].ContentType = "text/plain\r\nX-Bad: yes" }},
		{"type parameter", func(m *Message) { m.Attachments[0].ContentType = "text/plain; charset=utf-8" }},
		{"missing subtype", func(m *Message) { m.Attachments[0].ContentType = "text" }},
		{"wildcard type", func(m *Message) { m.Attachments[0].ContentType = "image/*" }},
		{"inline without html", func(m *Message) { m.HTML = ""; m.Text = "plain" }},
		{"inline without id", func(m *Message) { m.Attachments[1].ContentID = "" }},
		{"malformed id", func(m *Message) { m.Attachments[1].ContentID = "x>\r\nBad: y" }},
		{"id on ordinary attachment", func(m *Message) { m.Attachments[0].ContentID = "logo" }},
		{"duplicate inline id", func(m *Message) {
			m.Attachments = append(m.Attachments, Attachment{Filename: "other.png", ContentType: "image/png", Content: []byte("a"), Inline: true, ContentID: "LOGO@example.com"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := base
			message.Attachments = append([]Attachment(nil), base.Attachments...)
			test.mutate(&message)
			if message.Validate() == nil {
				t.Fatal("invalid attachment accepted")
			}
		})
	}
}

func TestAttachmentCeilingAndAdmissionLimit(t *testing.T) {
	m := Message{From: "sender@example.com", To: []string{"recipient@example.net"}, Subject: "subject", Text: "t"}
	m.Attachments = []Attachment{{Filename: "a.bin", ContentType: "application/octet-stream", Content: make([]byte, MaxAttachmentBytes-10)}, {Filename: "b.bin", ContentType: "application/octet-stream", Content: make([]byte, 10)}}
	if m.Validate() != nil {
		t.Fatal("total at the ceiling rejected")
	}
	m.Attachments[1].Content = make([]byte, 11)
	if m.Validate() == nil {
		t.Fatal("total above the ceiling accepted")
	}
	if DefaultMaxAttachmentBytes != 5<<20 || MaxAttachmentBytes < DefaultMaxAttachmentBytes {
		t.Fatal("default must fit under the ceiling")
	}
	// Admission applies the configured total on top of the strict check.
	m.Attachments = []Attachment{{Filename: "a.bin", ContentType: "application/octet-stream", Content: make([]byte, 600)}, {Filename: "b.bin", ContentType: "application/octet-stream", Content: make([]byte, 400)}}
	if m.Validate() != nil || m.ValidateAttachmentLimit(1000) != nil || m.ValidateAttachmentLimit(999) == nil {
		t.Fatal("configured limit not enforced as a total")
	}
}
