package message

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/submission"
)

func TestMessageIDHeaderUsesLowercasedSigningDomain(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef"
	input := submission.Message{From: "a@Example.COM", To: []string{"b@example.net"}, Subject: "s", Text: "body"}
	body, err := Build(input, Options{Recipient: "b@example.net", ID: id, Date: time.Now(), SigningDomain: "Example.COM", Selector: "mail", Signer: key})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(body))
	if err != nil || parsed.Header.Get("Message-ID") != "<"+id+"@example.com>" || parsed.Header.Get("Message-ID") != providerid.MessageIDHeader(id, "example.com") {
		t.Fatal("Message-ID is not <id@lowercased domain>", err, parsed.Header.Get("Message-ID"))
	}
}

func TestBuildSignedPerRecipientMIME(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	input := submission.Message{From: "Alice <alice@example.com>", To: []string{"Bob <bob@example.net>", "eve@example.net"}, ReplyTo: []string{"Help <help@example.com>", "support@example.com"}, Subject: "Hello ☃", Text: "plain body", HTML: "<b>html body</b>"}
	opts := Options{Recipient: "Bob <bob@example.net>", ID: "unique-123", Date: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), SigningDomain: "example.com", Selector: "mail", Signer: key}
	body, err := Build(input, opts)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(data []byte) bool {
		results, err := dkim.VerifyWithOptions(bytes.NewReader(data), &dkim.VerifyOptions{LookupTXT: func(name string) ([]string, error) {
			if name != "mail._domainkey.example.com" {
				return nil, fmt.Errorf("unexpected name %s", name)
			}
			return []string{"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
		}})
		return err == nil && len(results) == 1 && results[0].Err == nil
	}
	if !verify(body) {
		t.Fatal("valid DKIM signature failed verification")
	}
	if verify(bytes.Replace(body, []byte("plain body"), []byte("other body"), 1)) {
		t.Fatal("tampered message passed DKIM")
	}
	if verify(bytes.Replace(body, []byte("help@example.com"), []byte("evil@example.com"), 1)) {
		t.Fatal("tampered reply address passed DKIM")
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header.Get("Message-Id") != "<unique-123@example.com>" || strings.Contains(parsed.Header.Get("To"), "eve@example.net") {
		t.Fatal("incorrect message identity or recipient isolation")
	}
	replies, err := parsed.Header.AddressList("Reply-To")
	if err != nil || len(replies) != 2 || replies[0].Address != "help@example.com" || replies[1].Address != "support@example.com" {
		t.Fatal("reply addresses lost from signed MIME", err)
	}
	decoded, err := (&mime.WordDecoder{}).DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil || decoded != input.Subject {
		t.Fatalf("subject %q %v", decoded, err)
	}
	media, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil || media != "multipart/alternative" {
		t.Fatal("incorrect content type")
	}
	reader := multipart.NewReader(parsed.Body, params["boundary"])
	for _, expected := range []string{"plain body", "<b>html body</b>"} {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		var data []byte
		if part.Header.Get("Content-Transfer-Encoding") == "quoted-printable" {
			data, err = io.ReadAll(quotedprintable.NewReader(part))
		} else {
			data, err = io.ReadAll(part)
		}
		if err != nil || !strings.Contains(string(data), expected) {
			t.Fatalf("body mismatch %q %v", data, err)
		}
	}
}
func TestRejectsUnauthorizedRecipientAndSigningDomain(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	input := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "hi", Text: "body"}
	opts := Options{Recipient: "c@example.net", ID: "id", Date: time.Now(), SigningDomain: "example.com", Selector: "mail", Signer: key}
	if _, err = Build(input, opts); err == nil {
		t.Fatal("accepted recipient outside submission")
	}
	opts.Recipient = "b@example.net"
	opts.SigningDomain = "other.example"
	if _, err = Build(input, opts); err == nil {
		t.Fatal("accepted mismatched signing domain")
	}
}

