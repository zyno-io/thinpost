package format

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var receivedAt = time.Date(2026, 11, 5, 11, 59, 59, 250000000, time.UTC)

const inboundAlternative = "From: =?utf-8?q?Jos=C3=A9?= <jose@sender.example>\r\n" +
	"To: Reply <reply+ticket-42@mail.example.com>, other@mail.example.com\r\n" +
	"Cc: cc@example.net\r\n" +
	"Reply-To: replies@sender.example\r\n" +
	"Subject: =?utf-8?q?Re=3A_Your_receipt_=E2=9C=93?=\r\n" +
	"Date: Thu, 05 Nov 2026 11:59:50 +0000\r\n" +
	"Message-ID: <reply-1@sender.example>\r\n" +
	"In-Reply-To: <0123456789abcdef0123456789abcdef@example.com>\r\n" +
	"References: <a@x> <0123456789abcdef0123456789abcdef@example.com>\r\n" +
	"Auto-Submitted: auto-replied\r\n" +
	"Authentication-Results: smtp.example.com; spf=pass smtp.mailfrom=sender.example; dmarc=pass header.from=sender.example\r\n" +
	"Authentication-Results: elsewhere.example; dkim=pass header.d=sender.example\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/alternative; boundary=\"b1\"\r\n" +
	"\r\n" +
	"--b1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nThanks, that works.\r\n" +
	"--b1\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Thanks, that works.</p>\r\n" +
	"--b1--\r\n"

func inboundInput(raw string) *InboundInput {
	yes := true
	return &InboundInput{
		ID: "11112222333344445555666677778888", ReceivedAt: receivedAt, Recipient: "reply+ticket-42@mail.example.com",
		Recipients: []string{"reply+ticket-42@mail.example.com", "other@mail.example.com"},
		MailFrom:   "bounces@sender.example", ClientIP: "198.51.100.7", Helo: "mail.sender.example", TLSVersion: "TLS1.3", TLSCipher: "TLS_AES_128_GCM_SHA256",
		Receiver:       "smtp.example.com",
		Raw:            []byte(raw),
		Authentication: json.RawMessage(`{"spf":{"result":"pass","domain":"sender.example","identity":"mailfrom"},"dkim":[{"result":"pass","domain":"sender.example"}],"dmarc":{"result":"pass","domain":"sender.example","policy":"reject"},"arc":{"present":false,"evaluated":false}}`),
		SPF:            "pass", DKIM: "pass", DMARC: "pass",
		Correlation: &InboundCorrelation{MessageID: fixedMessage, DeliveryID: "delivery-1", Header: "in_reply_to", FromMatches: &yes},
	}
}

func buildInbound(t *testing.T, in *InboundInput) (Request, map[string]any) {
	t.Helper()
	impl, _ := Lookup(ThinpostInbound)
	req, err := impl.Build(Input{Event: Event{ID: "evt_inbound", Outcome: InboundReceived, OccurredAt: receivedAt}, AppID: "app-1", Secret: thinpostSecret, Now: sentAt, Inbound: in})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(req.Body, &doc); err != nil {
		t.Fatal(err)
	}
	return req, doc
}

func inboundOf(doc map[string]any) map[string]any { return doc["inbound"].(map[string]any) }

