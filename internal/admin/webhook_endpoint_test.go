package admin

import (
	"strings"
	"testing"
)

func TestNormalizeWebhookEndpoint(t *testing.T) {
	valid := map[string]string{
		"https://Example.COM/hook":        "https://example.com/hook",
		"https://example.com":             "https://example.com",
		"https://example.com:/h":          "https://example.com/h",
		"https://example.com:443/a/b":     "https://example.com/a/b",
		"https://example.com/a%2Fb":       "https://example.com/a%2Fb",
		"https://mx.example.net/x?a=1&b=": "https://mx.example.net/x?a=1&b=",
		"https://example.com?token=Ab":    "https://example.com?token=Ab",
		"https://xn--bcher-kva.example/h": "https://xn--bcher-kva.example/h",
		"https://example.com/?a=%41%2f":   "https://example.com/?a=%41%2f",
		"https://1st.example/h":           "https://1st.example/h",
		"https://a.b123/h":                "https://a.b123/h",
		"https://example.local.example/h": "https://example.local.example/h",
	}
	for input, want := range valid {
		if got, ok := NormalizeWebhookEndpoint(input); !ok || got != want {
			t.Errorf("%q = %q, %v; want %q", input, got, ok, want)
		}
	}
	invalid := []string{
		"", "http://example.com/h", "ftp://example.com/", "//example.com/h", "example.com/h", "https:///h",
		"https://example.com:8443/h", "https://127.0.0.1/h", "https://[::1]/h", "https://[::1]:443/h",
		"https://localhost/h", "https://example/h", "https://user@example.com/h", "https://user:pw@example.com/h",
		"https://example.com/h?", "https://example.com/h#frag", "https://example.com/h#", "https://example.com/h?a=1#x",
		"https://example.com\\@evil.example/h", "https://example.com/a\\b", "https://example.com/a b", "https://example.com/a\tb",
		"https://example.com/a\nb", "https://example.com/a\x00b", "https://example.com/a\x7fb", "https://example.com/?a=\x01",
		"https://exa mple.example/", "https://bücher.example/h", "https://example.com/\xff", "https://-a.example.com/",
		"https:example.com", "mailto:a@example.com", "https://example.com/%zz",
		"https://127.1/h", "https://10.1/h", "https://192.168.1/h", "https://0x7f.0.0.1/h", "https://a.0x1F/h", "https://a.0x/h",
		"https://example.com/?q=ü", "https://example.com/?q=\u0085", "https://example.com/?q=\u202e", "https://example.com/\u200b",
		"https://example.com/?a=%zz", "https://example.com/?a=%4", "https://example.com/?a=%",
		"https://kubernetes.default.svc.cluster.local/", "https://thinpost.thinpost.svc/", "https://x.localhost/",
		"https://metadata.google.internal/", "https://router.home.arpa/",
		"https://example.com/" + strings.Repeat("a", 2048),
		"https://example.com/?" + strings.Repeat("a", 2048),
	}
	for _, input := range invalid {
		if got, ok := NormalizeWebhookEndpoint(input); ok {
			t.Errorf("%q accepted as %q", input, got)
		}
	}
	atLimit := "https://example.com/" + strings.Repeat("a", 2048-len("https://example.com/"))
	if _, ok := NormalizeWebhookEndpoint(atLimit); !ok {
		t.Error("2048-byte endpoint rejected")
	}
}

func TestValidWebhookEventNames(t *testing.T) {
	if len(WebhookOutcomes) != 21 || !ValidWebhookEventNames(WebhookOutcomes) || !ValidWebhookEventNames([]string{"opened", "complained", "Delivery", "SpamComplaint", "email.complained", "email.bounced", "permanent_fail"}) {
		t.Fatal("known event names rejected")
	}
	for _, bad := range [][]string{nil, {}, {"bogus"}, {"opened", "opened"}, {"Opened"}, {""}, {"opened", "bogus"}, make([]string, 33)} {
		if ValidWebhookEventNames(bad) {
			t.Errorf("%v accepted", bad)
		}
	}
}
