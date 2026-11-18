package format

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSvixKnownAnswer(t *testing.T) {
	// The example in Svix's manual verification documentation.
	const secret = "whsec_plJ3nmyCDGBKInavdOK15jsl"
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		t.Fatal(err)
	}
	got := SignSvix(key, "msg_loFOjxBNrRLzqYUf", "1731705121", []byte(`{"event_type":"ping","data":{"success":true}}`))
	if want := "v1,rAvfW3dJ/X/qxhsaXPOyyCGmRKsaKWcsNccKXlIktD0="; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestResendHeadersAreSvixSigned(t *testing.T) {
	impl, _ := Lookup(Resend)
	tc := providerCases()[0]
	req, err := impl.Build(Input{Event: tc.ev, Context: tc.ctx, Secret: resendSecret, Now: sentAt})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(string(resendSecret), "whsec_"))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(req.Header["svix-id"][0] + "." + req.Header["svix-timestamp"][0] + "."))
	mac.Write(req.Body)
	if want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)); req.Header["svix-signature"][0] != want {
		t.Fatalf("signature %s want %s", req.Header["svix-signature"][0], want)
	}
	if req.Header["svix-id"][0] != tc.ev.ID || req.Header["svix-timestamp"][0] != strconv.FormatInt(sentAt.Unix(), 10) {
		t.Fatal("svix id or timestamp")
	}
}

func TestMailgunSignatureAndToken(t *testing.T) {
	impl, _ := Lookup(Mailgun)
	tc := providerCases()[0]
	build := func() (string, string, string) {
		req, err := impl.Build(Input{Event: tc.ev, Context: tc.ctx, Secret: mailgunSecret, Now: sentAt})
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Signature struct{ Timestamp, Token, Signature string }
		}
		if err = json.Unmarshal(req.Body, &body); err != nil {
			t.Fatal(err)
		}
		return body.Signature.Timestamp, body.Signature.Token, body.Signature.Signature
	}
	ts, token, sig := build()
	mac := hmac.New(sha256.New, mailgunSecret)
	mac.Write([]byte(ts + token))
	if sig != hex.EncodeToString(mac.Sum(nil)) || ts != strconv.FormatInt(sentAt.Unix(), 10) {
		t.Fatal("signature is not HMAC-SHA256(key, timestamp+token)")
	}
	if !regexp.MustCompile(`^[0-9a-f]{50}$`).MatchString(token) {
		t.Fatalf("token %q", token)
	}
	if _, token2, _ := build(); token2 == token {
		t.Fatal("token repeated across attempts")
	}
}

func TestThinpostVerifyRoundTrip(t *testing.T) {
	impl, _ := Lookup(Thinpost)
	req, err := impl.Build(Input{Event: event("accepted", 250, ""), Secret: thinpostSecret, Now: sentAt})
	if err != nil {
		t.Fatal(err)
	}
	id, ts, sig := req.Header.Get("X-Thinpost-Event-ID"), req.Header.Get("X-Thinpost-Timestamp"), req.Header.Get("X-Thinpost-Signature")
	if !VerifyThinpost(thinpostSecret, id, ts, req.Body, sig, sentAt, 5*time.Minute) {
		t.Fatal("valid signature rejected")
	}
	if VerifyThinpost(thinpostSecret, id, ts, []byte("tampered"), sig, sentAt, 5*time.Minute) {
		t.Error("accepted tampered payload")
	}
	if VerifyThinpost(thinpostSecret, id, ts, req.Body, sig, sentAt.Add(10*time.Minute), 5*time.Minute) {
		t.Error("accepted stale signature")
	}
	if VerifyThinpost([]byte("short"), id, ts, req.Body, sig, sentAt, 5*time.Minute) {
		t.Error("accepted short secret")
	}
}