func TestBuildSignsAttachmentAndInlineMIME(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	image := bytes.Repeat([]byte{1, 2, 3, 4, 5}, 40)
	file := []byte("synthetic private attachment")
	input := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "attachments", Text: "plain", HTML: `<img src="cid:logo@example.com">`, Attachments: []submission.Attachment{
		{Filename: "logo.png", ContentType: "image/png", Content: image, Inline: true, ContentID: "logo@example.com"},
		{Filename: "report ☃.txt", ContentType: "text/plain", Content: file},
	}}
	options := Options{Recipient: "b@example.net", ID: "attachment-test", Date: time.Now(), SigningDomain: "example.com", Selector: "mail", Signer: key}
	signed, err := Build(input, options)
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(data []byte) bool {
		results, verifyErr := dkim.VerifyWithOptions(bytes.NewReader(data), &dkim.VerifyOptions{LookupTXT: func(string) ([]string, error) {
			return []string{"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(public)}, nil
		}})
		return verifyErr == nil && len(results) == 1 && results[0].Err == nil
	}
	if !verify(signed) {
		t.Fatal("attachment MIME failed DKIM verification")
	}
	encodedFile := base64.StdEncoding.EncodeToString(file)
	tampered := bytes.Replace(signed, []byte(encodedFile), []byte("A"+encodedFile[1:]), 1)
	if bytes.Equal(tampered, signed) || verify(tampered) {
		t.Fatal("attachment tampering did not invalidate DKIM")
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(signed))
	if err != nil {
		t.Fatal(err)
	}
	reader := func(contentType string, body io.Reader, want string) *multipart.Reader {
		t.Helper()
		kind, params, parseErr := mime.ParseMediaType(contentType)
		if parseErr != nil || kind != want || params["boundary"] == "" {
			t.Fatal("wrong multipart nesting", kind, parseErr)
		}
		return multipart.NewReader(body, params["boundary"])
	}
	mixed := reader(parsed.Header.Get("Content-Type"), parsed.Body, "multipart/mixed")
	body, err := mixed.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	alt := reader(body.Header.Get("Content-Type"), body, "multipart/alternative")
	plain, err := alt.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	plainData, err := io.ReadAll(quotedprintable.NewReader(plain))
	if err != nil || strings.TrimSpace(string(plainData)) != "plain" {
		t.Fatal("plain body lost", err)
	}
	relatedPart, err := alt.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	related := reader(relatedPart.Header.Get("Content-Type"), relatedPart, "multipart/related")
	htmlPart, err := related.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	htmlData, err := io.ReadAll(quotedprintable.NewReader(htmlPart))
	if err != nil || !strings.Contains(string(htmlData), "cid:logo@example.com") {
		t.Fatal("HTML body lost", err)
	}
	inlinePart, err := related.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if inlinePart.Header.Get("Content-ID") != "<logo@example.com>" || inlinePart.Header.Get("Content-Transfer-Encoding") != "base64" {
		t.Fatal("inline metadata lost")
	}
	inlineData, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, inlinePart))
	if err != nil || !bytes.Equal(inlineData, image) {
		t.Fatal("inline bytes lost", err)
	}
	if _, err = related.NextPart(); err != io.EOF {
		t.Fatal("unexpected related part", err)
	}
	if _, err = alt.NextPart(); err != io.EOF {
		t.Fatal("unexpected alternative part", err)
	}
	filePart, err := mixed.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	disposition, params, err := mime.ParseMediaType(filePart.Header.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || params["filename"] != "report ☃.txt" {
		t.Fatal("filename or disposition lost", err)
	}
	fileData, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, filePart))
	if err != nil || !bytes.Equal(fileData, file) {
		t.Fatal("attachment bytes lost", err)
	}
	if _, err = mixed.NextPart(); err != io.EOF {
		t.Fatal("unexpected attachment", err)
	}
}

func TestAttachmentMIMEBodyVariants(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, text, html, expected string
		inline                     bool
	}{
		{"plain and file", "plain", "", "text/plain", false},
		{"html and file", "", "<b>html</b>", "text/html", false},
		{"html and inline", "", `<img src="cid:logo">`, "multipart/related", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			attachment := submission.Attachment{Filename: "fixture.bin", ContentType: "application/octet-stream", Content: []byte{}}
			if test.inline {
				attachment.Inline, attachment.ContentID = true, "logo"
			}
			input := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "fixture", Text: test.text, HTML: test.html, Attachments: []submission.Attachment{attachment}}
			signed, buildErr := Build(input, Options{Recipient: "b@example.net", ID: "fixture", Date: time.Now(), SigningDomain: "example.com", Selector: "mail", Signer: key})
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			parsed, parseErr := mail.ReadMessage(bytes.NewReader(signed))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			kind, params, parseErr := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
			if parseErr != nil || kind != "multipart/mixed" {
				t.Fatal("wrong outer MIME", parseErr)
			}
			mixed := multipart.NewReader(parsed.Body, params["boundary"])
			body, partErr := mixed.NextPart()
			if partErr != nil {
				t.Fatal(partErr)
			}
			bodyKind, _, parseErr := mime.ParseMediaType(body.Header.Get("Content-Type"))
			if parseErr != nil || bodyKind != test.expected {
				t.Fatal("wrong body MIME", bodyKind, parseErr)
			}
		})
	}
}

