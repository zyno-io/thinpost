package preparation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"regexp"
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

const (
	deploymentOrigin = "https://tracking.example.com"
	customOrigin     = "https://links.customer.example"
)

func attachmentBase64(t *testing.T) (raw []byte, encoded string) {
	t.Helper()
	raw = make([]byte, 6000)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	enc := base64.StdEncoding.EncodeToString(raw)
	var sb strings.Builder
	for len(enc) > 76 {
		sb.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	return raw, sb.String() + enc
}

func trackedRaw(attachment string) string {
	return "From: Alice <a@example.com>\r\nTo: b@example.net\r\nCc: c@example.net\r\nSubject: hello\r\nMessage-ID: <client@example.org>\r\nMIME-Version: 1.0\r\n" +
		"List-Unsubscribe: <https://app.example.org/u>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n" +
		"Content-Type: multipart/mixed; boundary=\"MIX\"\r\n\r\n--MIX\r\n" +
		"Content-Type: multipart/alternative; boundary=\"ALT\"\r\n\r\n" +
		"--ALT\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nplain https://plain.example.org/x\r\n" +
		"--ALT\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"<html><body><a href=3D\"https://shop.example.org/a?x=3D1\">one</a> <a href=3D\"https://shop.example.org/b\">two</a></body></html>\r\n--ALT--\r\n" +
		"--MIX\r\nContent-Type: application/pdf; name=\"a.pdf\"\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		attachment + "\r\n--MIX--\r\n"
}

type trackedRig struct {
	service *Service
	repo    *fakeRepository
	objects *fakeObjects
	keys    *fakeKeys
	mapped  *fakeTrackingObjects
}

func newTrackedRig(t *testing.T, raw string, opens, clicks, oneClick bool) *trackedRig {
	t.Helper()
	service, repo, objects, keys := setup(t)
	rawSource(t, objects, raw, func(h *submission.RawHeader) { h.TrackOpens, h.TrackClicks = opens, clicks })
	mapped := &fakeTrackingObjects{}
	service.Tracking = &TrackingConfig{Origin: deploymentOrigin, CustomDomains: true, Links: preparationLinkKeys(t), EventLimit: 7, Objects: mapped}
	repo.claim.TrackingDomain = "links.customer.example"
	repo.claim.ListUnsubscribe = oneClick
	return &trackedRig{service, repo, objects, keys, mapped}
}

func (r *trackedRig) prepare(t *testing.T) {
	t.Helper()
	if err := r.service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (r *trackedRig) verifyDKIM(t *testing.T, msg []byte) {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&r.keys.key.Signer.(*rsa.PrivateKey).PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	record := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(msg), &dkim.VerifyOptions{LookupTXT: func(d string) ([]string, error) {
		if d != "mail._domainkey.example.com" {
			return nil, errors.New("unexpected lookup")
		}
		return []string{record}, nil
	}})
	if err != nil || len(vs) != 1 || vs[0].Err != nil || vs[0].Domain != "example.com" {
		t.Fatalf("DKIM: %v %+v", err, vs)
	}
}

func htmlOf(t *testing.T, msg []byte) string {
	t.Helper()
	parsed, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	var html string
	walkParts(t, textproto.MIMEHeader(parsed.Header), parsed.Body, func(h textproto.MIMEHeader, data []byte) {
		if strings.HasPrefix(h.Get("Content-Type"), "text/html") {
			html = string(data)
		}
	})
	return html
}

// Each recipient gets its own tokens, mapping object and tracking_links rows;
// links and the pixel use the app's tracking domain, one-click stays on the
// deployment origin; attachments and text parts are byte for byte the source's.
func TestRawTrackingPerRecipient(t *testing.T) {
	attachment, encoded := attachmentBase64(t)
	raw := trackedRaw(encoded)
	r := newTrackedRig(t, raw, true, true, true)
	r.prepare(t)
	if len(r.repo.deliveries) != 3 || len(r.mapped.mappings) != 3 {
		t.Fatalf("%d deliveries, %d mapping objects", len(r.repo.deliveries), len(r.mapped.mappings))
	}
	tokens := map[string]bool{}
	pathRE := regexp.MustCompile(regexp.QuoteMeta(customOrigin) + `/t/[a-z]/v3/[A-Za-z0-9_\-/]+`)
	var firstHTML, firstHeaderLink string
	for i, d := range r.repo.deliveries {
		if d.Tracking == nil || !d.Tracking.Indefinite || d.Tracking.EventLimit != 7 || d.Tracking.ObjectKey == "" || d.Format != prepared.FormatRecipe {
			t.Fatalf("delivery %d: %+v", i, d.Tracking)
		}
		clicks, opens := 0, 0
		for _, l := range d.Tracking.Links {
			switch l.Kind {
			case "click":
				clicks++
			case "open":
				opens++
			}
		}
		if clicks != 2 || opens != 1 || len(d.Tracking.Links) != 3 {
			t.Fatalf("links %d clicks %d opens", clicks, opens)
		}
		for _, m := range r.mapped.mappings[i] {
			if tokens[m.Token] {
				t.Fatal("a tracking token is shared between recipients")
			}
			tokens[m.Token] = true
		}
		msg := r.objects.message(t, d.ObjectKey)
		r.verifyDKIM(t, msg)
		html := htmlOf(t, msg)
		if strings.Contains(html, "shop.example.org") || len(pathRE.FindAllString(html, -1)) != 3 || strings.Contains(html, deploymentOrigin) {
			t.Fatalf("html: %s", html)
		}
		parsed, _ := mail.ReadMessage(bytes.NewReader(msg))
		link := parsed.Header.Get("List-Unsubscribe")
		if !strings.HasPrefix(link, "<"+deploymentOrigin+"/t/u/v3/") || strings.Contains(link, "client") || strings.Contains(link, "app.example.org") {
			t.Fatalf("List-Unsubscribe: %q", link)
		}
		// The attachment and the text part are the source's bytes.
		if !bytes.Contains(msg, []byte(encoded+"\r\n--MIX--")) || !bytes.Contains(msg, []byte("plain https://plain.example.org/x\r\n--ALT")) {
			t.Fatal("attachment or text part changed")
		}
		if i == 0 {
			firstHTML, firstHeaderLink = html, link
		} else if html == firstHTML || link == firstHeaderLink {
			t.Fatal("recipients share tokens or an unsubscribe link")
		}
		// The recipe is small: the attachment is not copied.
		if len(r.objects.final[d.ObjectKey]) > len(attachment)/2 {
			t.Fatalf("recipe object is %d bytes", len(r.objects.final[d.ObjectKey]))
		}
	}
	// Every mapping is one pixel and two click destinations.
	var dests []string
	for _, m := range r.mapped.mappings[0] {
		if m.Kind == "click" {
			dests = append(dests, m.Destination)
		}
	}
	if strings.Join(dests, " ") != "https://shop.example.org/a?x=1 https://shop.example.org/b" {
		t.Fatalf("destinations %q", dests)
	}
}

// The links in the delivered message resolve through the tracking handler and
// record against the delivery's own token.
func TestRawTrackingLinksRedirectAndRecord(t *testing.T) {
	_, encoded := attachmentBase64(t)
	r := newTrackedRig(t, trackedRaw(encoded), true, true, false)
	r.prepare(t)
	d := r.repo.deliveries[1]
	html := htmlOf(t, r.objects.message(t, d.ObjectKey))
	visits := &visitRecorder{}
	handler := &tracking.Handler{Links: r.service.Tracking.Links, Visitor: visits}
	seen := map[string]string{}
	for _, link := range regexp.MustCompile(regexp.QuoteMeta(customOrigin)+`(/t/[a-z]/v3/[^"'\s>]+)`).FindAllStringSubmatch(html, -1) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, link[1], nil))
		seen[link[1][:5]+w.Header().Get("Location")] = w.Result().Status
	}
	if len(visits.kinds) != 3 || seen["/t/c/https://shop.example.org/a?x=1"] == "" || seen["/t/c/https://shop.example.org/b"] == "" || seen["/t/o/"] == "" {
		t.Fatalf("%v %v", visits.kinds, seen)
	}
	// The recorded hashes are the delivery's own mapping.
	want := map[[32]byte]bool{}
	for _, l := range d.Tracking.Links {
		want[l.Hash] = true
	}
	for _, h := range visits.hashes {
		if !want[h] {
			t.Fatal("a visit recorded against another delivery's token")
		}
	}
}