func TestSecrets(t *testing.T) {
	for name, check := range map[string]func(string) bool{
		Thinpost: func(s string) bool { return strings.HasPrefix(s, "tpwh_") && len(s) == 48 },
		Resend: func(s string) bool {
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "whsec_"))
			return strings.HasPrefix(s, "whsec_") && err == nil && len(raw) == 24
		},
		Mailgun: func(s string) bool { return regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s) },
	} {
		impl, _ := Lookup(name)
		a, err := impl.NewSecret()
		b, _ := impl.NewSecret()
		if err != nil || !check(a) || a == b {
			t.Errorf("%s secret %q", name, a)
		}
		// A freshly issued secret must be usable by the same format.
		ev, ctx := TestEvent("evt_test", sentAt)
		if _, err = impl.Build(Input{Event: ev, Context: ctx, Secret: []byte(a), Now: sentAt, Test: true}); err != nil {
			t.Errorf("%s cannot use its own secret: %v", name, err)
		}
	}
	impl, _ := Lookup(Postmark)
	if s, err := impl.NewSecret(); s != "" || !errors.Is(err, ErrNoSecret) {
		t.Error("postmark must have no secret")
	}
}

func TestBuildRejectsBadInputWithoutEchoingSecrets(t *testing.T) {
	secret := "whsec_SECRET-MATERIAL-THAT-MUST-NOT-LEAK"
	for _, name := range Names() {
		impl, _ := Lookup(name)
		ev, ctx := TestEvent("evt_test", sentAt)
		for label, in := range map[string]Input{
			"short secret": {Event: ev, Context: ctx, Secret: []byte(secret[:10]), Now: sentAt},
			"bad key":      {Event: ev, Context: ctx, Secret: []byte(secret), Now: sentAt},
			"no time":      {Event: ev, Context: ctx, Secret: secretFor(name)},
			"bad id":       {Event: Event{ID: "a b", MessageID: fixedMessage, Outcome: "accepted"}, Context: ctx, Secret: secretFor(name), Now: sentAt},
		} {
			_, err := impl.Build(in)
			if name == Postmark && (label == "short secret" || label == "bad key" || label == "no time") {
				continue // unsigned: secret and clock are not inputs
			}
			if name != Resend && label == "bad key" {
				continue // only Resend decodes its secret
			}
			if err == nil {
				t.Errorf("%s/%s: accepted", name, label)
			} else if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("%s/%s: error leaks secret", name, label)
			}
		}
	}
}

// Enhanced Protection parity: even if a caller passes the subject, no provider
// encoder emits it, and no encoder emits a body field. The From address is not
// masked and is still sent.
func TestEnhancedProtectionDropsSubjectInEveryFormat(t *testing.T) {
	const secretSubject = "TOP-SECRET-SUBJECT"
	for _, name := range []string{Postmark, Resend, Mailgun} {
		impl, _ := Lookup(name)
		for _, tc := range providerCases() {
			if _, ok := tc.want[name]; !ok {
				continue
			}
			ctx := tc.ctx
			ctx.EnhancedProtection, ctx.Subject = true, secretSubject
			req, err := impl.Build(Input{Event: tc.ev, Context: ctx, Secret: secretFor(name), Now: sentAt})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(req.Body), secretSubject) {
				t.Errorf("%s/%s emitted the EP subject", name, tc.name)
			}
			var doc map[string]any
			_ = json.Unmarshal(req.Body, &doc)
			for _, key := range []string{"TextBody", "HtmlBody", "text", "html"} {
				if _, ok := doc[key]; ok {
					t.Errorf("%s/%s emitted %s", name, tc.name, key)
				}
			}
		}
	}
}

func TestBounceReceivedUnverifiedNeverRendersInProviderFormats(t *testing.T) {
	for _, name := range []string{Postmark, Resend, Mailgun} {
		for _, state := range []string{"", "failed", "queued"} {
			if Wants(name, nil, Fact{Outcome: "bounce_received_unverified", DeliveryState: state}) {
				t.Errorf("%s queues bounce_received_unverified", name)
			}
		}
	}
	if !Wants(Thinpost, nil, Fact{Outcome: "bounce_received_unverified"}) {
		t.Error("native must still receive it")
	}
}