func TestBuildEmitsAndSignsListUnsubscribe(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(data []byte) bool {
		results, err := dkim.VerifyWithOptions(bytes.NewReader(data), &dkim.VerifyOptions{LookupTXT: func(string) ([]string, error) {
			return []string{"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
		}})
		return err == nil && len(results) == 1 && results[0].Err == nil
	}
	input := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "body", ListUnsubscribe: "<https://app.example.com/u/1>, <mailto:unsub@example.com>", ListUnsubscribePost: true}
	opts := Options{Recipient: "b@example.net", ID: "id-1", Date: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), SigningDomain: "example.com", Selector: "mail", Signer: key}
	body, err := Build(input, opts)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(body))
	if err != nil || parsed.Header.Get("List-Unsubscribe") != input.ListUnsubscribe || parsed.Header.Get("List-Unsubscribe-Post") != "List-Unsubscribe=One-Click" {
		t.Fatal("headers not emitted", err)
	}
	signature := strings.ToLower(parsed.Header.Get("Dkim-Signature"))
	if !strings.Contains(signature, "list-unsubscribe:") && !strings.Contains(signature, "list-unsubscribe;") || !strings.Contains(signature, "list-unsubscribe-post") {
		t.Fatal("DKIM does not cover List-Unsubscribe headers")
	}
	if !verify(body) {
		t.Fatal("signature failed verification")
	}
	if verify(bytes.Replace(body, []byte("app.example.com/u/1"), []byte("evil.example.com/u/1"), 1)) {
		t.Fatal("tampered List-Unsubscribe passed DKIM")
	}
	plain, err := Build(submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "body"}, opts)
	if err != nil || bytes.Contains(bytes.ToLower(plain), []byte("list-unsubscribe")) {
		t.Fatal("headers emitted for a message without them", err)
	}
	input.ListUnsubscribe = "<https://app.example.com/u/1>\r\nBcc: x@example.net"
	if _, err = Build(input, opts); err == nil {
		t.Fatal("injected header value was accepted")
	}
}

func TestBuildEmitsAndSignsThreadingHeaders(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(data []byte) bool {
		results, err := dkim.VerifyWithOptions(bytes.NewReader(data), &dkim.VerifyOptions{LookupTXT: func(string) ([]string, error) {
			return []string{"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
		}})
		return err == nil && len(results) == 1 && results[0].Err == nil
	}
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = fmt.Sprintf("<%d.q%s@mail%d.example.com>", i, strings.Repeat("q", i%30), i)
	}
	long := "<" + strings.Repeat("l", 980) + "@example.com>"
	ids[50] = long
	input := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "body", InReplyTo: ids[99], References: strings.Join(ids, " ")}
	opts := Options{Recipient: "b@example.net", ID: "id-1", Date: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), SigningDomain: "example.com", Selector: "mail", Signer: key}
	body, err := Build(input, opts)
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := strings.Cut(string(body), "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		if len(line) > 998 {
			t.Fatal("line over 998 bytes")
		}
		if (strings.HasPrefix(line, "References:") || strings.HasPrefix(line, " <")) && len(line) > 78 && !strings.Contains(line, long) {
			t.Fatalf("References line of %d bytes", len(line))
		}
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(body))
	if err != nil || parsed.Header.Get("In-Reply-To") != ids[99] || parsed.Header.Get("Message-Id") != "<id-1@example.com>" {
		t.Fatal("In-Reply-To or Message-ID wrong", err)
	}
	if got := strings.Fields(parsed.Header.Get("References")); strings.Join(got, " ") != input.References {
		t.Fatal("References do not round-trip through folding")
	}
	signature := strings.ToLower(parsed.Header.Get("Dkim-Signature"))
	if !strings.Contains(signature, "references") || !strings.Contains(signature, "in-reply-to") {
		t.Fatal("DKIM does not cover threading headers")
	}
	if !verify(body) {
		t.Fatal("signature over folded References failed verification")
	}
	if verify(bytes.Replace(body, []byte("<1.qq@mail1.example.com>"), []byte("<9.qq@mail1.example.com>"), 1)) {
		t.Fatal("tampered References passed DKIM")
	}
	plain, err := Build(submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "body"}, opts)
	if err != nil || bytes.Contains(bytes.ToLower(plain), []byte("references")) || bytes.Contains(bytes.ToLower(plain), []byte("in-reply-to")) {
		t.Fatal("headers emitted for a message without them", err)
	}
	for _, bad := range []submission.Message{
		{InReplyTo: "<a@b>\r\nBcc: x@example.net"}, {References: "<a@b>\r\nBcc: x@example.net"}, {References: "<a@b>  <c@d>"}, {InReplyTo: "junk"},
	} {
		bad.From, bad.To, bad.Text = input.From, input.To, "body"
		if _, err = Build(bad, opts); err == nil {
			t.Fatal("invalid threading header accepted by Build")
		}
	}
}

