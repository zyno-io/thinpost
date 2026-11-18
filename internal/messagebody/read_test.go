package messagebody

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/message"
	"github.com/zyno-io/thinpost/internal/submission"
)

// repo records each authorization. Like the PostgreSQL implementation it
// audits only Enhanced Protection reads, and only before returning a key.
type repo struct {
	actor, app, message string
	err                 error
	protected           bool
	audited             bool
	audits              []string
}

func (r *repo) authorize(action, resource string) (string, bool, error) {
	if r.err != nil {
		return "", false, r.err
	}
	if r.protected {
		r.audited = true
		r.audits = append(r.audits, action+" "+resource)
	}
	return "source", r.protected, nil
}

func (r *repo) AuthorizeBodyRead(_ context.Context, actor, app, message string) (string, bool, error) {
	r.actor, r.app, r.message = actor, app, message
	return r.authorize("message.body.read", message)
}
func (r *repo) AuthorizeAttachmentRead(_ context.Context, actor, app, message string, index int) (string, bool, error) {
	r.actor, r.app, r.message = actor, app, message
	return r.authorize("message.attachment.read", message+":"+string(rune('0'+index)))
}
func (r *repo) AuthorizeDeliveryHeadersRead(_ context.Context, actor, app, message, delivery string) (string, bool, error) {
	r.actor, r.app, r.message = actor, app, message
	return r.authorize("message.delivery.headers.read", message+":"+delivery)
}

type objects struct {
	repo *repo
	data []byte
	gets int
}

func (o *objects) Get(_ context.Context, key string, _ int64) ([]byte, error) {
	o.gets++
	if o.repo.protected && !o.repo.audited || key != "source" {
		return nil, errors.New("not audited")
	}
	return o.data, nil
}

func source(t *testing.T, m submission.Message) []byte {
	t.Helper()
	data, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{1, m})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBodyReadAuditsBeforeFetchingAndPreservesPolicy(t *testing.T) {
	m := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, ReplyTo: []string{"help@example.com"}, Subject: "subject", Text: "private content", EnhancedProtection: true}
	r := &repo{protected: true}
	o := &objects{repo: r, data: source(t, m)}
	service := &Service{Repository: r, Objects: o}
	body, err := service.ReadMessageBody(context.Background(), "operator", "app", "message")
	if err != nil || body.Text != "private content" || !body.EnhancedProtection || len(body.ReplyTo) != 1 || body.ReplyTo[0] != "help@example.com" || r.actor != "operator" || r.app != "app" {
		t.Fatal("incorrect body read")
	}
	if body.Attachments == nil || len(body.Attachments) != 0 {
		t.Fatal("legacy source must report an empty attachment array")
	}
	if len(r.audits) != 1 || r.audits[0] != "message.body.read message" {
		t.Fatal("EP body read not audited", r.audits)
	}
	m.EnhancedProtection = false
	o.data = source(t, m)
	if _, err = service.ReadMessageBody(context.Background(), "operator", "app", "message"); err == nil {
		t.Fatal("accepted corrupted TLS policy")
	}
}

func TestNonProtectedReadsAreNotAudited(t *testing.T) {
	m := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "t", HTML: "<b>t</b>", Attachments: []submission.Attachment{{Filename: "a.txt", ContentType: "text/plain", Content: []byte("one")}}}
	r := &repo{}
	service := &Service{Repository: r, Objects: &objects{repo: r, data: source(t, m)}}
	if _, err := service.ReadMessageBody(context.Background(), "operator", "app", "message"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReadAttachment(context.Background(), "operator", "app", "message", 0); err != nil {
		t.Fatal(err)
	}
	if len(r.audits) != 0 {
		t.Fatal("non-EP reads audited", r.audits)
	}
}