func TestEventTypesAndFilters(t *testing.T) {
	if !ValidEvents(Thinpost, []string{"accepted", "opened"}) || ValidEvents(Thinpost, []string{"Delivery"}) {
		t.Error("native vocabulary is outcome names")
	}
	if !ValidEvents(Postmark, []string{"Delivery", "Bounce"}) || ValidEvents(Postmark, []string{"accepted"}) || ValidEvents(Postmark, []string{"Click"}) {
		t.Error("postmark vocabulary")
	}
	if !ValidEvents(Resend, []string{"email.bounced"}) || !ValidEvents(Mailgun, []string{"permanent_fail"}) {
		t.Error("resend or mailgun vocabulary")
	}
	if ValidEvents(Postmark, nil) || ValidEvents(Postmark, []string{"Delivery", "Delivery"}) || ValidEvents("nope", []string{"Delivery"}) {
		t.Error("empty, duplicate, or unknown format accepted")
	}
	hard := Fact{Outcome: "rejected", SMTPCode: 550, DeliveryState: "failed"}
	deferred := Fact{Outcome: "rejected", SMTPCode: 451, DeliveryState: "queued"}
	for _, tc := range []struct {
		format string
		filter []string
		fact   Fact
		want   bool
	}{
		{Postmark, nil, hard, true},
		{Postmark, []string{"Delivery"}, hard, false},
		{Postmark, []string{"Bounce"}, hard, true},
		{Postmark, nil, deferred, false},
		{Resend, []string{"email.delivery_delayed"}, deferred, true},
		{Resend, []string{"email.bounced"}, deferred, false},
		{Mailgun, []string{"temporary_fail"}, deferred, true},
		{Thinpost, []string{"rejected"}, deferred, true},
		{Thinpost, []string{"accepted"}, deferred, false},
		{"unknown", nil, hard, false},
	} {
		if got := Wants(tc.format, tc.filter, tc.fact); got != tc.want {
			t.Errorf("Wants(%s,%v,%+v) = %v", tc.format, tc.filter, tc.fact, got)
		}
	}
}

// Building twice from the same event yields the same stable identifiers, so
// retries and replays render the same event.
func TestStableIdentifiersAcrossBuilds(t *testing.T) {
	for _, name := range []string{Postmark, Resend} {
		impl, _ := Lookup(name)
		tc := providerCases()[0]
		a, _ := impl.Build(Input{Event: tc.ev, Context: tc.ctx, Secret: secretFor(name), Now: sentAt})
		b, _ := impl.Build(Input{Event: tc.ev, Context: tc.ctx, Secret: secretFor(name), Now: sentAt.Add(time.Minute)})
		if string(a.Body) != string(b.Body) {
			t.Errorf("%s body changed between attempts", name)
		}
	}
}

func TestDeliveryStateIsAdditiveInNativeBody(t *testing.T) {
	impl, _ := Lookup(Thinpost)
	ev := event("rejected", 550, "failed")
	req, _ := impl.Build(Input{Event: ev, Secret: thinpostSecret, Now: sentAt})
	if !strings.HasSuffix(string(req.Body), `"occurred_at":"2026-11-05T12:00:00.123456Z","delivery_state":"failed"}`) {
		t.Fatal(string(req.Body))
	}
}

// route_rejected precedes any recipient, so it is never a permanent bounce.
func TestClassifyRouteRejectedIsNeverPermanent(t *testing.T) {
	for _, code := range []int{0, 421, 450, 500, 550, 554} {
		for _, state := range []string{"queued", "failed", ""} {
			c := classify(Fact{Outcome: "route_rejected", SMTPCode: code, DeliveryState: state})
			want := classDeferred
			if state == "failed" {
				want = classSoft
			}
			if c != want {
				t.Errorf("route_rejected %d %q: class %d want %d", code, state, c, want)
			}
		}
	}
	for _, f := range []Fact{{Outcome: "rejected", SMTPCode: 550, DeliveryState: "failed"}, {Outcome: "null_mx"}, {Outcome: "domain_not_found"}} {
		if classify(f) != classPermanent {
			t.Errorf("%+v must be permanent", f)
		}
	}
}

func TestDescriptionsNeverSayCodeZero(t *testing.T) {
	for _, f := range []Fact{
		{Outcome: "rejected", DeliveryState: "queued"}, {Outcome: "route_rejected", DeliveryState: "queued"},
		{Outcome: "rejected", DeliveryState: "failed"}, {Outcome: "route_rejected", DeliveryState: "failed"},
		{Outcome: "retry_expired", DeliveryState: "failed"}, {Outcome: "null_mx"}, {Outcome: "domain_not_found"},
		{Outcome: "tracking_abandoned"}, {Outcome: "preparation_failed"}, {Outcome: "suppressed", DeliveryState: "failed"}, {Outcome: "accepted", SMTPCode: 250},
	} {
		if d := describe(classify(f), f); d == "" || strings.Contains(d, "SMTP 0") || strings.Contains(d, "(SMTP )") {
			t.Errorf("%+v: %q", f, d)
		}
	}
}

