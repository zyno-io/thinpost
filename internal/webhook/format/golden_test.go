package format

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite provider golden files (never the native ones captured before the refactor)")

const (
	fixedMessage = "0123456789abcdef0123456789abcdef"
	fixedSubject = "Your receipt"
)

var (
	occurredAt = time.Date(2026, 11, 5, 12, 0, 0, 123456000, time.UTC)
	sentAt     = time.Date(2026, 11, 5, 12, 0, 7, 0, time.UTC)
	acceptedAt = time.Date(2026, 11, 5, 11, 59, 58, 500000000, time.UTC)

	thinpostSecret = []byte("0123456789abcdef0123456789abcdef")
	resendSecret   = []byte("whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYX") // key bytes 0..23
	mailgunSecret  = []byte("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
)

// sequence is a deterministic Rand: 1, 2, 3, ... modulo 256.
type sequence struct{ n byte }

func (s *sequence) Read(p []byte) (int, error) {
	for i := range p {
		s.n++
		p[i] = s.n
	}
	return len(p), nil
}

func secretFor(name string) []byte {
	switch name {
	case Resend:
		return resendSecret
	case Mailgun:
		return mailgunSecret
	case Thinpost:
		return thinpostSecret
	}
	return nil
}

func fullContext() Context {
	first := true
	return Context{AcceptedAt: acceptedAt, Subject: fixedSubject, SenderDomain: "sender.example", SenderAddress: "Sender@Sender.example", SenderName: "Sender Name", Recipients: []string{"Recipient@Example.NET"}, FirstOpen: &first}
}

func event(outcome string, code int, state string) Event {
	return Event{ID: "evt_" + outcome, MessageID: fixedMessage, DeliveryID: "delivery-1", AttemptID: "attempt-1", Outcome: outcome, SMTPCode: code, OccurredAt: occurredAt, DeliveryState: state}
}

type providerCase struct {
	name string
	ev   Event
	ctx  Context
	want map[string]string // format -> event type; absent means not representable
}

func suppressed(reason string) Event {
	ev := event("suppressed", 0, "failed")
	ev.SuppressionReason = reason
	return ev
}

func providerCases() []providerCase {
	ep := fullContext()
	ep.EnhancedProtection = true
	notFirst := fullContext()
	f := false
	notFirst.FirstOpen = &f
	multi := fullContext()
	multi.Recipients = []string{"a@example.net", "b@example.net"}
	noFirstInfo := fullContext()
	noFirstInfo.FirstOpen = nil
	special := fullContext()
	special.SenderName = `Doe, "Jane" \ Co.`
	bare := fullContext()
	bare.SenderName = ""
	pm, rs, mg := Postmark, Resend, Mailgun
	all := func(p, r, m string) map[string]string {
		out := map[string]string{}
		for k, v := range map[string]string{pm: p, rs: r, mg: m} {
			if v != "" {
				out[k] = v
			}
		}
		return out
	}
	none := all("", "", "")
	return []providerCase{
		{"accepted", event("accepted", 250, "accepted"), fullContext(), all("Delivery", "email.delivered", "delivered")},
		{"accepted_ep", event("accepted", 250, "accepted"), ep, all("Delivery", "email.delivered", "delivered")},
		{"accepted_special_name", event("accepted", 250, "accepted"), special, all("Delivery", "email.delivered", "delivered")},
		{"permanent_special_name", event("rejected", 550, "failed"), special, all("Bounce", "email.bounced", "permanent_fail")},
		{"accepted_bare_from", event("accepted", 250, "accepted"), bare, all("Delivery", "email.delivered", "delivered")},
		{"permanent_bare_from", event("rejected", 550, "failed"), bare, all("Bounce", "email.bounced", "permanent_fail")},
		{"accepted_pre028", event("accepted", 250, ""), Context{}, all("Delivery", "email.delivered", "delivered")},
		{"deferred_4xx", event("rejected", 451, "queued"), fullContext(), all("", "email.delivery_delayed", "temporary_fail")},
		{"deferred_route", event("route_rejected", 0, "queued"), fullContext(), all("", "email.delivery_delayed", "temporary_fail")},
		{"permanent_550", event("rejected", 550, "failed"), fullContext(), all("Bounce", "email.bounced", "permanent_fail")},
		{"permanent_550_ep", event("rejected", 550, "failed"), ep, all("Bounce", "email.bounced", "permanent_fail")},
		{"permanent_pre030_550", event("rejected", 550, ""), fullContext(), all("Bounce", "email.bounced", "permanent_fail")},
		{"permanent_null_mx", event("null_mx", 0, "failed"), fullContext(), all("Bounce", "email.bounced", "permanent_fail")},
		{"permanent_domain_not_found", event("domain_not_found", 0, "failed"), fullContext(), all("Bounce", "email.bounced", "permanent_fail")},
		{"soft_final_452", event("rejected", 452, "failed"), fullContext(), all("Bounce", "email.bounced", "permanent_fail")},
		{"soft_route_rejected_554", event("route_rejected", 554, "failed"), fullContext(), all("Bounce", "email.bounced", "permanent_fail")},
		{"soft_final_no_reply", event("rejected", 0, "failed"), fullContext(), all("Bounce", "email.bounced", "permanent_fail")},
		{"soft_retry_expired", event("retry_expired", 0, "failed"), fullContext(), all("Bounce", "email.bounced", "permanent_fail")},
		{"suppressed_manual", suppressed("manual"), fullContext(), all("", "email.suppressed", "permanent_fail")},
		{"suppressed_hard_bounce", suppressed("hard_bounce"), fullContext(), all("", "email.suppressed", "permanent_fail")},
		{"suppressed_unsubscribe", suppressed("unsubscribe"), fullContext(), all("", "email.suppressed", "permanent_fail")},
		{"suppressed_spam_complaint", suppressed("spam_complaint"), fullContext(), all("", "email.suppressed", "permanent_fail")},
		{"tracking_abandoned", event("tracking_abandoned", 0, "failed"), fullContext(), all("", "email.failed", "permanent_fail")},
		{"preparation_failed", event("preparation_failed", 0, ""), multi, all("", "email.failed", "")},
		{"hold_expired", event("hold_expired", 0, ""), multi, all("", "email.failed", "")},
		{"held", event("held", 0, ""), multi, none},
		{"released", event("released", 0, ""), multi, none},
		{"cancelled", event("cancelled", 0, ""), multi, none},
		{"opened_first", event("opened", 0, ""), fullContext(), all("Open", "email.opened", "opened")},
		{"opened_repeat", event("opened", 0, ""), notFirst, all("Open", "email.opened", "opened")},
		{"opened_unknown_first", event("opened", 0, ""), noFirstInfo, all("Open", "email.opened", "opened")},
		{"unsubscribed", event("unsubscribed", 0, ""), fullContext(), all("SubscriptionChange", "", "unsubscribed")},
		{"unsubscribed_ep", event("unsubscribed", 0, ""), ep, all("SubscriptionChange", "", "unsubscribed")},
		{"complained", event("complained", 0, ""), fullContext(), all("SpamComplaint", "email.complained", "complained")},
		{"complained_ep", event("complained", 0, ""), ep, all("SpamComplaint", "email.complained", "complained")},
		{"uncertain", event("uncertain", 0, "uncertain"), fullContext(), none},
		{"recipient_policy_denied", event("recipient_policy_denied", 0, "recipient_policy_held"), fullContext(), none},
		{"tracking_unavailable", event("tracking_unavailable", 0, "tracking_held"), fullContext(), none},
		{"bounce_received_unverified", event("bounce_received_unverified", 0, ""), fullContext(), none},
		{"clicked", event("clicked", 0, ""), fullContext(), none},
	}
}

func renderHeaders(h http.Header) string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(h[k], ", "))
	}
	return b.String()
}

