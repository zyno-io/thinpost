package preparation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/prepared"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/tracking"
)

func TestRawLimitsMatchRawmime(t *testing.T) {
	if submission.MaxRawBytes != rawmime.MaxRawBytes {
		t.Fatal("submission.MaxRawBytes drifted from rawmime.MaxRawBytes")
	}
}

const rawMessage = "From: Alice <a@example.com>\r\nTo: b@example.net\r\nCc: c@example.net\r\nSubject: hello\r\nMessage-ID: <client@example.org>\r\nList-Unsubscribe: <mailto:u@example.com>\r\n\r\nraw body\r\n"

// rawSource stores a v3 source for rawMessage, deciding From with rawmime the
// way acceptance will.
func rawSource(t *testing.T, objects *fakeObjects, raw string, mutate func(*submission.RawHeader)) {
	t.Helper()
	s, err := rawmime.Scan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	d, err := rawmime.DecideFrom(s, "", rawmime.Policy{Mode: rawmime.FromOff}, func(d string) bool { return d == "example.com" })
	if err != nil {
		t.Fatal(err)
	}
	rcpts, kinds, err := rawmime.Kinds([]string{"b@example.net", "c@example.net", "bcc@example.org"}, s)
	if err != nil {
		t.Fatal(err)
	}
	ks := make([]string, len(kinds))
	for i, k := range kinds {
		ks[i] = string(k)
	}
	h := submission.RawHeader{
		MailFrom: "a@example.com", Recipients: rcpts, Kinds: ks, From: "", FromField: d.Field, FromRewritten: d.Rewritten,
		StripSender: d.StripSender, SenderDomain: d.Domain, EnhancedProtection: true,
	}
	if d.Field != "" {
		h.From = d.Value
	}
	if mutate != nil {
		mutate(&h)
	}
	objects.source, err = submission.EncodeRawSource(h, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
}

func TestRawPreparationStoresARecipePerRecipient(t *testing.T) {
	service, repo, objects, _ := setup(t)
	rawSource(t, objects, rawMessage, nil)
	// Tracking on the app side must not touch a raw message, and with one-click
	// off nothing is added or removed, so every recipient's message is identical.
	service.Tracking = &TrackingConfig{Origin: "https://tracking.example.com", Links: preparationLinkKeys(t), EventLimit: 10, Objects: &fakeTrackingObjects{}}
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.commits != 1 || len(repo.deliveries) != 3 || len(objects.final) != 3 {
		t.Fatalf("%d commits, %d deliveries", repo.commits, len(repo.deliveries))
	}
	source, err := submission.DecodeSource(objects.source, true)
	if err != nil {
		t.Fatal(err)
	}
	var first, firstHeader []byte
	for i, d := range repo.deliveries {
		if d.RecipientIndex != i || d.Tracking != nil || d.Format != prepared.FormatRecipe {
			t.Fatal("index, tracking or format", i, d.Format)
		}
		object := objects.final[d.ObjectKey]
		if !prepared.IsRecipe(object) {
			t.Fatal("not a recipe object")
		}
		recipe, err := prepared.Decode(object)
		if err != nil || recipe.DeliveryID() != d.ID || recipe.BaseSHA256() != source.RawHeader.RawSHA256 {
			t.Fatalf("recipe: %v", err)
		}
		data, err := recipe.Assemble(source.Raw, source.RawHeader.RawSHA256)
		if err != nil || !bytes.HasPrefix(data, recipe.Header()) || !bytes.HasSuffix(data, []byte("\r\n\r\nraw body\r\n")) {
			t.Fatalf("assembled message: %v", err)
		}
		if first == nil {
			first, firstHeader = data, recipe.Header()
		} else if !bytes.Equal(first, data) || !bytes.Equal(firstHeader, recipe.Header()) {
			t.Fatal("per-recipient messages differ")
		}
	}
	if repo.deliveries[0].ObjectKey == repo.deliveries[1].ObjectKey || repo.deliveries[0].ID == repo.deliveries[1].ID {
		t.Fatal("deliveries must have distinct ids and keys")
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := parsed.Header.Get("Message-ID"), "<"+repo.claim.MessageID+"@example.com>"; got != want {
		t.Fatalf("Message-ID %q want %q", got, want)
	}
	// With one-click off the client's own List-Unsubscribe stays; none is added.
	if got := parsed.Header["List-Unsubscribe"]; len(got) != 1 || got[0] != "<mailto:u@example.com>" || bytes.Contains(first, []byte("List-Unsubscribe-Post")) {
		t.Fatalf("one-click applied: %q", got)
	}
}

// An attachment is stored once, in the source: each recipient's object holds
// only its headers.
func TestRawPreparationRecipeIsMuchSmallerThanTheMessage(t *testing.T) {
	service, repo, objects, keys := setup(t)
	attachment := make([]byte, 1<<20)
	if _, err := rand.Read(attachment); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(attachment)
	var body strings.Builder
	for len(encoded) > 76 {
		body.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	body.WriteString(encoded + "\r\n")
	raw := "From: Alice <a@example.com>\r\nTo: b@example.net\r\nCc: c@example.net\r\nSubject: report\r\nMIME-Version: 1.0\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\n" + body.String()
	rawSource(t, objects, raw, nil)
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var stored int
	for _, d := range repo.deliveries {
		object := objects.final[d.ObjectKey]
		if len(object) > 8<<10 {
			t.Fatalf("a recipe object is %d bytes", len(object))
		}
		stored += len(object)
		if message := objects.message(t, d.ObjectKey); len(message) < 1<<20 || !bytes.Contains(message, []byte(raw[len(raw)-200:])) {
			t.Fatal("the assembled message lacks the attachment")
		}
	}
	if stored > 3*(8<<10) || len(objects.source) < 1<<20 {
		t.Fatal(stored, len(objects.source))
	}
	// DKIM verifies over the assembled bytes, body hash included.
	der, err := x509.MarshalPKIXPublicKey(&keys.key.Signer.(*rsa.PrivateKey).PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	record := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(objects.message(t, repo.deliveries[2].ObjectKey)), &dkim.VerifyOptions{
		LookupTXT: func(string) ([]string, error) { return []string{record}, nil },
	})
	if err != nil || len(vs) != 1 || vs[0].Err != nil {
		t.Fatalf("%v %+v", err, vs)
	}
}

func TestRecipeObjectSelfCheck(t *testing.T) {
	raw := []byte("From: a@example.com\nSubject: x\n\nbody\nmore\n")
	scan, err := rawmime.Scan(raw)
	if err != nil {
		t.Fatal(err)
	}
	signed := append([]byte("DKIM-Signature: v=1\r\nFrom: a@example.com\r\n\r\n"), "body\r\nmore\r\n"...)
	const id = "0123456789abcdef0123456789abcdef"
	sum := sha256.Sum256(raw)
	baseSHA := hex.EncodeToString(sum[:])
	good := rawmime.Prepared{Message: signed, HeaderLen: len(signed) - len("body\r\nmore\r\n")}
	if _, err = recipeObject(id, baseSHA, raw, scan.BodyStart, good, nil); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		raw    []byte
		signed rawmime.Prepared
	}{
		"the source is not the one signed": {[]byte("From: a@example.com\nSubject: x\n\nBODY\nmore\n"), good},
		"the body differs":                 {raw, rawmime.Prepared{Message: append(append([]byte{}, signed[:good.HeaderLen]...), "body\r\nmoreX\r\n"...), HeaderLen: good.HeaderLen}},
		"the header length is wrong":       {raw, rawmime.Prepared{Message: signed, HeaderLen: good.HeaderLen - 2}},
	} {
		if _, err = recipeObject(id, baseSHA, tc.raw, scan.BodyStart, tc.signed, nil); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
}

func TestRawPreparationDKIMVerifies(t *testing.T) {
	service, repo, objects, keys := setup(t)
	rawSource(t, objects, rawMessage, nil)
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&keys.key.Signer.(*rsa.PrivateKey).PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	record := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(objects.message(t, repo.deliveries[0].ObjectKey)), &dkim.VerifyOptions{
		LookupTXT: func(domain string) ([]string, error) {
			if domain != "mail._domainkey.example.com" {
				return nil, errors.New("unexpected lookup")
			}
			return []string{record}, nil
		},
	})
	if err != nil || len(vs) != 1 || vs[0].Err != nil || vs[0].Domain != "example.com" {
		t.Fatalf("%v %+v", err, vs)
	}
}

func TestRawPreparationSigningDomainMismatch(t *testing.T) {
	for name, edit := range map[string]func(*fakeRepository, *fakeKeys, *submission.RawHeader){
		"claim":  func(r *fakeRepository, _ *fakeKeys, _ *submission.RawHeader) { r.claim.SenderDomain = "other.example" },
		"key":    func(_ *fakeRepository, k *fakeKeys, _ *submission.RawHeader) { k.key.Domain = "other.example" },
		"source": func(_ *fakeRepository, _ *fakeKeys, h *submission.RawHeader) { h.SenderDomain = "example.net" },
	} {
		t.Run(name, func(t *testing.T) {
			service, repo, objects, keys := setup(t)
			rawSource(t, objects, rawMessage, func(h *submission.RawHeader) { edit(repo, keys, h) })
			if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 || len(objects.final) != 0 || len(repo.failures) != 1 || repo.failures[0] != "signing_key" {
				t.Fatal("prepared across a signing domain mismatch", err, repo.failures)
			}
		})
	}
}

func TestRawPreparationDate(t *testing.T) {
	const noDate = "From: a@example.com\r\nTo: b@example.net\r\nSubject: hi\r\n\r\nbody\r\n"
	const withDate = "From: a@example.com\r\nTo: b@example.net\r\nSubject: hi\r\nDate: Tue, 17 Nov 2026 10:00:00 +0000\r\n\r\nbody\r\n"
	accepted := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	released := time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC)
	for _, tc := range []struct{ name, raw, want string }{
		{"inserted from claim.date", noDate, "Sat, 21 Nov 2026 09:30:00 +0000"},
		{"client date kept", withDate, "Tue, 17 Nov 2026 10:00:00 +0000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, repo, objects, _ := setup(t)
			repo.claim.AcceptedAt, repo.claim.Date = accepted, released
			s, _ := rawmime.Scan([]byte(tc.raw))
			d, err := rawmime.DecideFrom(s, "", rawmime.Policy{Mode: rawmime.FromOff}, func(d string) bool { return d == "example.com" })
			if err != nil {
				t.Fatal(err)
			}
			objects.source, err = submission.EncodeRawSource(submission.RawHeader{
				MailFrom: "a@example.com", Recipients: []string{"b@example.net"}, Kinds: []string{"to"},
				SenderDomain: d.Domain, EnhancedProtection: true,
			}, []byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if err := service.PrepareNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			final := string(objects.message(t, repo.deliveries[0].ObjectKey))
			if got := strings.Count(final, "\r\nDate: "); got != 1 || !strings.Contains(final, "\r\nDate: "+tc.want+"\r\n") {
				t.Fatalf("Date: want %q in\n%s", tc.want, final)
			}
		})
	}
}

func TestRawPreparationRewrittenFrom(t *testing.T) {
	service, repo, objects, _ := setup(t)
	const raw = "From: x@example.org\r\nTo: b@example.net\r\nSubject: hi\r\n\r\nbody\r\n"
	s, _ := rawmime.Scan([]byte(raw))
	d, err := rawmime.DecideFrom(s, "", rawmime.Policy{Mode: rawmime.FromAddress, Address: "noreply@example.com"}, func(d string) bool { return d == "example.com" })
	if err != nil {
		t.Fatal(err)
	}
	objects.source, err = submission.EncodeRawSource(submission.RawHeader{
		Recipients: []string{"b@example.net"}, Kinds: []string{"to"}, From: d.Value, FromField: d.Field, FromRewritten: true,
		OriginalFrom: d.Original, StripSender: d.StripSender, SenderDomain: d.Domain, EnhancedProtection: true,
	}, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	final := string(objects.message(t, repo.deliveries[0].ObjectKey))
	if !strings.Contains(final, "\r\nFrom: <noreply@example.com>\r\n") || strings.Contains(final, "x@example.org") {
		t.Fatalf("From not rewritten:\n%s", final)
	}
}

func TestRawPreparationRejectsAKeptFromOutsideTheRecordedDomain(t *testing.T) {
	service, repo, objects, _ := setup(t)
	var err error
	objects.source, err = submission.EncodeRawSource(submission.RawHeader{
		Recipients: []string{"b@example.net"}, Kinds: []string{"to"}, SenderDomain: "example.com", EnhancedProtection: true,
	}, []byte("From: a@example.net\r\nTo: b@example.net\r\nSubject: hi\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 || len(objects.final) != 0 || len(repo.failures) != 1 || repo.failures[0] != "source_invalid" {
		t.Fatal("prepared a From the source does not vouch for", err, repo.failures)
	}
}

func TestRawPreparationRejectsMessageTheScannerRefuses(t *testing.T) {
	service, repo, objects, _ := setup(t)
	var err error
	objects.source, err = submission.EncodeRawSource(submission.RawHeader{
		Recipients: []string{"b@example.net"}, Kinds: []string{"to"}, SenderDomain: "example.com", EnhancedProtection: true,
	}, []byte("not a message"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 || len(repo.failures) != 1 || repo.failures[0] != "source_invalid" {
		t.Fatal("prepared garbage", err, repo.failures)
	}
}

// The Received trace names Thinpost's host, the transport and the message ID,
// with the acceptance time, and nothing from the client. It is the same on every
// run, and a service without a host name adds none.
func TestPreparationAddsTraceField(t *testing.T) {
	host := func(context.Context) (string, error) { return "mx.example.net", nil }
	const rawWithClientTrace = "Received: from laptop.corp.example ([10.9.8.7])\r\n\tby smtp.corp.example; Mon, 01 Jan 2024 00:00:00 +0000\r\n" + rawMessage
	for _, tc := range []struct {
		name, with string
		raw        bool
	}{{"smtp", "ESMTPSA", true}, {"http", "HTTP", false}} {
		t.Run(tc.name, func(t *testing.T) {
			service, repo, objects, _ := setup(t)
			service.Hostname = host
			if tc.raw {
				rawSource(t, objects, rawWithClientTrace, nil)
			}
			if err := service.PrepareNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := objects.message(t, repo.deliveries[0].ObjectKey)
			want := "Received: by mx.example.net (Thinpost)\r\n\twith " + tc.with + " id " + repo.claim.MessageID + ";\r\n\t" + repo.claim.AcceptedAt.UTC().Format(time.RFC1123Z) + "\r\n"
			if !bytes.Contains(got, []byte(want)) || bytes.Count(got, []byte("Received:")) != 1 {
				t.Fatalf("want %q in\n%s", want, got)
			}
			if bytes.Contains(got, []byte("corp.example")) || bytes.Contains(got, []byte("10.9.8.7")) {
				t.Fatal("client trace survived")
			}
		})
	}
	t.Run("a failing host lookup does not fail preparation", func(t *testing.T) {
		service, repo, objects, _ := setup(t)
		service.Hostname = func(context.Context) (string, error) { return "", errors.New("db down: secret@example.com") }
		rawSource(t, objects, rawMessage, nil)
		if err := service.PrepareNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if repo.commits != 1 || bytes.Contains(objects.message(t, repo.deliveries[0].ObjectKey), []byte("Received:")) {
			t.Fatal("expected a committed message without a trace")
		}
	})
	t.Run("none without a host", func(t *testing.T) {
		service, repo, objects, _ := setup(t)
		rawSource(t, objects, rawMessage, nil)
		if err := service.PrepareNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(objects.message(t, repo.deliveries[0].ObjectKey), []byte("Received:")) {
			t.Fatal("unexpected trace")
		}
	})
}

type unsubscribeCapture struct{ ids []string }

func (c *unsubscribeCapture) RecordUnsubscribe(_ context.Context, id string) error {
	c.ids = append(c.ids, id)
	return nil
}

const clientUnsubscribeRaw = "DKIM-Signature: v=1; a=rsa-sha256; d=client.example; s=one; c=relaxed/relaxed; h=from:to:subject; bh=AAAA; b=AAAA\r\n" +
	"DKIM-Signature: v=1; a=rsa-sha256; d=client.example; s=two; c=relaxed/relaxed; h=from:list-unsubscribe:list-unsubscribe-post; bh=AAAA; b=AAAA\r\n" +
	"From: Alice <a@example.com>\r\nTo: b@example.net\r\nCc: c@example.net\r\nSubject: hello\r\nMessage-ID: <client@example.org>\r\n" +
	"List-Unsubscribe: <https://App.Example.com/u?t=abc>, <mailto:u@example.com>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n\r\nraw body\r\n"

func oneClickRawService(t *testing.T) (*Service, *fakeRepository, *fakeObjects, *fakeKeys) {
	t.Helper()
	service, repo, objects, keys := setup(t)
	repo.claim.ListUnsubscribe = true
	service.Tracking = &TrackingConfig{Origin: "https://tracking.example.com", Links: preparationLinkKeys(t), EventLimit: 10, Objects: &fakeTrackingObjects{}}
	return service, repo, objects, keys
}

// With one-click on, every recipient's copy has its own link, the client's
// List-Unsubscribe fields are gone, and the client signatures that covered them
// are dropped while the others stay.
func TestRawOneClickGivesEachRecipientItsOwnLinkAndReplacesTheClients(t *testing.T) {
	service, repo, objects, keys := oneClickRawService(t)
	rawSource(t, objects, clientUnsubscribeRaw, nil)
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.commits != 1 || len(repo.deliveries) != 3 || len(objects.final) != 3 {
		t.Fatalf("%d commits, %d deliveries", repo.commits, len(repo.deliveries))
	}
	capture := &unsubscribeCapture{}
	handler := &tracking.Handler{Links: service.Tracking.Links, Unsubscribes: capture}
	seen := map[string]bool{}
	der, err := x509.MarshalPKIXPublicKey(&keys.key.Signer.(*rsa.PrivateKey).PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	record := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)
	for _, delivery := range repo.deliveries {
		final := objects.message(t, delivery.ObjectKey)
		parsed, err := mail.ReadMessage(bytes.NewReader(final))
		if err != nil {
			t.Fatal(err)
		}
		list := parsed.Header["List-Unsubscribe"]
		if len(list) != 1 || !strings.HasPrefix(list[0], "<https://tracking.example.com/t/u/v3/") || seen[list[0]] || len(parsed.Header["List-Unsubscribe-Post"]) != 1 || parsed.Header.Get("List-Unsubscribe-Post") != "List-Unsubscribe=One-Click" {
			t.Fatalf("unexpected headers %q", list)
		}
		seen[list[0]] = true
		if bytes.Contains(final, []byte("app.example.com")) || bytes.Contains(final, []byte("u@example.com")) || bytes.Contains(bytes.ToLower(final), []byte("s=two")) {
			t.Fatal("the client's unsubscribe link or the signature that covered it survived")
		}
		// One client signature (not covering the unsubscribe fields) is kept, and
		// Thinpost's own covers both new fields.
		if got := bytes.Count(final, []byte("d=client.example")); got != 1 {
			t.Fatalf("client signatures kept: %d", got)
		}
		w := httptest.NewRecorder()
		post := httptest.NewRequest(http.MethodPost, strings.Trim(list[0], "<>"), strings.NewReader("List-Unsubscribe=One-Click"))
		post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		handler.ServeHTTP(w, post)
		if w.Code != 200 || capture.ids[len(capture.ids)-1] != delivery.ID {
			t.Fatal("the link does not verify to its own delivery", w.Code)
		}
		vs, err := dkim.VerifyWithOptions(bytes.NewReader(final), &dkim.VerifyOptions{
			LookupTXT: func(domain string) ([]string, error) {
				if domain != "mail._domainkey.example.com" {
					return nil, errors.New("unexpected lookup")
				}
				return []string{record}, nil
			},
		})
		var thinpost *dkim.Verification
		for _, v := range vs {
			if v.Domain == "example.com" {
				thinpost = v
			}
		}
		if err != nil || thinpost == nil || thinpost.Err != nil {
			t.Fatalf("Thinpost's signature does not verify: %v %+v", err, vs)
		}
		if signed := strings.Join(thinpost.HeaderKeys, ":"); !strings.Contains(strings.ToLower(signed), "list-unsubscribe-post") {
			t.Fatal("the signature does not cover the one-click fields", signed)
		}
		if delivery.Forward == nil || delivery.Forward.Host != "app.example.com" || delivery.Forward.Reason != "" {
			t.Fatalf("forward %+v", delivery.Forward)
		}
	}
	// Each copy differs, so each is its own object; the body is the client's.
	for _, delivery := range repo.deliveries {
		if !bytes.HasSuffix(objects.message(t, delivery.ObjectKey), []byte("\r\n\r\nraw body\r\n")) {
			t.Fatal("the body changed")
		}
	}
	if len(seen) != 3 {
		t.Fatal("links are shared")
	}
}

func TestRawOneClickAddsHeadersToAMessageWithoutThem(t *testing.T) {
	service, repo, objects, _ := oneClickRawService(t)
	rawSource(t, objects, "From: Alice <a@example.com>\r\nTo: b@example.net\r\nSubject: hello\r\n\r\nraw body\r\n", nil)
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, delivery := range repo.deliveries {
		parsed, err := mail.ReadMessage(bytes.NewReader(objects.message(t, delivery.ObjectKey)))
		if err != nil || len(parsed.Header["List-Unsubscribe"]) != 1 || delivery.Forward != nil {
			t.Fatal("expected one added header and no forward", err)
		}
	}
}

func TestRawOneClickUsesTheDeploymentOriginEvenWithATrackingDomain(t *testing.T) {
	service, repo, objects, _ := oneClickRawService(t)
	service.Tracking.CustomDomains = true
	repo.claim.TrackingDomain = "links.example.com"
	rawSource(t, objects, rawMessage, nil)
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, delivery := range repo.deliveries {
		final := objects.message(t, delivery.ObjectKey)
		if bytes.Contains(final, []byte("links.example.com")) || !bytes.Contains(final, []byte("<https://tracking.example.com/t/u/v3/")) {
			t.Fatal("the unsubscribe link left the deployment origin")
		}
	}
}

func TestRawOneClickWithoutTrackingRuntimeFailsVisibly(t *testing.T) {
	for name, config := range map[string]*TrackingConfig{
		"no tracking": nil,
		"no keys":     {Origin: "https://tracking.example.com", EventLimit: 10},
		"no origin":   {EventLimit: 10},
	} {
		t.Run(name, func(t *testing.T) {
			service, repo, objects, _ := setup(t)
			repo.claim.ListUnsubscribe = true
			service.Tracking = config
			rawSource(t, objects, rawMessage, nil)
			if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 || len(objects.final) != 0 || len(repo.failures) != 1 || repo.failures[0] != "internal" {
				t.Fatal("prepared without the runtime or not failed with reason internal", err, repo.failures)
			}
		})
	}
}

func TestRawOneClickIsSkippedWhenTheMessageCannotCarryTheHeaders(t *testing.T) {
	var lines []string
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }))
	service, repo, objects, keys := oneClickRawService(t)
	const head = "From: Alice <a@example.com>\r\nTo: b@example.net\r\nCc: c@example.net\r\nSubject: hello\r\nMessage-ID: <client@example.org>\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\n" +
		"List-Unsubscribe: <mailto:u@example.com>\r\n\r\n"
	scanned := mustScanRaw(t, head)
	decision, err := rawmime.DecideFrom(scanned, "", rawmime.Policy{Mode: rawmime.FromOff}, func(d string) bool { return d == "example.com" })
	if err != nil {
		t.Fatal(err)
	}
	size, err := rawmime.PreparedSize([]byte(head), rawmime.PrepareInput{Scan: scanned, From: decision, MessageID: repo.claim.MessageID, Date: repo.claim.date(), Selector: "mail", Signer: keys.key.Signer})
	if err != nil {
		t.Fatal(err)
	}
	// Bare line feeds become CRLF (two bytes each). Leave 60 bytes: enough for
	// the message as written, not for Thinpost's longer headers.
	lf := (edgeprotocol.MaxMIMEBytes - size.Total - 60) / 2
	rawSource(t, objects, head+strings.Repeat("\n", lf), nil)
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal("preparation failed instead of skipping one-click:", err)
	}
	if repo.commits != 1 || len(repo.deliveries) != 3 {
		t.Fatal("not committed")
	}
	for _, delivery := range repo.deliveries {
		final := objects.message(t, delivery.ObjectKey)
		if len(final) > edgeprotocol.MaxMIMEBytes || !bytes.Contains(final, []byte("List-Unsubscribe: <mailto:u@example.com>\r\n")) || bytes.Contains(final, []byte("tracking.example.com")) || delivery.Forward != nil {
			t.Fatal("the client's headers were not kept, or Thinpost's were added, or a forward was recorded", len(final), edgeprotocol.MaxMIMEBytes, bytes.Contains(final, []byte("List-Unsubscribe: <mailto:u@example.com>\r\n")), bytes.Contains(final, []byte("tracking.example.com")), delivery.Forward)
		}
	}
	skipped := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "one_click ") {
			skipped++
		}
	}
	if skipped != 1 {
		t.Fatal("expected one constant log line", lines)
	}
}

func mustScanRaw(t *testing.T, raw string) *rawmime.Scanned {
	t.Helper()
	s, err := rawmime.Scan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