// fromOf extracts the From a provider payload carries, "" when omitted.
func fromOf(t *testing.T, name string, body []byte) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	switch name {
	case Postmark:
		s, _ := doc["From"].(string)
		return s
	case Resend:
		s, _ := doc["data"].(map[string]any)["from"].(string)
		return s
	}
	ev, _ := doc["event-data"].(map[string]any)
	msg, _ := ev["message"].(map[string]any)
	headers, _ := msg["headers"].(map[string]any)
	s, _ := headers["from"].(string)
	return s
}

func TestFromAddressPresentOrOmitted(t *testing.T) {
	const bounce, delivered = "permanent_550", "accepted"
	cases := []struct {
		name, event, sender string
		want                map[string]string
	}{
		{"named", delivered, "Sender Name", map[string]string{Postmark: "", Resend: "Sender Name <Sender@Sender.example>", Mailgun: "Sender Name <Sender@Sender.example>"}},
		{"named bounce", bounce, "Sender Name", map[string]string{Postmark: "Sender@Sender.example", Resend: "Sender Name <Sender@Sender.example>", Mailgun: "Sender Name <Sender@Sender.example>"}},
		{"bare", bounce, "", map[string]string{Postmark: "Sender@Sender.example", Resend: "Sender@Sender.example", Mailgun: "Sender@Sender.example"}},
		{"specials", delivered, `Doe, "Jane" \ Co.`, map[string]string{Resend: `"Doe, \"Jane\" \\ Co." <Sender@Sender.example>`, Mailgun: `"Doe, \"Jane\" \\ Co." <Sender@Sender.example>`}},
		{"unicode", delivered, "Zoë Müller", map[string]string{Resend: "Zoë Müller <Sender@Sender.example>"}},
	}
	for _, tc := range cases {
		for name, want := range tc.want {
			impl, _ := Lookup(name)
			ctx := fullContext()
			ctx.SenderName = tc.sender
			ev := event("accepted", 250, "accepted")
			if tc.event == bounce {
				ev = event("rejected", 550, "failed")
			}
			req, err := impl.Build(Input{Event: ev, Context: ctx, Secret: secretFor(name), Now: sentAt})
			if err != nil {
				t.Fatal(err)
			}
			if got := fromOf(t, name, req.Body); got != want {
				t.Errorf("%s/%s: From %q, want %q", name, tc.name, got, want)
			}
		}
	}
	// Not recorded (pre-schema-31): omitted in every format, never invented.
	for _, name := range []string{Postmark, Resend, Mailgun} {
		impl, _ := Lookup(name)
		ctx := fullContext()
		ctx.SenderAddress, ctx.SenderName = "", ""
		req, err := impl.Build(Input{Event: event("rejected", 550, "failed"), Context: ctx, Secret: secretFor(name), Now: sentAt})
		if err != nil {
			t.Fatal(err)
		}
		if got := fromOf(t, name, req.Body); got != "" {
			t.Errorf("%s: unrecorded From emitted as %q", name, got)
		}
	}
	// A test event carries no sender.
	for _, name := range []string{Postmark, Resend, Mailgun} {
		impl, _ := Lookup(name)
		ev, ctx := TestEvent("evt_test", sentAt)
		req, err := impl.Build(Input{Event: ev, Context: ctx, Secret: secretFor(name), Now: sentAt, Test: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := fromOf(t, name, req.Body); got != "" {
			t.Errorf("%s: test event emitted From %q", name, got)
		}
	}
}

func TestNativeSuppressedCarriesReason(t *testing.T) {
	impl, _ := Lookup(Thinpost)
	req, err := impl.Build(Input{Event: suppressed("manual"), Secret: thinpostSecret, Now: sentAt})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(req.Body), `"outcome":"suppressed"`) || !strings.Contains(string(req.Body), `"suppression_reason":"manual"`) || !strings.Contains(string(req.Body), `"delivery_state":"failed"`) {
		t.Fatalf("unexpected body %s", req.Body)
	}
	req, err = impl.Build(Input{Event: event("accepted", 250, "accepted"), Secret: thinpostSecret, Now: sentAt})
	if err != nil || strings.Contains(string(req.Body), "suppression_reason") {
		t.Fatal("suppression_reason must be omitted for other outcomes")
	}
}
