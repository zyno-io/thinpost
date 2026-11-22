package dispatch

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/prepared"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/submission"
)

const recipeDelivery = "0123456789abcdef0123456789abcdef"

// recipeFixture is a raw source and the recipe for it, built by the real header
// surgery and signer, as preparation does.
type recipeFixture struct {
	source, recipe, signed []byte
}

func newRecipeFixture(t *testing.T, id string) recipeFixture {
	t.Helper()
	raw := []byte("From: a@example.com\nTo: b@example.net\nSubject: hi\n\nline one\nline two\r\n" + strings.Repeat("attachment-ish data\n", 500))
	sourceObject, err := submission.EncodeRawSource(submission.RawHeader{
		Recipients: []string{"b@example.net"}, Kinds: []string{"to"}, SenderDomain: "example.com", EnhancedProtection: true,
	}, raw)
	if err != nil {
		t.Fatal(err)
	}
	source, err := submission.DecodeSource(sourceObject, true)
	if err != nil {
		t.Fatal(err)
	}
	scan, err := rawmime.Scan(raw)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := rawmime.DecideFrom(scan, "", rawmime.Policy{Mode: rawmime.FromOff}, func(d string) bool { return d == "example.com" })
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p, err := rawmime.Prepare(raw, rawmime.PrepareInput{Scan: scan, From: decision, MessageID: strings.Repeat("a1", 16), Date: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), Selector: "mail", Signer: key})
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := prepared.New(id, source.RawHeader.RawSHA256, scan.BodyStart, p.Message, p.HeaderLen, nil)
	if err != nil {
		t.Fatal(err)
	}
	object, err := recipe.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return recipeFixture{source: sourceObject, recipe: object, signed: p.Message}
}

func TestDispatchAssemblesARecipeToTheSignedBytes(t *testing.T) {
	fx := newRecipeFixture(t, recipeDelivery)
	if len(fx.recipe)*4 > len(fx.signed) {
		t.Fatalf("recipe %d bytes for a %d byte message", len(fx.recipe), len(fx.signed))
	}
	var sent [][]byte
	for attempt := 0; attempt < 2; attempt++ {
		p, r, o, lease := setup(t)
		o.source, o.mime = fx.source, fx.recipe
		r.claim.PreparedFormat = prepared.FormatRecipe
		r.claim.DeliveryID = recipeDelivery
		start, mime, err := p.Next(context.Background(), lease, primaryCaps())
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(fx.signed)
		if !bytes.Equal(mime, fx.signed) || start.ExpectedSize != uint64(len(fx.signed)) || !bytes.Equal(start.Sha256, digest[:]) {
			t.Fatal("the bytes sent are not the bytes signed")
		}
		sent = append(sent, mime)
	}
	if !bytes.Equal(sent[0], sent[1]) {
		t.Fatal("attempts differ")
	}
}

func TestDispatchRefusesAFinalObjectThatIsNotWhatItsFormatSays(t *testing.T) {
	fx := newRecipeFixture(t, recipeDelivery)
	other := newRecipeFixture(t, "fedcba9876543210fedcba9876543210")
	tamperedHeader := append([]byte(nil), fx.recipe...)
	tamperedHeader[bytes.Index(tamperedHeader, []byte("Subject: hi"))+9] = 'X' // header bytes: the SHA-256 no longer matches
	tamperedJSON := bytes.Replace(fx.recipe, []byte(`"body_start":`), []byte(`"body_start":1`), 1)
	httpSource := setupHTTPSource(t)
	cases := map[string]struct {
		format int
		final  []byte
		source []byte
	}{
		"tampered header":             {prepared.FormatRecipe, tamperedHeader, fx.source},
		"tampered json":               {prepared.FormatRecipe, tamperedJSON, fx.source},
		"truncated":                   {prepared.FormatRecipe, fx.recipe[:len(fx.recipe)-3], fx.source},
		"extra bytes":                 {prepared.FormatRecipe, append(append([]byte(nil), fx.recipe...), 'x'), fx.source},
		"format 2 with a full object": {prepared.FormatRecipe, fx.signed, fx.source},
		"format 1 with a recipe":      {prepared.FormatFull, fx.recipe, fx.source},
		"recipe for another delivery": {prepared.FormatRecipe, other.recipe, fx.source},
		"recipe over another source":  {prepared.FormatRecipe, other.recipe, other.source},
		"http source with format 2":   {prepared.FormatRecipe, fx.recipe, httpSource},
		"unknown format 0":            {0, fx.signed, fx.source},
		"unknown format 3":            {3, fx.signed, fx.source},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var lines []string
			t.Cleanup(oplog.SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }))
			p, r, o, lease := setup(t)
			o.source, o.mime = tc.source, tc.final
			r.claim.PreparedFormat = tc.format
			r.claim.DeliveryID = recipeDelivery
			start, mime, err := p.Next(context.Background(), lease, primaryCaps())
			if !errors.Is(err, delivery.ErrNoWork) || start != nil || mime != nil {
				t.Fatalf("work was handed out: %v", err)
			}
			if len(r.results) != 1 || r.results[0].Outcome != "rejected" || r.results[0].SMTPCode != 0 {
				t.Fatalf("not committed as a local rejection: %+v", r.results)
			}
			invalid := 0
			for _, line := range lines {
				if strings.HasPrefix(line, "prepared ") {
					invalid++
					if !strings.HasSuffix(line, "status=invalid") || strings.Contains(line, "Subject") {
						t.Fatal("unexpected line", line)
					}
				}
			}
			if invalid != 1 {
				t.Fatalf("expected one constant log line: %q", lines)
			}
		})
	}
}

func setupHTTPSource(t *testing.T) []byte {
	t.Helper()
	_, _, o, _ := setup(t)
	return o.source
}
