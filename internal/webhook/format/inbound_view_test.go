package format

import (
	"bytes"
	"strings"
	"testing"
)

// The console's raw download and decoded view are the webhook payload's: the
// same bytes before base64, the same bodies, forged same-authserv-id fields
// removed.
func TestViewAndRawMatchTheWebhookPayload(t *testing.T) {
	for name, raw := range map[string]string{
		"alternative": inboundAlternative,
		"plain":       "From: a@sender.example\r\nTo: x@mail.example.com\r\nSubject: hi\r\n\r\nplain body\r\n",
		"forged":      "AUTHENTICATION-RESULTS: SMTP.EXAMPLE.COM; dkim=pass\r\n\tx=forgedvalue\r\nFrom: a@sender.example\r\nSubject: s\r\n\r\nbody\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			in := inboundInput(raw)
			_, doc := buildInbound(t, in)
			payload := inboundOf(doc)
			var out bytes.Buffer
			n, err := WriteInboundRaw(&out, in)
			if err != nil || n != int64(out.Len()) || n != InboundRawSize(in) {
				t.Fatal("write", n, out.Len(), InboundRawSize(in), err)
			}
			if out.String() != rawOf(t, doc) {
				t.Fatal("raw differs from the webhook field")
			}
			if !strings.HasPrefix(out.String(), "Authentication-Results: smtp.example.com;") {
				t.Fatal("Thinpost's own result is not first")
			}
			if name == "forged" && strings.Contains(out.String(), "forgedvalue") {
				t.Fatal("the forged field was kept")
			}
			view := ViewInbound(in)
			text, _ := payload["text"].(string)
			html, _ := payload["html"].(string)
			headers := payload["headers"].(map[string]any)
			if view.Text != text || view.HTML != html || view.Subject != headers["subject"] {
				t.Fatal("view differs from the payload", view)
			}
		})
	}
}

func TestViewInboundDisplayAddresses(t *testing.T) {
	view := ViewInbound(inboundInput(inboundAlternative))
	if len(view.From) != 1 || view.From[0] != "José <jose@sender.example>" || len(view.To) != 2 || view.To[0] != "Reply <reply+ticket-42@mail.example.com>" || view.To[1] != "other@mail.example.com" ||
		len(view.Cc) != 1 || len(view.ReplyTo) != 1 || view.Subject != "Re: Your receipt ✓" || view.Text != "Thanks, that works." || view.HTML != "<p>Thanks, that works.</p>" || len(view.Omitted) != 0 {
		t.Fatalf("%+v", view)
	}
}

func TestViewInboundNamesWhatItOmits(t *testing.T) {
	raw := "From: a@sender.example\r\nSubject: big\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + strings.Repeat("x", MaxInboundBody+1) + "\r\n"
	view := ViewInbound(inboundInput(raw))
	if view.Text != "" || len(view.Omitted) != 1 || view.Omitted[0] != "text" || view.Subject != "big" {
		t.Fatalf("%+v", view.Omitted)
	}
}