type visitRecorder struct {
	hashes [][32]byte
	kinds  []string
}

func (v *visitRecorder) Visit(_ context.Context, hash [32]byte, kind string, _ bool) error {
	v.hashes, v.kinds = append(v.hashes, hash), append(v.kinds, kind)
	return nil
}

// A top-level single-part HTML message gets new Content-Type and
// Content-Transfer-Encoding fields, with one-click, and still verifies.
func TestRawTrackingTopLevelHTML(t *testing.T) {
	raw := "From: Alice <a@example.com>\r\nTo: b@example.net\r\nSubject: hi\r\nMIME-Version: 1.0\r\nContent-Type: text/html;\r\n charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n<p>caf\xc3\xa9 <a href=\"https://shop.example.org/a\">a</a></p>\r\n"
	r := newTrackedRig(t, raw, true, true, true)
	r.prepare(t)
	for _, d := range r.repo.deliveries {
		msg := r.objects.message(t, d.ObjectKey)
		r.verifyDKIM(t, msg)
		parsed, err := mail.ReadMessage(bytes.NewReader(msg))
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Header.Get("Content-Type") != "text/html; charset=utf-8" || parsed.Header.Get("Content-Transfer-Encoding") != "quoted-printable" || len(parsed.Header["Content-Type"]) != 1 {
			t.Fatalf("%v", parsed.Header)
		}
		body, _ := io.ReadAll(quotedprintable.NewReader(parsed.Body))
		if !strings.Contains(string(body), "café") || strings.Contains(string(body), "shop.example.org") || !strings.Contains(string(body), customOrigin) {
			t.Fatalf("%s", body)
		}
		if !strings.Contains(parsed.Header.Get("List-Unsubscribe"), deploymentOrigin) {
			t.Fatal("no one-click")
		}
	}
}

