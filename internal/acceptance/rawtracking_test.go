package acceptance

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/message"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/receivedtrace"
	"github.com/zyno-io/thinpost/internal/submission"
)

const htmlHead = "From: Sender <sender@example.com>\r\nTo: rcpt@example.net\r\nSubject: Hello\r\nMessage-ID: <abc@client.example.org>\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\nMIME-Version: 1.0\r\n"

// htmlMail is an alternative message: pad bytes of padding (prepared size), then an HTML part with n links.
func htmlMail(links, pad int) string {
	var sb strings.Builder
	sb.WriteString(htmlHead + "Content-Type: multipart/alternative; boundary=\"B\"\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\n")
	// Each bare LF becomes a CRLF, so the prepared message grows by two bytes
	// for each byte of source: the size limit is reached before the source's.
	sb.WriteString(strings.Repeat("\n", pad/2))
	sb.WriteString("--B\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<html><body>")
	for i := 0; i < links; i++ {
		fmt.Fprintf(&sb, "<a href=\"https://shop.example.org/item/%d\">item</a>\r\n", i)
	}
	sb.WriteString("</body></html>\r\n--B--\r\n")
	return sb.String()
}

func acceptTracked(t *testing.T, service *Service, app submission.App, raw string, mutate func(*RawRequest)) (rawSourceHeader, []string, error) {
	t.Helper()
	repo := service.repository.(*smtpRepository)
	objects := service.objects.(*memoryObjects)
	var lines []string
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }))
	req := rawRequest(t, raw, "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
	if mutate != nil {
		mutate(&req)
	}
	before := len(repo.records)
	_, err := service.AcceptRaw(context.Background(), app, smtpFence, req)
	if err != nil {
		return rawSourceHeader{}, lines, err
	}
	record := repo.records[before]
	source, err := submission.DecodeSource(objects.objects[record.ObjectKey], false)
	if err != nil {
		t.Fatal(err)
	}
	return rawSourceHeader{source.RawHeader.TrackOpens, source.RawHeader.TrackClicks}, lines, nil
}

type rawSourceHeader struct{ opens, clicks bool }

func trackingService(t *testing.T, defaults bool) *Service {
	t.Helper()
	service := rawService(t, &smtpRepository{memoryRepository: &memoryRepository{}}, &memoryObjects{}, true)
	service.ApplyTrackingDefaults = defaults
	return service
}

func ptr(b bool) *bool { return &b }

func TestAcceptRawTrackingDefaultsAndHeaders(t *testing.T) {
	on := smtpApp
	on.DefaultTrackOpens, on.DefaultTrackClicks = true, true
	opensOnly := smtpApp
	opensOnly.DefaultTrackOpens = true
	for name, tc := range map[string]struct {
		app                   submission.App
		defaults              bool
		raw                   string
		opens, clicks         *bool
		wantOpens, wantClicks bool
		wantLog               string
	}{
		"defaults apply":                  {on, true, htmlMail(2, 0), nil, nil, true, true, ""},
		"only the default kinds":          {opensOnly, true, htmlMail(2, 0), nil, nil, true, false, ""},
		"explicit false wins":             {on, true, htmlMail(2, 0), ptr(false), ptr(false), false, false, ""},
		"explicit false per kind":         {on, true, htmlMail(2, 0), nil, ptr(false), true, false, ""},
		"explicit true over no default":   {smtpApp, true, htmlMail(2, 0), ptr(true), ptr(true), true, true, ""},
		"no default no header":            {smtpApp, true, htmlMail(2, 0), nil, nil, false, false, ""},
		"defaults need the runtime":       {on, false, htmlMail(2, 0), nil, nil, false, false, ""},
		"explicit true needs the runtime": {smtpApp, false, htmlMail(2, 0), ptr(true), nil, false, false, "reason=not_configured"},
		"plain text with defaults":        {on, true, plainRaw, nil, nil, false, false, ""},
		"plain text with explicit true":   {smtpApp, true, plainRaw, ptr(true), nil, false, false, "reason=not_mime"},
		"signed mail with defaults": {on, true, htmlHead + "Content-Type: multipart/signed; boundary=S\r\n\r\n--S\r\nContent-Type: text/html\r\n\r\n<a href=\"https://a.example.org/\">x</a>\r\n--S\r\nContent-Type: application/pkcs7-signature\r\n\r\nxx\r\n--S--\r\n",
			nil, nil, false, false, "reason=signed_or_encrypted"},
		"too many links": {on, true, htmlMail(101, 0), nil, nil, true, false, "reason=too_many_links"},
	} {
		t.Run(name, func(t *testing.T) {
			service := trackingService(t, tc.defaults)
			got, lines, err := acceptTracked(t, service, tc.app, tc.raw, func(r *RawRequest) { r.TrackOpens, r.TrackClicks = tc.opens, tc.clicks })
			if err != nil {
				t.Fatal(err)
			}
			if got.opens != tc.wantOpens || got.clicks != tc.wantClicks {
				t.Fatalf("stored %+v", got)
			}
			logged := ""
			for _, l := range lines {
				if strings.HasPrefix(l, "tracking ") {
					logged = l
				}
			}
			if tc.wantLog == "" && logged != "" || tc.wantLog != "" && !strings.Contains(logged, tc.wantLog) {
				t.Fatalf("log %q, want %q", logged, tc.wantLog)
			}
		})
	}
}