func TestBuildFoldsLongFirstReference(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Recipient: "b@example.net", ID: "id-1", Date: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), SigningDomain: "example.com", Selector: "mail", Signer: key}
	for _, n := range []int{986, 990, 997} {
		first := "<" + strings.Repeat("f", n-len("<@example.com>")) + "@example.com>"
		refs := first + " <second@example.com>"
		body, err := Build(submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "body", References: refs}, opts)
		if err != nil {
			t.Fatal(n, err)
		}
		head, _, _ := strings.Cut(string(body), "\r\n\r\n")
		for _, line := range strings.Split(head, "\r\n") {
			if len(line) > 998 {
				t.Fatalf("first id of %d bytes produced a %d-byte line", n, len(line))
			}
		}
		parsed, err := mail.ReadMessage(bytes.NewReader(body))
		if err != nil || strings.Join(strings.Fields(parsed.Header.Get("References")), " ") != refs {
			t.Fatal("References do not round-trip", n, err)
		}
		results, err := dkim.VerifyWithOptions(bytes.NewReader(body), &dkim.VerifyOptions{LookupTXT: func(string) ([]string, error) {
			return []string{"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
		}})
		if err != nil || len(results) != 1 || results[0].Err != nil {
			t.Fatal("signature failed", n, err)
		}
	}
}

func TestFoldReferencesPrefersShortLines(t *testing.T) {
	got := foldReferences("<aaaaaaaaaaaaaaaaa@example.com> <bbbbbbbbbbbbbbbbb@example.com> <ccccccccccccccccc@example.com>")
	want := "References: <aaaaaaaaaaaaaaaaa@example.com> <bbbbbbbbbbbbbbbbb@example.com>\r\n <ccccccccccccccccc@example.com>\r\n"
	if got != want {
		t.Fatalf("%q", got)
	}
}

func TestNonASCIIDisplayNamesAreEncodedAndSigned(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	input := submission.Message{From: `"Ada Ünï, Co" <ada@example.com>`, To: []string{`"Lovelace, Zoë" <zoe@example.net>`, "Bob <bob@example.net>"}, ReplyTo: []string{`"Hélp, Desk" <help@example.com>`}, Subject: "Hello", Text: "plain body"}
	body, err := Build(input, Options{Recipient: input.To[0], ID: "n-1", Date: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), SigningDomain: "example.com", Selector: "mail", Signer: key})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	results, err := dkim.VerifyWithOptions(bytes.NewReader(body), &dkim.VerifyOptions{LookupTXT: func(string) ([]string, error) {
		return []string{"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
	}})
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatal("DKIM failed for non-ASCII names", err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"From", "To", "Reply-To"} {
		for _, r := range parsed.Header.Get(name) {
			if r > 127 {
				t.Fatalf("%s header carries raw non-ASCII", name)
			}
		}
	}
	from, err := parsed.Header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Name != "Ada Ünï, Co" || from[0].Address != "ada@example.com" {
		t.Fatal("From name did not round trip", err)
	}
	to, err := parsed.Header.AddressList("To")
	if err != nil || len(to) != 1 || to[0].Name != "Lovelace, Zoë" || to[0].Address != "zoe@example.net" {
		t.Fatal("recipient's own To did not round trip, or lists other recipients", err)
	}
	reply, err := parsed.Header.AddressList("Reply-To")
	if err != nil || len(reply) != 1 || reply[0].Name != "Hélp, Desk" {
		t.Fatal("Reply-To name did not round trip", err)
	}
}