func TestAttachmentMetadataAndDownload(t *testing.T) {
	m := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "t", HTML: `<img src="cid:logo@example.com">`, EnhancedProtection: true, Attachments: []submission.Attachment{
		{Filename: "report.html", ContentType: "text/html", Content: []byte("<script>x</script>")},
		{Filename: "logo.png", ContentType: "image/png", Content: []byte{1, 2, 3}, Inline: true, ContentID: "logo@example.com"},
	}}
	r := &repo{protected: true}
	o := &objects{repo: r, data: source(t, m)}
	service := &Service{Repository: r, Objects: o}
	body, err := service.ReadMessageBody(context.Background(), "operator", "app", "message")
	if err != nil || len(body.Attachments) != 2 {
		t.Fatal("attachment metadata missing", err)
	}
	first, second := body.Attachments[0], body.Attachments[1]
	if first.Index != 0 || first.Filename != "report.html" || first.ContentType != "text/html" || first.Size != 18 || first.Inline || first.ContentID != "" ||
		second.Index != 1 || !second.Inline || second.ContentID != "logo@example.com" || second.Size != 3 {
		t.Fatalf("incorrect metadata: %+v", body.Attachments)
	}
	r.audits = nil
	content, err := service.ReadAttachment(context.Background(), "operator", "app", "message", 1)
	if err != nil || string(content.Content) != "\x01\x02\x03" || content.Filename != "logo.png" {
		t.Fatal("attachment bytes wrong", err)
	}
	if len(r.audits) != 1 || r.audits[0] != "message.attachment.read message:1" {
		t.Fatal("EP attachment read not audited", r.audits)
	}
	gets := o.gets
	for _, index := range []int{2, 19, 20, -1} {
		if _, err = service.ReadAttachment(context.Background(), "operator", "app", "message", index); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("out-of-range attachment found", index, err)
		}
	}
	if o.gets != gets+2 { // 2 and 19 were authorized and fetched; 20 and -1 were rejected first
		t.Fatal("index syntax must be checked before authorization", o.gets-gets)
	}
}

func TestUnauthorizedContentNeverFetched(t *testing.T) {
	r := &repo{err: admin.ErrNotFound}
	o := &objects{repo: r}
	service := &Service{Repository: r, Objects: o}
	if _, err := service.ReadMessageBody(context.Background(), "operator", "other-app", "message"); !errors.Is(err, admin.ErrNotFound) || o.gets != 0 {
		t.Fatal("unauthorized body fetched")
	}
	if _, err := service.ReadAttachment(context.Background(), "operator", "other-app", "message", 0); !errors.Is(err, admin.ErrNotFound) || o.gets != 0 {
		t.Fatal("unauthorized attachment fetched")
	}
	if _, err := service.ReadDeliveryHeaders(context.Background(), "operator", "other-app", "message", "delivery"); !errors.Is(err, admin.ErrNotFound) || o.gets != 0 {
		t.Fatal("unauthorized headers fetched")
	}
}

func TestDeliveryHeadersFromSignedMIME(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := submission.Message{From: "Sender <sender@example.com>", To: []string{"b@example.net"}, ReplyTo: []string{"help@example.com"}, Subject: "Café invoice", Text: "private body", EnhancedProtection: true}
	final, err := message.Build(m, message.Options{Recipient: "Bob <b@example.net>", ID: "0123456789abcdef0123456789abcdef", Date: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), SigningDomain: "example.com", Selector: "mail", Signer: key})
	if err != nil {
		t.Fatal(err)
	}
	r := &repo{protected: true}
	service := &Service{Repository: r, Objects: &objects{repo: r, data: final}}
	headers, err := service.ReadDeliveryHeaders(context.Background(), "operator", "app", "message", "delivery")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]admin.HeaderField{}
	for _, h := range headers {
		names = append(names, h.Name)
		byName[h.Name] = h
	}
	if got := strings.Join(names, ","); got != "DKIM-Signature,From,To,Subject,Date,Message-ID,MIME-Version,Reply-To,Content-Type,Content-Transfer-Encoding" {
		t.Fatal("stored order not preserved:", got)
	}
	if names[0] != "DKIM-Signature" {
		t.Fatal("DKIM-Signature must be first")
	}
	dkim := byName["DKIM-Signature"].Value
	if strings.ContainsAny(dkim, "\r\n") || !strings.Contains(dkim, "d=example.com") || !strings.Contains(dkim, "bh=") {
		t.Fatal("folded DKIM-Signature not unfolded", dkim)
	}
	subject := byName["Subject"]
	if !strings.HasPrefix(subject.Value, "=?utf-8?") || subject.DecodedValue != "Café invoice" {
		t.Fatalf("subject not decoded: %+v", subject)
	}
	if byName["Message-ID"].Value != "<0123456789abcdef0123456789abcdef@example.com>" || byName["Message-ID"].DecodedValue != "" {
		t.Fatal("plain header altered", byName["Message-ID"])
	}
	for _, h := range headers {
		if strings.Contains(h.Value, "private body") {
			t.Fatal("body leaked into headers")
		}
	}
	if len(r.audits) != 1 || r.audits[0] != "message.delivery.headers.read message:delivery" {
		t.Fatal("EP header read not audited", r.audits)
	}
}

