package preparation

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/submission"
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

func TestRawPreparationStoresIdenticalObjectsPerRecipient(t *testing.T) {
	service, repo, objects, _ := setup(t)
	rawSource(t, objects, rawMessage, nil)
	// Tracking and one-click on the app side must not touch a raw message.
	repo.claim.ListUnsubscribe = true
	service.Tracking = &TrackingConfig{Origin: "https://tracking.example.com", Links: preparationLinkKeys(t), EventLimit: 10, Objects: &fakeTrackingObjects{}}
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.commits != 1 || len(repo.deliveries) != 3 || len(objects.final) != 3 {
		t.Fatalf("%d commits, %d deliveries", repo.commits, len(repo.deliveries))
	}
	var first []byte
	for i, d := range repo.deliveries {
		if d.RecipientIndex != i || d.Tracking != nil {
			t.Fatal("index or tracking", i)
		}
		data := objects.final[d.ObjectKey]
		if first == nil {
			first = data
		} else if !bytes.Equal(first, data) {
			t.Fatal("per-recipient objects differ")
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
	// The client's own List-Unsubscribe stays; none is added.
	if got := parsed.Header["List-Unsubscribe"]; len(got) != 1 || got[0] != "<mailto:u@example.com>" || bytes.Contains(first, []byte("List-Unsubscribe-Post")) {
		t.Fatalf("one-click applied: %q", got)
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
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(objects.final[repo.deliveries[0].ObjectKey]), &dkim.VerifyOptions{
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
			final := string(objects.final[repo.deliveries[0].ObjectKey])
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
	final := string(objects.final[repo.deliveries[0].ObjectKey])
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
			got := objects.final[repo.deliveries[0].ObjectKey]
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
		if repo.commits != 1 || bytes.Contains(objects.final[repo.deliveries[0].ObjectKey], []byte("Received:")) {
			t.Fatal("expected a committed message without a trace")
		}
	})
	t.Run("none without a host", func(t *testing.T) {
		service, repo, objects, _ := setup(t)
		rawSource(t, objects, rawMessage, nil)
		if err := service.PrepareNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(objects.final[repo.deliveries[0].ObjectKey], []byte("Received:")) {
			t.Fatal("unexpected trace")
		}
	})
}