func compareGolden(t *testing.T, dir, name string, req Request, allowUpdate bool) {
	t.Helper()
	files := map[string][]byte{name + ".json": req.Body, name + ".headers": []byte(renderHeaders(req.Header))}
	for file, got := range files {
		path := filepath.Join("testdata", "golden", dir, file)
		if *update && allowUpdate {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (run with -update to create)", path, err)
		}
		if !bytes.Equal(want, got) {
			t.Errorf("%s differs\nwant: %s\ngot:  %s", path, want, got)
		}
	}
}

func TestProviderGolden(t *testing.T) {
	for _, name := range []string{Postmark, Resend, Mailgun} {
		impl, _ := Lookup(name)
		for _, tc := range providerCases() {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				in := Input{Event: tc.ev, Context: tc.ctx, AppID: "app", Secret: secretFor(name), Now: sentAt, Rand: &sequence{}}
				want, representable := tc.want[name]
				got, ok := impl.Classify(Fact{tc.ev.Outcome, tc.ev.SMTPCode, tc.ev.DeliveryState})
				if ok != representable || got != want {
					t.Fatalf("Classify = %q %v, want %q %v", got, ok, want, representable)
				}
				req, err := impl.Build(in)
				if !representable {
					if err == nil {
						t.Fatal("Build rendered an event the format cannot represent honestly")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				compareGolden(t, name, tc.name, req, true)
			})
		}
	}
}