func TestParseHeadersIsStrict(t *testing.T) {
	good := "From: a@example.com\r\nSubject: one\r\n two\t three\r\nX-Empty:\r\n\r\nbody: not a header\r\n"
	headers, err := ParseHeaders([]byte(good))
	if err != nil || len(headers) != 3 || headers[1].Value != "one two\t three" || headers[2].Value != "" || headers[0].Name != "From" {
		t.Fatalf("good block misparsed: %+v %v", headers, err)
	}
	var many strings.Builder
	for i := 0; i < 101; i++ {
		many.WriteString("X-H: v\r\n")
	}
	many.WriteString("\r\n")
	hundred := strings.Replace(many.String(), "X-H: v\r\n", "", 1)
	if _, err = ParseHeaders([]byte(hundred)); err != nil {
		t.Fatal("100 fields rejected", err)
	}
	for name, input := range map[string]string{
		"no terminator":        "From: a@example.com\r\nSubject: x\r\n",
		"leading continuation": " folded\r\nFrom: a@example.com\r\n\r\n",
		"no colon":             "From a@example.com\r\n\r\n",
		"empty name":           ": value\r\n\r\n",
		"space in name":        "Bad Name: v\r\n\r\n",
		"bare LF":              "From: a@example.com\nSubject: x\n\n",
		"too many fields":      many.String(),
		"empty message":        "",
		"no fields":            "\r\n\r\n",
		"oversized block":      "X: " + strings.Repeat("a", 70000) + "\r\n\r\n",
	} {
		if _, err := ParseHeaders([]byte(input)); err == nil {
			t.Fatal("accepted:", name)
		} else if strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), "Bad Name") {
			t.Fatal("error quotes input:", name)
		}
	}
}

// Readers validate stored sources against the fixed ceiling only, so an
// attachment accepted under a larger configured limit stays readable after the
// limit is lowered, and one above the ceiling is not.
func TestReadersUseAttachmentCeilingNotConfiguredLimit(t *testing.T) {
	m := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "t", Attachments: []submission.Attachment{{Filename: "big.bin", ContentType: "application/octet-stream", Content: make([]byte, submission.MaxAttachmentBytes)}}}
	r := &repo{}
	service := &Service{Repository: r, Objects: &objects{repo: r, data: source(t, m)}}
	if _, err := service.ReadMessageBody(context.Background(), "operator", "app", "message"); err != nil {
		t.Fatal("ceiling-size source unreadable", err)
	}
	if content, err := service.ReadAttachment(context.Background(), "operator", "app", "message", 0); err != nil || len(content.Content) != submission.MaxAttachmentBytes {
		t.Fatal("ceiling-size attachment unreadable", err)
	}
	m.Attachments[0].Content = make([]byte, submission.MaxAttachmentBytes+1)
	service = &Service{Repository: r, Objects: &objects{repo: r, data: source(t, m)}}
	if _, err := service.ReadMessageBody(context.Background(), "operator", "app", "message"); err == nil {
		t.Fatal("source above the ceiling accepted")
	}
}
