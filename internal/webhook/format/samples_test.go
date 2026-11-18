package format

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// Each file under testdata/samples is a synthetic payload written for this
// project with the same keys, nesting and JSON types as the example on the
// provider documentation page below (checked 2026-11-05). The values are
// invented; only the structure follows the provider.
var sampleSources = map[string]string{
	"postmark/delivery.json":        "https://postmarkapp.com/developer/webhooks/delivery-webhook",
	"postmark/bounce.json":          "https://postmarkapp.com/developer/webhooks/bounce-webhook",
	"postmark/open.json":            "https://postmarkapp.com/developer/webhooks/open-tracking-webhook",
	"resend/delivered.json":         "https://resend.com/docs/webhooks/emails/delivered",
	"resend/delivery_delayed.json":  "https://resend.com/docs/webhooks/emails/delivery-delayed",
	"resend/bounced.json":           "https://resend.com/docs/webhooks/emails/bounced",
	"resend/failed.json":            "https://resend.com/docs/webhooks/emails/failed",
	"resend/opened.json":            "https://resend.com/docs/webhooks/emails/opened",
	"mailgun/delivered.json":        "https://documentation.mailgun.com/docs/mailgun/user-manual/webhooks/webhook-payloads (Delivered Event)",
	"mailgun/failed_permanent.json": "https://documentation.mailgun.com/docs/mailgun/user-manual/webhooks/webhook-payloads (Permanent Failed Event)",
	"mailgun/failed_temporary.json": "https://documentation.mailgun.com/docs/mailgun/user-manual/webhooks/webhook-payloads (Temporary Failed Event)",
	"mailgun/opened.json":           "https://documentation.mailgun.com/docs/mailgun/user-manual/webhooks/webhook-payloads (Opened Event)",
}

// flatten returns every JSON path with its kind. Array elements share the
// path "x[]". The root is "".
func flatten(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		if prefix != "" {
			out[prefix] = "object"
		}
		for k, c := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flatten(p, c, out)
		}
	case []any:
		out[prefix] = "array"
		for _, c := range t {
			flatten(prefix+"[]", c, out)
		}
	case string:
		out[prefix] = "string"
	case float64:
		out[prefix] = "number"
	case bool:
		out[prefix] = "boolean"
	case nil:
		out[prefix] = "null"
	}
}

func paths(t *testing.T, raw []byte, root string) map[string]string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var v any = doc
	if root != "" {
		v = doc[root]
	}
	out := map[string]string{}
	flatten("", v, out)
	return out
}

type shapeCase struct {
	format, sample, ours string
	root                 string   // subtree of our body to compare
	omitted              []string // sample paths Thinpost deliberately never emits
	extra                []string // documented paths absent from the sample JSON
}

var (
	postmarkOmitDelivery = []string{"ServerID"}
	postmarkOmitBounce   = []string{"CanActivate", "Content", "ID", "ServerID"}
	postmarkOmitOpen     = []string{"Client", "Client.Company", "Client.Family", "Client.Name", "Geo", "Geo.City", "Geo.Coords", "Geo.Country", "Geo.CountryISOCode", "Geo.IP", "Geo.Region", "Geo.RegionISOCode", "Geo.Zip", "OS", "OS.Company", "OS.Family", "OS.Name", "Platform", "ReadSeconds", "UserAgent"}
	resendOmit           = []string{"data.broadcast_id", "data.message_id", "data.tags", "data.tags.category", "data.template_id"}
	resendOmitFailed     = []string{"data.broadcast_id", "data.tags", "data.tags.category", "data.template_id"}
	mailgunOmitDelivered = []string{"flags", "flags.is-authenticated", "flags.is-big", "flags.is-routed", "flags.is-system-test", "flags.is-test-mode", "message.attachments", "message.headers.to", "message.size", "storage", "storage.key", "storage.url", "primary-dkim", "recipient-provider", "campaigns", "account", "account.id", "delivery-status.attempt-no", "delivery-status.certificate-verified", "delivery-status.description", "delivery-status.enhanced-code", "delivery-status.first-delivery-attempt-seconds", "delivery-status.mx-host", "delivery-status.session-seconds", "delivery-status.tls", "delivery-status.utf8", "envelope", "envelope.sender", "envelope.sending-ip", "envelope.targets", "envelope.transport"}
)

func shapeCases() []shapeCase {
	return []shapeCase{
		{format: Postmark, sample: "postmark/delivery.json", ours: "accepted", omitted: postmarkOmitDelivery},
		{format: Postmark, sample: "postmark/bounce.json", ours: "permanent_550", omitted: postmarkOmitBounce},
		{format: Postmark, sample: "postmark/bounce.json", ours: "soft_final_452", omitted: postmarkOmitBounce},
		{format: Postmark, sample: "postmark/open.json", ours: "opened_first", omitted: postmarkOmitOpen},
		{format: Resend, sample: "resend/delivered.json", ours: "accepted", omitted: resendOmit},
		{format: Resend, sample: "resend/delivery_delayed.json", ours: "deferred_4xx", omitted: resendOmit},
		{format: Resend, sample: "resend/bounced.json", ours: "permanent_550", omitted: resendOmit, extra: []string{"data.bounce.diagnosticCode", "data.bounce.diagnosticCode[]"}},
		{format: Resend, sample: "resend/failed.json", ours: "tracking_abandoned", omitted: resendOmitFailed},
		{format: Resend, sample: "resend/opened.json", ours: "opened_first", omitted: resendOmit},
		{format: Mailgun, sample: "mailgun/delivered.json", ours: "accepted", root: "event-data", omitted: mailgunOmitDelivered},
		{format: Mailgun, sample: "mailgun/failed_permanent.json", ours: "permanent_550", root: "event-data", omitted: mailgunFailedPermanentOmit},
		{format: Mailgun, sample: "mailgun/failed_temporary.json", ours: "deferred_4xx", root: "event-data", omitted: mailgunFailedTemporaryOmit},
		{format: Mailgun, sample: "mailgun/opened.json", ours: "opened_first", root: "event-data", omitted: mailgunOmitOpened},
	}
}