// A source written before SMTP tracking existed (a RawHeader without the
// fields) is prepared untracked, whatever the runtime.
func TestRawPreparationWithoutTrackingFieldsIsUntracked(t *testing.T) {
	_, encoded := attachmentBase64(t)
	raw := trackedRaw(encoded)
	r := newTrackedRig(t, raw, false, false, false)
	lines := captureOplog(t)
	r.prepare(t)
	for _, d := range r.repo.deliveries {
		if d.Tracking != nil {
			t.Fatal("tracking on an untracked source")
		}
		msg := r.objects.message(t, d.ObjectKey)
		if !bytes.HasSuffix(msg, []byte(raw[strings.Index(raw, "\r\n\r\n")+4:])) {
			t.Fatal("body changed")
		}
		r.verifyDKIM(t, msg)
	}
	if len(r.mapped.mappings) != 0 || strings.Contains(strings.Join(*lines, "\n"), "tracking") {
		t.Fatal("tracking work for an untracked source")
	}
}

func captureOplog(t *testing.T) *[]string {
	var lines []string
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }))
	return &lines
}

// HTTP fails visibly when the tracking runtime is missing: the caller asked for
// tracked HTML and can be told. An SMTP client cannot, so the message goes out
// untracked with an oplog line.
func TestRawTrackingWithoutRuntimeSendsUntracked(t *testing.T) {
	_, encoded := attachmentBase64(t)
	for name, mutate := range map[string]func(*Service){
		"no tracking config": func(s *Service) { s.Tracking = nil },
		"no mapping objects": func(s *Service) { s.Tracking.Objects = nil },
		"no link keys":       func(s *Service) { s.Tracking.Links = nil },
		"no event limit":     func(s *Service) { s.Tracking.EventLimit = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			r := newTrackedRig(t, trackedRaw(encoded), true, true, false)
			mutate(r.service)
			lines := captureOplog(t)
			r.prepare(t)
			if len(r.repo.deliveries) != 3 || r.repo.deliveries[0].Tracking != nil {
				t.Fatal("not prepared untracked")
			}
			if !strings.Contains(strings.Join(*lines, "\n"), "status=skipped reason=not_configured") {
				t.Fatalf("log %q", *lines)
			}
			msg := r.objects.message(t, r.repo.deliveries[0].ObjectKey)
			if !strings.Contains(htmlOf(t, msg), "shop.example.org") {
				t.Fatal("rewritten without a runtime")
			}
		})
	}
}

