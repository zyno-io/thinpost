package tracking

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/zyno-io/thinpost/internal/message"
	"github.com/zyno-io/thinpost/internal/submission"
)

func TestRewriteIssuesIndependentOpaqueRecipientMappings(t *testing.T) {
	source := `<!doctype html><html><body><a href="https://shop.example/path?order=secret&amp;x=1">Order</a><a href="mailto:help@example.com">Mail</a><a href="/relative">Relative</a><a href="https://user:password@example.com/private">Credentials</a><script>const example = "https://shop.example/unchanged"</script></body></html>`
	options := Options{Links: testLinkKeys(t), Origin: "https://track.example.com", Opens: true, Clicks: true}
	out, mappings, err := Rewrite(source, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 2 || mappings[0].Kind != "click" || mappings[0].Destination != "https://shop.example/path?order=secret&x=1" || mappings[1].Kind != "open" || mappings[1].Destination != "" {
		t.Fatal("incorrect tracking mappings", mappings, err)
	}
	if len(mappings[0].Token) != 32 || len(mappings[1].Token) != 32 || mappings[0].Token == mappings[1].Token {
		t.Fatal("tracking tokens were not independent 128-bit values")
	}
	if !strings.Contains(out, "https://track.example.com/t/c/v2/") || !strings.Contains(out, "https://track.example.com/t/o/v2/") || strings.Contains(out, "order=secret") || strings.Contains(out, mappings[0].Token) || strings.Contains(out, mappings[1].Token) {
		t.Fatal("tracked links did not use opaque capabilities")
	}
	for _, m := range mappings {
		link := regexp.MustCompile(`https://track\.example\.com/t/` + m.Kind[:1] + `/v2/[A-Za-z0-9_/-]+`).FindString(out)
		hash, kind, destination, verr := options.Links.verify(options.Origin, strings.TrimPrefix(link, options.Origin))
		want, _ := HashToken(m.Token)
		if link == "" || verr != nil || hash != want || kind != m.Kind || destination != m.Destination {
			t.Fatal("link does not carry its mapping", m.Kind, verr)
		}
	}
	for _, untouched := range []string{"mailto:help@example.com", "/relative", "https://user:password@example.com/private", "https://shop.example/unchanged"} {
		if !strings.Contains(out, untouched) {
			t.Fatal("unrelated content changed", untouched)
		}
	}
	_, second, err := Rewrite(source, options)
	if err != nil || len(second) != 2 || second[0].Token == mappings[0].Token || second[1].Token == mappings[1].Token {
		t.Fatal("recipient mappings were reused", err)
	}
}

func TestRewriteFailsClosedForAmbiguousOrOversizedInput(t *testing.T) {
	options := Options{Links: testLinkKeys(t), Origin: "https://track.example.com", Clicks: true}
	for i, source := range []string{
		`<a href="https://example.com/` + strings.Repeat("x", maxTargetBytes) + `">long</a>`,
		strings.Repeat(`<a href="https://example.com">x</a>`, maxLinks+1),
		strings.Repeat("x", maxHTMLBytes+1),
	} {
		if _, mappings, err := Rewrite(source, options); err == nil || len(mappings) != 0 {
			t.Fatalf("invalid tracking input %d was accepted", i)
		}
	}
	for _, origin := range []string{"http://track.example.com", "https://user:pass@track.example.com", "https://track.example.com/path", "https://track.example.com/?secret=1", "https://track.example.com/#fragment", "https://track.example.com\nheader"} {
		if _, _, err := Rewrite("<p>hello</p>", Options{Links: testLinkKeys(t), Origin: origin, Opens: true}); err == nil {
			t.Fatal("invalid tracking origin accepted")
		}
	}
}

func TestDuplicateAttributesCannotRedirectToDiscardedValue(t *testing.T) {
	out, mappings, err := Rewrite(`<a href="https://one.example" href="https://two.example">duplicate</a>`, Options{Links: testLinkKeys(t), Origin: "https://track.example.com", Clicks: true})
	if err != nil || len(mappings) != 1 || mappings[0].Destination != "https://one.example" || strings.Contains(out, "https://two.example") {
		t.Fatal("duplicate href was not normalized safely", err)
	}
}

func TestRewriteOptOutDoesNotChangeContent(t *testing.T) {
	source := "<p>unaltered & bytes</p>"
	out, mappings, err := Rewrite(source, Options{})
	if err != nil || out != source || len(mappings) != 0 {
		t.Fatal("tracking opt-out changed the message", err)
	}
}

func TestMappingFormattingDoesNotExposeCapabilitiesOrDestinations(t *testing.T) {
	mapping := Mapping{Token: "secret-token", Kind: "click", Destination: "https://example.com/private"}
	for _, rendered := range []string{fmt.Sprintf("%v", mapping), fmt.Sprintf("%+v", mapping), fmt.Sprintf("%#v", mapping)} {
		if strings.Contains(rendered, mapping.Token) || strings.Contains(rendered, mapping.Destination) {
			t.Fatal("mapping leaked through formatting")
		}
	}
	if _, err := json.Marshal(mapping); err == nil {
		t.Fatal("mapping allowed accidental JSON serialization")
	}
}

func TestTrackingTransformationPrecedesDKIMSigning(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	html, mappings, err := Rewrite(`<a href="https://shop.example/order">Order</a>`, Options{Links: testLinkKeys(t), Origin: "https://track.example.com", Opens: true, Clicks: true})
	if err != nil || len(mappings) != 2 {
		t.Fatal("tracking transformation failed", err)
	}
	input := submission.Message{From: "sender@example.com", To: []string{"recipient@example.net"}, Subject: "Receipt", HTML: html}
	signed, err := message.Build(input, message.Options{Recipient: input.To[0], ID: "tracking-test", Date: time.Now(), SigningDomain: "example.com", Selector: "mail", Signer: key})
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	results, err := dkim.VerifyWithOptions(bytes.NewReader(signed), &dkim.VerifyOptions{LookupTXT: func(name string) ([]string, error) {
		if name != "mail._domainkey.example.com" {
			return nil, fmt.Errorf("unexpected signing lookup")
		}
		return []string{"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(public)}, nil
	}})
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatal("tracking-modified MIME did not verify", err)
	}
}