func TestProviderTestEvent(t *testing.T) {
	ev, ctx := TestEvent("evt_test", sentAt)
	for _, name := range []string{Postmark, Resend, Mailgun} {
		t.Run(name, func(t *testing.T) {
			impl, _ := Lookup(name)
			req, err := impl.Build(Input{Event: ev, Context: ctx, Secret: secretFor(name), Now: sentAt, Test: true, Rand: &sequence{}})
			if err != nil {
				t.Fatal(err)
			}
			if req.Header.Get("X-Thinpost-Test") != "true" {
				t.Error("missing test header")
			}
			compareGolden(t, name, "test", req, true)
		})
	}
}

// The native body gains message_id_header only when the event carries it; every
// golden above is byte-identical without it.
func TestThinpostMessageIDHeader(t *testing.T) {
	impl, _ := Lookup(Thinpost)
	ev := Event{ID: "evt_accepted", MessageID: fixedMessage, DeliveryID: "delivery-1", AttemptID: "attempt-1", Outcome: "accepted", SMTPCode: 250, OccurredAt: occurredAt, MessageIDHeader: "<" + fixedMessage + "@example.com>"}
	req, err := impl.Build(Input{Event: ev, Secret: thinpostSecret, Now: sentAt})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"evt_accepted","message_id":"0123456789abcdef0123456789abcdef","delivery_id":"delivery-1","attempt_id":"attempt-1","outcome":"accepted","smtp_code":250,"occurred_at":"2026-11-05T12:00:00.123456Z","message_id_header":"\u003c0123456789abcdef0123456789abcdef@example.com\u003e"}`
	if string(req.Body) != want {
		t.Fatalf("body %q", req.Body)
	}
	ev.MessageIDHeader = ""
	req, err = impl.Build(Input{Event: ev, Secret: thinpostSecret, Now: sentAt})
	if err != nil || bytes.Contains(req.Body, []byte("message_id_header")) {
		t.Fatalf("field not omitted when empty: %s %v", req.Body, err)
	}
}

// The native goldens were captured from the sender and dispatcher before this
// package existed; they are never regenerated, so a diff means native
// endpoints changed.
func TestThinpostGoldenIsByteIdentical(t *testing.T) {
	impl, _ := Lookup(Thinpost)
	events := map[string]Event{
		"accepted":                   {ID: "evt_accepted", MessageID: fixedMessage, DeliveryID: "delivery-1", AttemptID: "attempt-1", Outcome: "accepted", SMTPCode: 250, OccurredAt: occurredAt},
		"rejected":                   {ID: "evt_rejected", MessageID: fixedMessage, DeliveryID: "delivery-1", AttemptID: "attempt-2", Outcome: "rejected", SMTPCode: 550, OccurredAt: occurredAt},
		"preparation_failed":         {ID: "evt_prep", MessageID: fixedMessage, Outcome: "preparation_failed", OccurredAt: occurredAt},
		"bounce_received_unverified": {ID: "evt_bounce", MessageID: fixedMessage, DeliveryID: "delivery-1", BounceReceiptID: "receipt-1", Outcome: "bounce_received_unverified", OccurredAt: occurredAt},
		"opened":                     {ID: "evt_open", MessageID: fixedMessage, DeliveryID: "delivery-1", TrackingLinkID: "link-1", Outcome: "opened", OccurredAt: occurredAt},
		"unsubscribed":               {ID: "evt_unsub", MessageID: fixedMessage, DeliveryID: "delivery-1", Outcome: "unsubscribed", OccurredAt: occurredAt},
		"complained":                 {ID: "evt_complained", MessageID: fixedMessage, DeliveryID: "delivery-1", Outcome: "complained", OccurredAt: occurredAt},
		"html_chars":                 {ID: "evt_html", MessageID: "m<&>", Outcome: "accepted", SMTPCode: 250, OccurredAt: occurredAt},
	}
	for name, ev := range events {
		t.Run(name, func(t *testing.T) {
			req, err := impl.Build(Input{Event: ev, Secret: thinpostSecret, Now: sentAt})
			if err != nil {
				t.Fatal(err)
			}
			compareGolden(t, Thinpost, name, req, false)
		})
	}
	t.Run("test", func(t *testing.T) {
		req, err := impl.Build(Input{Event: Event{ID: "evt_test", OccurredAt: sentAt}, Secret: thinpostSecret, Now: sentAt, Test: true})
		if err != nil {
			t.Fatal(err)
		}
		compareGolden(t, Thinpost, "test", req, false)
	})
}