var (
	mailgunFailedPermanentOmit = []string{"flags", "flags.is-authenticated", "flags.is-big", "flags.is-routed", "flags.is-system-test", "flags.is-test-mode", "message.attachments", "message.headers.to", "message.size", "storage", "storage.key", "storage.url", "primary-dkim", "recipient-provider", "campaigns", "account", "account.id", "delivery-status.attempt-no", "delivery-status.certificate-verified", "delivery-status.description", "delivery-status.enhanced-code", "delivery-status.first-delivery-attempt-seconds", "delivery-status.mx-host", "delivery-status.session-seconds", "delivery-status.tls", "delivery-status.utf8", "envelope", "envelope.sender", "envelope.sending-ip", "envelope.targets", "envelope.transport"}
	mailgunFailedTemporaryOmit = []string{"flags", "flags.is-authenticated", "flags.is-big", "flags.is-routed", "flags.is-system-test", "flags.is-test-mode", "message.attachments", "message.headers.to", "message.size", "storage", "storage.key", "storage.url", "primary-dkim", "recipient-provider", "campaigns", "account", "account.id", "delivery-status.attempt-no", "delivery-status.description", "delivery-status.enhanced-code", "delivery-status.first-delivery-attempt-seconds", "delivery-status.mx-host", "delivery-status.session-seconds", "delivery-status.retry-seconds", "envelope", "envelope.sender", "envelope.sending-ip", "envelope.targets", "envelope.transport"}
	mailgunOmitOpened          = []string{"account", "account.id", "campaigns", "client-info", "client-info.bot", "client-info.client-name", "client-info.client-os", "client-info.client-type", "client-info.device-type", "client-info.user-agent", "delivered-at", "geolocation", "geolocation.city", "geolocation.country", "geolocation.region", "geolocation.timezone", "ip", "recipient-provider"}
)

func TestSampleSourcesAreRecorded(t *testing.T) {
	files, _ := filepath.Glob("testdata/samples/*/*.json")
	if len(files) != len(sampleSources) {
		t.Fatalf("%d sample files, %d sources", len(files), len(sampleSources))
	}
	for _, f := range files {
		rel, _ := filepath.Rel("testdata/samples", f)
		if sampleSources[filepath.ToSlash(rel)] == "" {
			t.Errorf("%s has no recorded source", rel)
		}
	}
}

// Every key Thinpost emits must exist in the provider's documented sample with
// the same JSON kind, and every sample key Thinpost leaves out is listed, so
// omissions are reviewed rather than accidental.
func TestShapeMatchesProviderSamples(t *testing.T) {
	cases := map[string]providerCase{}
	for _, c := range providerCases() {
		cases[c.name] = c
	}
	for _, sc := range shapeCases() {
		t.Run(sc.sample+"/"+sc.ours, func(t *testing.T) {
			sample, err := os.ReadFile(filepath.Join("testdata/samples", sc.sample))
			if err != nil {
				t.Fatal(err)
			}
			tc := cases[sc.ours]
			impl, _ := Lookup(sc.format)
			req, err := impl.Build(Input{Event: tc.ev, Context: tc.ctx, Secret: secretFor(sc.format), Now: sentAt, Rand: &sequence{}})
			if err != nil {
				t.Fatal(err)
			}
			want := paths(t, sample, "")
			got := paths(t, req.Body, sc.root)
			// Free-form containers Thinpost always emits empty cover any sample content.
			for p := range want {
				for q, kind := range got {
					if len(p) > len(q) && p[:len(q)] == q && (p[len(q)] == '.' || p[len(q)] == '[') && (q == "Metadata" || q == "tags" || q == "user-variables") && kind != "" {
						delete(want, p)
					}
				}
			}
			var bad, omitted []string
			for p, kind := range got {
				if want[p] == "" && !slices.Contains(sc.extra, p) {
					bad = append(bad, p+" (not in sample)")
				} else if want[p] != "" && want[p] != kind && !(sc.format == Mailgun && p == "user-variables") {
					bad = append(bad, fmt.Sprintf("%s kind %s, sample %s", p, kind, want[p]))
				}
			}
			for p := range want {
				if got[p] == "" {
					omitted = append(omitted, p)
				}
			}
			sort.Strings(bad)
			sort.Strings(omitted)
			wantOmitted := slices.Clone(sc.omitted)
			sort.Strings(wantOmitted)
			if len(bad) > 0 {
				t.Errorf("emitted paths disagree with the sample: %v", bad)
			}
			if !slices.Equal(omitted, wantOmitted) {
				t.Errorf("omitted sample keys changed\n got: %q\nwant: %q", omitted, wantOmitted)
			}
		})
	}
}
