package message

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/zyno-io/thinpost/internal/submission"
)

func recipientOptions(t *testing.T, recipient string, list bool) (Options, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return Options{Recipient: recipient, ID: "unique-123", Date: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), SigningDomain: "example.com", Selector: "mail", Signer: key, ListRecipients: list}, key
}

func TestVersion2CopiesListToAndCcAndNeverBcc(t *testing.T) {
	input := submission.Message{From: "alice@example.com", To: []string{"Bob <bob@example.net>", "eve@example.net"}, Cc: []string{"Carol <carol@example.net>"}, Bcc: []string{"dave@example.net"}, Subject: "Hello", Text: "body"}
	for _, recipient := range input.Envelope() {
		options, key := recipientOptions(t, recipient, true)
		body, err := Build(input, options)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := mail.ReadMessage(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		to, err := parsed.Header.AddressList("To")
		if err != nil || len(to) != 2 || to[0].Address != "bob@example.net" || to[1].Address != "eve@example.net" {
			t.Fatal("To list", recipient, err)
		}
		cc, err := parsed.Header.AddressList("Cc")
		if err != nil || len(cc) != 1 || cc[0].Address != "carol@example.net" || cc[0].Name != "Carol" {
			t.Fatal("Cc list", recipient, err)
		}
		if _, ok := parsed.Header["Bcc"]; ok || bytes.Contains(bytes.ToLower(body), []byte("bcc:")) || bytes.Contains(body, []byte("dave@example.net")) {
			t.Fatal("Bcc recipient leaked into the copy for", recipient)
		}
		// DKIM signs every header, so the Cc list is covered.
		if !dkimResult(t, body, key) || dkimResult(t, bytes.Replace(body, []byte("carol@example.net"), []byte("evil@example.net"), 1), key) {
			t.Fatal("DKIM must verify and cover the Cc header for", recipient)
		}
	}
}

// dkimResult verifies the signature on body against key's public half.
func dkimResult(t *testing.T, body []byte, key *rsa.PrivateKey) bool {
	t.Helper()
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	results, err := dkim.VerifyWithOptions(bytes.NewReader(body), &dkim.VerifyOptions{LookupTXT: func(string) ([]string, error) {
		return []string{"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
	}})
	return err == nil && len(results) == 1 && results[0].Err == nil
}

func TestVersion2WithoutCcHasNoCcHeader(t *testing.T) {
	input := submission.Message{From: "alice@example.com", To: []string{"bob@example.net", "eve@example.net"}, Subject: "Hello", Text: "body"}
	options, _ := recipientOptions(t, "eve@example.net", true)
	body, err := Build(input, options)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := mail.ReadMessage(bytes.NewReader(body))
	if to, err := parsed.Header.AddressList("To"); err != nil || len(to) != 2 {
		t.Fatal("To list", err)
	}
	if _, ok := parsed.Header["Cc"]; ok {
		t.Fatal("unexpected Cc header")
	}
}

func TestVersion1KeepsSingleRecipientToAndRefusesCcAndBcc(t *testing.T) {
	input := submission.Message{From: "alice@example.com", To: []string{"bob@example.net", "eve@example.net"}, Subject: "Hello", Text: "body"}
	options, _ := recipientOptions(t, "bob@example.net", false)
	body, err := Build(input, options)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("To: <bob@example.net>\r\n")) || bytes.Contains(body, []byte("eve@example.net")) {
		t.Fatal("version 1 copy must show only its own recipient")
	}
	input.Cc = []string{"carol@example.net"}
	if _, err = Build(input, options); err == nil {
		t.Fatal("Cc on a version 1 build must be refused")
	}
	input.Cc, input.Bcc = nil, []string{"carol@example.net"}
	if _, err = Build(input, options); err == nil {
		t.Fatal("Bcc on a version 1 build must be refused")
	}
}

func TestBuildRejectsRecipientOutsideEnvelope(t *testing.T) {
	input := submission.Message{From: "alice@example.com", To: []string{"bob@example.net"}, Cc: []string{"carol@example.net"}, Subject: "Hello", Text: "body"}
	options, _ := recipientOptions(t, "other@example.net", true)
	if _, err := Build(input, options); err == nil {
		t.Fatal("recipient outside To, Cc and Bcc accepted")
	}
}

func TestRecipientHeadersFoldWithinLimits(t *testing.T) {
	var to, cc []string
	for i := 0; i < 40; i++ {
		to = append(to, fmt.Sprintf("Recipient Number %d <recipient%d@example.net>", i, i))
		cc = append(cc, fmt.Sprintf("copy%d@example.org", i))
	}
	input := submission.Message{From: "alice@example.com", To: to, Cc: cc, Subject: "Hello", Text: "body"}
	options, key := recipientOptions(t, to[7], true)
	body, err := Build(input, options)
	if err != nil {
		t.Fatal(err)
	}
	header, _, _ := strings.Cut(string(body), "\r\n\r\n")
	for _, line := range strings.Split(header, "\r\n") {
		if len(line) > 998 {
			t.Fatal("line over 998 bytes")
		}
		if strings.HasPrefix(line, " recipient") || strings.HasPrefix(line, " Recipient") {
			if len(line) > 78 {
				t.Fatalf("folded address line is %d bytes", len(line))
			}
		}
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	gotTo, err := parsed.Header.AddressList("To")
	gotCc, errCc := parsed.Header.AddressList("Cc")
	if err != nil || errCc != nil || len(gotTo) != 40 || len(gotCc) != 40 || gotTo[39].Address != "recipient39@example.net" {
		t.Fatal("folded lists did not parse back", err, errCc)
	}
	if !dkimResult(t, body, key) {
		t.Fatal("DKIM does not verify folded recipient headers")
	}
}

func TestFoldAddressesPrefersShortLinesAndNeverBreaksAnAddress(t *testing.T) {
	got := foldAddresses("To", []string{"a@example.net", "b@example.net", "c@example.net", "d@example.net", "e@example.net", "f@example.net", "g@example.net"})
	want := "To: a@example.net, b@example.net, c@example.net, d@example.net, e@example.net,\r\n f@example.net, g@example.net\r\n"
	if got != want {
		t.Fatalf("%q", got)
	}
	long := "\"" + strings.Repeat("x", 120) + "\" <long@example.net>"
	got = foldAddresses("Cc", []string{long, "b@example.net"})
	if got != "Cc: "+long+",\r\n b@example.net\r\n" {
		t.Fatalf("%q", got)
	}
}

func TestRecipientHeaderLimits(t *testing.T) {
	// One rendered address that cannot fit a header line.
	long := fmt.Sprintf("%s@example.net", strings.Repeat("x", 64))
	input := submission.Message{From: "alice@example.com", To: []string{"Name <" + long + ">"}, Subject: "Hello", Text: "body"}
	options, _ := recipientOptions(t, input.To[0], true)
	if _, err := Build(input, options); err != nil {
		t.Fatal("ordinary long address rejected", err)
	}
	input.To = []string{strings.Repeat("n", 1000) + " <a@example.net>"}
	// A display name that long does not validate as a header line.
	options.Recipient = input.To[0]
	if _, err := Build(input, options); err == nil {
		t.Fatal("address over the per-line limit accepted")
	}
	// Folded To and Cc together are capped at 16 KiB.
	var to []string
	for i := 0; i < 100; i++ {
		to = append(to, fmt.Sprintf("\"%s\" <u%d@example.net>", strings.Repeat("n", 190), i))
	}
	input = submission.Message{From: "alice@example.com", To: to, Subject: "Hello", Text: "body"}
	options.Recipient = to[0]
	if _, err := Build(input, options); err == nil {
		t.Fatal("recipient headers over 16 KiB accepted")
	}
	input.To = to[:50]
	if _, err := Build(input, options); err != nil {
		t.Fatal("recipient headers under 16 KiB rejected", err)
	}
}