func TestRawTrackingMappingWriteFailureFailsPreparation(t *testing.T) {
	_, encoded := attachmentBase64(t)
	r := newTrackedRig(t, trackedRaw(encoded), true, true, false)
	r.mapped.err = errors.New("store down")
	if err := r.service.PrepareNext(context.Background()); err == nil || r.repo.commits != 0 {
		t.Fatalf("%v, %d commits", err, r.commits())
	}
}

func (r *trackedRig) commits() int { return r.repo.commits }

// Untracked mail that cannot carry the tracking (signed, text only) goes out
// as the client wrote it, with one-click still added to signed mail.
func TestRawTrackingSkipsSignedAndTextOnlyMail(t *testing.T) {
	signed := "From: Alice <a@example.com>\r\nTo: b@example.net\r\nSubject: hi\r\nMIME-Version: 1.0\r\nContent-Type: multipart/signed; boundary=S; protocol=\"application/pkcs7-signature\"\r\n\r\n--S\r\nContent-Type: text/html\r\n\r\n<a href=\"https://shop.example.org/\">x</a>\r\n--S\r\nContent-Type: application/pkcs7-signature\r\n\r\nxx\r\n--S--\r\n"
	r := newTrackedRig(t, signed, true, true, true)
	lines := captureOplog(t)
	r.prepare(t)
	msg := r.objects.message(t, r.repo.deliveries[0].ObjectKey)
	if r.repo.deliveries[0].Tracking != nil || !bytes.HasSuffix(msg, []byte(signed[strings.Index(signed, "\r\n\r\n")+4:])) || !bytes.Contains(msg, []byte("List-Unsubscribe-Post: List-Unsubscribe=One-Click")) {
		t.Fatal("signed mail was changed or lost one-click")
	}
	if !strings.Contains(strings.Join(*lines, "\n"), "reason=signed_or_encrypted") {
		t.Fatalf("log %q", *lines)
	}
}

// With the real origin and the one-click link, a tracked message that does not
// fit the edge's limit is sent with less tracking, once for the whole message.
func TestRawTrackingWithdrawsKindsThatDoNotFit(t *testing.T) {
	mk := func(pad int) string {
		return "From: Alice <a@example.com>\r\nTo: b@example.net\r\nSubject: hi\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\n" +
			strings.Repeat("\n", pad/2) + "--B\r\nContent-Type: text/html\r\n\r\n<html><body>" + strings.Repeat("<a href=\"https://shop.example.org/item\">i</a>\r\n", 40) + "</body></html>\r\n--B--\r\n"
	}
	measure := func(r *trackedRig, raw string) int {
		s, err := rawmime.Scan([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		d, err := rawmime.DecideFrom(s, "", rawmime.Policy{Mode: rawmime.FromOff}, func(d string) bool { return d == "example.com" })
		if err != nil {
			t.Fatal(err)
		}
		size, err := rawmime.PreparedSize([]byte(raw), rawmime.PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a", 32), Date: time.Now(), Selector: "mail", Signer: r.keys.key.Signer})
		if err != nil {
			t.Fatal(err)
		}
		return size.Total
	}
	probe := newTrackedRig(t, mk(0), false, false, false)
	room := edgeprotocol.MaxMIMEBytes - measure(probe, mk(0))
	for name, tc := range map[string]struct {
		margin        int
		opens, clicks bool
	}{"everything fits": {room - 300000, true, true}, "without clicks": {2000, true, false}, "untracked": {5, false, false}} {
		t.Run(name, func(t *testing.T) {
			r := newTrackedRig(t, mk(room-tc.margin), true, true, true)
			lines := captureOplog(t)
			r.prepare(t)
			for _, d := range r.repo.deliveries {
				opens, clicks := 0, 0
				if d.Tracking != nil {
					for _, l := range d.Tracking.Links {
						if l.Kind == "open" {
							opens++
						} else {
							clicks++
						}
					}
				}
				if (opens == 1) != tc.opens || (clicks > 0) != tc.clicks {
					t.Fatalf("%d opens %d clicks", opens, clicks)
				}
				if msg := r.objects.message(t, d.ObjectKey); len(msg) > edgeprotocol.MaxMIMEBytes {
					t.Fatal("oversize")
				}
			}
			logged := strings.Contains(strings.Join(*lines, "\n"), "reason=message_too_large")
			if logged != (!tc.clicks) {
				t.Fatalf("log %q", *lines)
			}
		})
	}
}
