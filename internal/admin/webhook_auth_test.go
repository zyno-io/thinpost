package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestWebhookAuthIsRedactedEverywhere(t *testing.T) {
	a := NewWebhookAuth("user-secret", "pass-secret", []WebhookHeader{{Name: "X-Token", Value: "value-secret"}})
	h := a.Headers()[0]
	var out []string
	for _, v := range []any{a, &a, h, &h, []WebhookHeader{h}, map[string]any{"auth": a}} {
		out = append(out, fmt.Sprint(v), fmt.Sprintf("%v %+v %#v %s %q %d", v, v, v, v, v, v))
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(raw))
	}
	out = append(out, a.String(), a.GoString(), h.String(), h.GoString())
	for _, s := range out {
		for _, secret := range []string{"user-secret", "pass-secret", "value-secret"} {
			if strings.Contains(s, secret) {
				t.Fatalf("%q leaked %s", s, secret)
			}
		}
	}
}

func TestWebhookAuthSealRoundTrip(t *testing.T) {
	a := NewWebhookAuth("u", "p", []WebhookHeader{{Name: "X-Token", Value: "v"}})
	raw, err := a.Seal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenWebhookAuth(raw)
	if err != nil || got.Username() != "u" || got.Password() != "p" || len(got.Headers()) != 1 || got.Headers()[0].Value != "v" {
		t.Fatal("round trip", err)
	}
	if _, err = OpenWebhookAuth([]byte(`{"username":"secret-without-password"}`)); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("invalid sealed auth accepted or error echoes input", err)
	}
	if _, err = OpenWebhookAuth([]byte(`not json`)); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestValidWebhookAuth(t *testing.T) {
	hdr := func(n, v string) WebhookHeader { return WebhookHeader{n, v} }
	many := []WebhookHeader{}
	for i := 0; i < 6; i++ {
		many = append(many, hdr(fmt.Sprintf("X-H%d", i), "v"))
	}
	for _, tc := range []struct {
		name string
		auth WebhookAuth
		ok   bool
	}{
		{"empty", WebhookAuth{}, true},
		{"basic", NewWebhookAuth("user", "pass", nil), true},
		{"headers", NewWebhookAuth("", "", []WebhookHeader{hdr("X-Token", "abc 123")}), true},
		{"authorization without basic", NewWebhookAuth("", "", []WebhookHeader{hdr("Authorization", "Bearer x")}), true},
		{"underscore in name", NewWebhookAuth("", "", []WebhookHeader{hdr("X_Token", "v")}), false},
		{"authorization with basic", NewWebhookAuth("u", "p", []WebhookHeader{hdr("authorization", "Bearer x")}), false},
		{"username only", NewWebhookAuth("u", "", nil), false},
		{"password only", NewWebhookAuth("", "p", nil), false},
		{"colon in username", NewWebhookAuth("a:b", "p", nil), false},
		{"control in password", NewWebhookAuth("u", "p\r\nX: y", nil), false},
		{"long username", NewWebhookAuth(strings.Repeat("u", 257), "p", nil), false},
		{"long password", NewWebhookAuth("u", strings.Repeat("p", 257), nil), false},
		{"six headers", NewWebhookAuth("", "", many), false},
		{"five headers", NewWebhookAuth("", "", many[:5]), true},
		{"duplicate after canonicalization", NewWebhookAuth("", "", []WebhookHeader{hdr("x-token", "a"), hdr("X-TOKEN", "b")}), false},
		{"crlf in value", NewWebhookAuth("", "", []WebhookHeader{hdr("X-Token", "a\r\nInjected: 1")}), false},
		{"non-ascii value", NewWebhookAuth("", "", []WebhookHeader{hdr("X-Token", "é")}), false},
		{"empty value", NewWebhookAuth("", "", []WebhookHeader{hdr("X-Token", "")}), false},
		{"long value", NewWebhookAuth("", "", []WebhookHeader{hdr("X-Token", strings.Repeat("v", 1025))}), false},
		{"bad name", NewWebhookAuth("", "", []WebhookHeader{hdr("X Token", "v")}), false},
		{"long name", NewWebhookAuth("", "", []WebhookHeader{hdr(strings.Repeat("X", 65), "v")}), false},
		{"host", NewWebhookAuth("", "", []WebhookHeader{hdr("Host", "evil.example")}), false},
		{"content-type", NewWebhookAuth("", "", []WebhookHeader{hdr("content-type", "text/plain")}), false},
		{"transfer-encoding", NewWebhookAuth("", "", []WebhookHeader{hdr("Transfer-Encoding", "chunked")}), false},
		{"te", NewWebhookAuth("", "", []WebhookHeader{hdr("TE", "trailers")}), false},
		{"proxy prefix", NewWebhookAuth("", "", []WebhookHeader{hdr("Proxy-Authorization", "x")}), false},
		{"thinpost prefix", NewWebhookAuth("", "", []WebhookHeader{hdr("X-Thinpost-Signature", "x")}), false},
		{"svix prefix", NewWebhookAuth("", "", []WebhookHeader{hdr("svix-id", "x")}), false},
		{"webhook prefix", NewWebhookAuth("", "", []WebhookHeader{hdr("Webhook-Signature", "x")}), false},
		{"postmark prefix", NewWebhookAuth("", "", []WebhookHeader{hdr("X-PM-Webhook-Trace-Id", "x")}), false},
		{"twilio prefix", NewWebhookAuth("", "", []WebhookHeader{hdr("X-Twilio-Email-Event-Webhook-Signature", "x")}), false},
	} {
		if got := ValidWebhookAuth(tc.auth); got != tc.ok {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.ok)
		}
	}
}

func TestWebhookAuthSecretsSurviveReflectionPrinters(t *testing.T) {
	a := NewWebhookAuth("user-secret", "pass-secret", []WebhookHeader{{Name: "X-Token", Value: "value-secret"}})
	holder := struct{ inner WebhookAuth }{a}
	outputs := []string{fmt.Sprintf("%v", holder), fmt.Sprintf("%+v", holder), fmt.Sprintf("%#v", holder)}
	for _, handler := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var buf bytes.Buffer
		logger := slog.New(handler(&buf))
		logger.Info("x", "holder", holder, "auth", a, "ptr", &a, "headers", a.Headers())
		outputs = append(outputs, buf.String())
	}
	for _, out := range outputs {
		for _, secret := range []string{"user-secret", "pass-secret", "value-secret"} {
			if strings.Contains(out, secret) {
				t.Fatalf("%q leaked %s", out, secret)
			}
		}
	}
}

func TestSealDoesNotHTMLEscape(t *testing.T) {
	value := strings.Repeat("&<>", 341)
	auth := NewWebhookAuth("", "", []WebhookHeader{{Name: "X-Token", Value: value}})
	plain, err := auth.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plain), value) || len(plain) > len(value)+128 {
		t.Fatalf("sealed plaintext is escaped or inflated: %d bytes", len(plain))
	}
}