func rawOf(t *testing.T, doc map[string]any) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(inboundOf(doc)["raw"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The goldens are hand-reviewed and never regenerated: a diff means the native
// inbound payload changed. (-update writes them once, for review.)
func TestInboundGolden(t *testing.T) {
	impl, _ := Lookup(ThinpostInbound)
	null := inboundInput("From: a@sender.example\r\nTo: x@mail.example.com\r\nSubject: hi\r\n\r\nplain body\r\n")
	null.MailFrom, null.Correlation, null.TLSVersion, null.TLSCipher, null.Authentication = "", nil, "", "", nil
	null.SPF, null.DKIM, null.DMARC = "none", "none", "none"
	null.Recipient, null.Recipients = "x@mail.example.com", nil
	for name, in := range map[string]*InboundInput{"full": inboundInput(inboundAlternative), "minimal": null} {
		t.Run(name, func(t *testing.T) {
			req, _ := buildInbound(t, in)
			compareGolden(t, ThinpostInbound, name, req, false)
		})
	}
	t.Run("test", func(t *testing.T) {
		req, err := impl.Build(Input{Event: Event{ID: "evt_test", OccurredAt: sentAt}, AppID: "app-1", Secret: thinpostSecret, Now: sentAt, Test: true})
		if err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("X-Thinpost-Test") != "true" {
			t.Error("missing test header")
		}
		compareGolden(t, ThinpostInbound, "test", req, false)
	})
}

func TestInboundSignatureVerifiesLikeNative(t *testing.T) {
	req, _ := buildInbound(t, inboundInput(inboundAlternative))
	if !VerifyThinpost(thinpostSecret, "evt_inbound", req.Header.Get("X-Thinpost-Timestamp"), req.Body, req.Header.Get("X-Thinpost-Signature"), sentAt, 5*time.Minute) {
		t.Fatal("signature does not verify")
	}
	if VerifyThinpost(thinpostSecret, "evt_inbound", req.Header.Get("X-Thinpost-Timestamp"), append([]byte(" "), req.Body...), req.Header.Get("X-Thinpost-Signature"), sentAt, 5*time.Minute) {
		t.Fatal("a modified body verifies")
	}
}

func TestInboundPayloadShape(t *testing.T) {
	_, doc := buildInbound(t, inboundInput(inboundAlternative))
	in := inboundOf(doc)
	if doc["outcome"] != "inbound_received" || doc["app_id"] != "app-1" || doc["id"] != "evt_inbound" {
		t.Fatal("envelope keys", doc)
	}
	if _, ok := in["attachments"]; ok {
		t.Fatal("attachments are not listed separately")
	}
	rec := in["recipient"].(map[string]any)
	if rec["local_part"] != "reply+ticket-42" || rec["tag"] != "ticket-42" || rec["domain"] != "mail.example.com" {
		t.Fatal("recipient", rec)
	}
	headers := in["headers"].(map[string]any)
	if headers["subject"] != "Re: Your receipt ✓" || headers["from"].([]any)[0].(map[string]any)["name"] != "José" {
		t.Fatal("decoded headers", headers)
	}
	if len(headers["references"].([]any)) != 2 || headers["auto_submitted"] != "auto-replied" {
		t.Fatal("references or auto-submitted", headers)
	}
	if in["text"] != "Thanks, that works." || in["html"] != "<p>Thanks, that works.</p>" || len(in["omitted"].([]any)) != 0 {
		t.Fatal("bodies", in["text"], in["html"], in["omitted"])
	}
	if !in["classification"].(map[string]any)["auto_submitted"].(bool) {
		t.Fatal("classification")
	}
	corr := in["correlation"].(map[string]any)
	if corr["matched"] != true || corr["delivery_id"] != "delivery-1" || corr["header"] != "in_reply_to" || corr["from_matches_recipient"] != true {
		t.Fatal("correlation", corr)
	}
	auth := in["authentication"].(map[string]any)
	if auth["arc"].(map[string]any)["evaluated"] != false || auth["spf"].(map[string]any)["result"] != "pass" {
		t.Fatal("authentication", auth)
	}
	if env := in["envelope"].(map[string]any); env["tls"].(map[string]any)["version"] != "TLS1.3" || env["mail_from"] != "bounces@sender.example" {
		t.Fatal("envelope", env)
	}
	// Unmatched, null sender, no TLS.
	m := inboundInput("From: a@b.example\r\n\r\nx\r\n")
	m.Correlation, m.MailFrom, m.TLSVersion, m.TLSCipher = nil, "", "", ""
	_, doc = buildInbound(t, m)
	in = inboundOf(doc)
	if c := in["correlation"].(map[string]any); c["matched"] != false || len(c) != 1 {
		t.Fatal("unmatched correlation", c)
	}
	if env := in["envelope"].(map[string]any); env["tls"] != nil || env["mail_from"] != "" {
		t.Fatal("envelope", env)
	}
}

func TestInboundRawPrependsResultsAndRemovesForgedClaims(t *testing.T) {
	_, doc := buildInbound(t, inboundInput(inboundAlternative))
	raw := rawOf(t, doc)
	if !strings.HasPrefix(raw, "Authentication-Results: smtp.example.com;") || !strings.Contains(raw, "\r\nReceived: from mail.sender.example ([198.51.100.7])\r\n\tby smtp.example.com with ESMTPS id 11112222333344445555666677778888;") {
		t.Fatalf("Thinpost headers not prepended:\n%s", raw)
	}
	head := raw[:strings.Index(raw, "\r\n\r\n")]
	if n := strings.Count(head, "Authentication-Results: smtp.example.com"); n != 1 {
		t.Fatalf("%d fields claim our authserv-id, want only Thinpost's:\n%s", n, head)
	}
	if strings.Contains(head, "smtp.mailfrom=sender.example; dmarc=pass header.from=sender.example\r\nAuthentication-Results: elsewhere") {
		t.Fatal("the forged field survived")
	}
	if !strings.Contains(head, "Authentication-Results: elsewhere.example;") {
		t.Fatal("another server's results must stay")
	}
	if !strings.Contains(head, "spf=pass") || !strings.Contains(head, "dkim=pass header.d=sender.example") || !strings.Contains(head, "dmarc=pass header.from=sender.example") {
		t.Fatalf("results missing:\n%s", head)
	}
	// A forged field in any case, with a version number and a folded value, is
	// removed.
	forged := "From: a@b.example\r\nauthentication-results: SMTP.example.com 1;\r\n\tspf=pass\r\nSubject: s\r\n\r\nbody\r\n"
	_, doc = buildInbound(t, inboundInput(forged))
	if r := rawOf(t, doc); strings.Contains(r, "spf=pass\r\nSubject") || strings.Contains(strings.ToLower(r), "smtp.example.com 1") || !strings.Contains(r, "Subject: s\r\n\r\nbody\r\n") {
		t.Fatalf("forged field survived or the message changed:\n%s", r)
	}
	// The body is never touched, even when it looks like a header.
	body := "From: a@b.example\r\n\r\nAuthentication-Results: smtp.example.com; spf=pass\r\n"
	_, doc = buildInbound(t, inboundInput(body))
	if !strings.HasSuffix(rawOf(t, doc), "\r\n\r\nAuthentication-Results: smtp.example.com; spf=pass\r\n") {
		t.Fatal("the body was modified")
	}
}

func TestInboundBodySizeOmission(t *testing.T) {
	big := strings.Repeat("a", MaxInboundBody+1)
	fits := strings.Repeat("b", MaxInboundBody-2)
	raw := "From: a@b.example\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + big + "\r\n--x\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + fits + "\r\n--x--\r\n"
	_, doc := buildInbound(t, inboundInput(raw))
	in := inboundOf(doc)
	if _, ok := in["text"]; ok {
		t.Fatal("an oversized text body must be omitted")
	}
	if omitted := in["omitted"].([]any); len(omitted) != 1 || omitted[0] != "text" {
		t.Fatal("omitted", omitted)
	}
	if html, _ := in["html"].(string); len(html) != MaxInboundBody-2 {
		t.Fatalf("html of %d bytes should be carried", len(html))
	}
	// Raw is always carried whole.
	if !strings.Contains(rawOf(t, doc), big) {
		t.Fatal("raw must carry the full message")
	}
}

func TestInboundCharsetsAreDecoded(t *testing.T) {
	cases := []struct {
		charset, encoded, want string
		omitted                bool
	}{
		{"iso-8859-1", "caf=E9", "café", false},
		{"shift_jis", "=93=FA=96=7B=8C=EA", "日本語", false},
		{"windows-1252", "=93quoted=94", "“quoted”", false},
		{"x-no-such-charset", "abc", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.charset, func(t *testing.T) {
			raw := "From: a@b.example\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=" + tc.charset + "\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" + tc.encoded + "\r\n"
			_, doc := buildInbound(t, inboundInput(raw))
			in := inboundOf(doc)
			text, _ := in["text"].(string)
			if tc.omitted {
				if text != "" || len(in["omitted"].([]any)) != 1 || in["omitted"].([]any)[0] != "text" {
					t.Fatal("an undecodable charset must be omitted", in["text"], in["omitted"])
				}
				return
			}
			if strings.TrimSpace(text) != tc.want {
				t.Fatalf("text %q, want %q", text, tc.want)
			}
		})
	}
}

func TestInboundSkipsAttachmentBodies(t *testing.T) {
	raw := "From: a@b.example\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=n.txt\r\n\r\nattached text\r\n--m\r\nContent-Type: text/plain\r\n\r\nthe body\r\n--m--\r\n"
	_, doc := buildInbound(t, inboundInput(raw))
	if got := strings.TrimSpace(inboundOf(doc)["text"].(string)); got != "the body" {
		t.Fatalf("text %q", got)
	}
}

func TestInboundUnparsableHeadersStillDeliverRaw(t *testing.T) {
	raw := strings.Repeat("X-Filler: "+strings.Repeat("a", 200)+"\r\n", 400) // no blank line, over 64 KiB
	_, doc := buildInbound(t, inboundInput(raw))
	in := inboundOf(doc)
	if o := in["omitted"].([]any); len(o) != 1 || o[0] != "headers" {
		t.Fatal("omitted", o)
	}
	if !strings.Contains(rawOf(t, doc), "X-Filler") {
		t.Fatal("raw missing")
	}
}

func TestInboundWantsIsolation(t *testing.T) {
	inboundFact := Fact{Outcome: InboundReceived}
	for _, name := range Names() {
		want := name == ThinpostInbound
		if got := Wants(name, nil, inboundFact); got != want {
			t.Errorf("%s wants inbound_received = %v, want %v", name, got, want)
		}
	}
	if !Wants(ThinpostInbound, []string{InboundReceived}, inboundFact) || Wants(ThinpostInbound, []string{"accepted"}, inboundFact) {
		t.Error("filter handling")
	}
	// An inbound endpoint never receives any outbound outcome.
	for _, outcome := range append([]string{"test", "bogus"}, Outcomes...) {
		if Wants(ThinpostInbound, nil, Fact{Outcome: outcome, SMTPCode: 250, DeliveryState: "accepted"}) {
			t.Errorf("inbound endpoint wants %s", outcome)
		}
	}
	if !ValidEvents(ThinpostInbound, []string{InboundReceived}) || ValidEvents(ThinpostInbound, []string{"accepted"}) || ValidEvents(Thinpost, []string{InboundReceived}) {
		t.Error("event names")
	}
}

func TestInboundSecretMatchesNativeShape(t *testing.T) {
	impl, _ := Lookup(ThinpostInbound)
	secret, err := impl.NewSecret()
	if err != nil || !strings.HasPrefix(secret, SecretPrefix) || len(secret) < minThinpostSecret || Unsigned(ThinpostInbound) {
		t.Fatal("secret", secret, err)
	}
}

// Hostile input never fails or panics the renderer: whatever the bytes, the
// request is built and is valid JSON.
func FuzzInboundBuild(f *testing.F) {
	for _, seed := range []string{inboundAlternative, "", "\r\n\r\n", "From: \xff\xfe\r\n\r\n", "Content-Type: multipart/mixed; boundary=\r\n\r\n--\r\n", "Authentication-Results:\r\n\t\r\n\r\nx"} {
		f.Add([]byte(seed))
	}
	impl, _ := Lookup(ThinpostInbound)
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) == 0 || len(raw) > 1<<16 {
			return
		}
		req, err := impl.Build(Input{Event: Event{ID: "evt"}, AppID: "app", Secret: thinpostSecret, Now: sentAt, Inbound: inboundInput(string(raw))})
		if err != nil || !json.Valid(req.Body) {
			t.Fatalf("render failed: %v", err)
		}
	})
}

func TestInboundBuildRejectsBadInput(t *testing.T) {
	impl, _ := Lookup(ThinpostInbound)
	ok := inboundInput(inboundAlternative)
	for name, in := range map[string]Input{
		"no message":   {Event: Event{ID: "evt"}, Secret: thinpostSecret, Now: sentAt},
		"short secret": {Event: Event{ID: "evt"}, Secret: []byte("short"), Now: sentAt, Inbound: ok},
		"no clock":     {Event: Event{ID: "evt"}, Secret: thinpostSecret, Inbound: ok},
		"bad id":       {Event: Event{ID: "a b"}, Secret: thinpostSecret, Now: sentAt, Inbound: ok},
	} {
		if _, err := impl.Build(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