// Tracking that does not fit is withdrawn, clicks first; the untracked message
// still fits and is accepted.
func TestAcceptRawWithdrawsTrackingThatDoesNotFit(t *testing.T) {
	signer, err := rawmime.SizingSigner(message.MaxDKIMRSABits)
	if err != nil {
		t.Fatal(err)
	}
	trace, _ := receivedtrace.Field(receivedtrace.SizingHost(), receivedtrace.WithSMTP, sizingMessageID, time.Unix(0, 0).UTC())
	unsub, _ := sizingUnsubscribeLink()
	untracked := func(raw string) int {
		req := rawRequest(t, raw, "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
		worst := 0
		for _, link := range []string{"", unsub} {
			size, err := rawmime.PreparedSize(req.Raw, rawmime.PrepareInput{Scan: req.Scan, From: req.From, MessageID: sizingMessageID, Date: time.Unix(0, 0).UTC(), Selector: sizingSelector, Signer: signer, Received: trace, ListUnsubscribe: link})
			if err != nil {
				t.Fatal(err)
			}
			worst = max(worst, size.Total)
		}
		return worst
	}
	base := untracked(htmlMail(30, 0))
	room := rawmime.MaxPreparedMessage - rawmime.SizeSlack - base
	on := smtpApp
	on.DefaultTrackOpens, on.DefaultTrackClicks = true, true
	for name, tc := range map[string]struct {
		margin                int
		wantOpens, wantClicks bool
	}{
		"fits with both":      {room - 200000, true, true},
		"fits without clicks": {1500, true, false},
		"fits untracked only": {10, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			service := trackingService(t, true)
			raw := htmlMail(30, room-tc.margin)
			if size := untracked(raw); rawmime.MaxPreparedMessage-rawmime.SizeSlack-size < 0 {
				t.Fatalf("untracked does not fit: %d", size)
			}
			got, lines, err := acceptTracked(t, service, on, raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.opens != tc.wantOpens || got.clicks != tc.wantClicks {
				t.Fatalf("stored %+v", got)
			}
			if wantLog := !tc.wantClicks; wantLog != strings.Contains(strings.Join(lines, "\n"), "reason=message_too_large") {
				t.Fatalf("log %q", lines)
			}
		})
	}
	// Untracked that does not fit is refused as before.
	service := trackingService(t, true)
	if _, _, err := acceptTracked(t, service, on, htmlMail(30, room+1000), nil); err == nil {
		t.Fatal("an oversize message was accepted")
	}
}
