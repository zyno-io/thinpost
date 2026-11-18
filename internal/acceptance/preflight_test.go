package acceptance

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/message"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/tracking"
)

func TestPreflightRejectsPredictablePreparationFailuresBeforeUpload(t *testing.T) {
	tests := []struct {
		name string
		edit func(*submission.Message)
	}{
		{"oversized encoded body", func(m *submission.Message) { m.Text = strings.Repeat("é", 1500000) }},
		{"oversized subject header", func(m *submission.Message) { m.Subject = strings.Repeat("x", 1000) }},
		{"too many tracked links", func(m *submission.Message) {
			m.HTML = strings.Repeat(`<a href="https://example.com">x</a>`, 101)
			m.Tracking = &submission.TrackingOptions{Clicks: true}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memoryRepository{}
			objects := &memoryObjects{}
			service := testService(t, repo, objects)
			req := validRequest()
			tc.edit(&req.Message)
			_, err := service.Accept(context.Background(), submission.App{ID: "app"}, req)
			if !errors.Is(err, submission.ErrInvalidMessage) || len(objects.objects) != 0 || len(repo.records) != 0 {
				t.Fatal("invalid message reached storage or commit", err)
			}
			if len(repo.reservations) != 1 || repo.reservations[0] != 1 {
				t.Fatal("preflight did not follow rate reservation")
			}
		})
	}
}

// The placeholder origin is an upper bound on any real, validated origin, so a
// message the preflight admits at its limit still builds under the MIME cap
// with a short origin and with a maximum-length one.
func TestPreflightIsUpperBoundForRealOrigins(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	maxOrigin := "https://" + strings.Repeat(strings.Repeat("a", 63)+".", 7) + strings.Repeat("a", 512-len("https://")-7*64)
	if got, err := tracking.NormalizeOrigin(maxOrigin); err != nil || len(got) != tracking.MaxOriginBytes {
		t.Fatal("test origin is not a maximum-length valid origin", err)
	}
	base := func(n int) submission.Message {
		m := validRequest().Message
		m.HTML = strings.Repeat("é", 1000) + manyLinks(100)
		m.Text = strings.Repeat("é", n)
		m.Tracking = &submission.TrackingOptions{Opens: true, Clicks: true}
		return m
	}
	low, high := 1, 4<<20
	for low < high {
		mid := (low + high + 1) / 2
		if preflight(base(mid), false) == nil {
			low = mid
		} else {
			high = mid - 1
		}
	}
	m := base(low)
	if preflight(m, false) != nil || preflight(base(low+1000), false) == nil && low+1000 < 4<<20 {
		t.Fatal("search did not find the preflight limit")
	}
	opts := message.Options{Recipient: m.To[0], ID: strings.Repeat("a", 32), Date: time.Now(), SigningDomain: "example.com", Selector: "mail", Signer: key}
	render := func(origin string, opts message.Options) []byte {
		t.Helper()
		html, mappings, err := tracking.Rewrite(m.HTML, tracking.Options{Links: preflightTrackingKeys, Origin: origin, Opens: true, Clicks: true})
		if err != nil || len(mappings) != 101 {
			t.Fatal("tracking rewrite failed", err)
		}
		real := m
		real.HTML = html
		final, err := message.Build(real, opts)
		if err != nil {
			t.Fatal(err)
		}
		return final
	}
	// The same signer, selector, ID and date for both renderings, so only the
	// origin differs: the preflight's placeholder must never be smaller.
	sizing := message.Options{Recipient: m.To[0], ID: strings.Repeat("a", 32), Date: time.Unix(0, 0).UTC(), SigningDomain: "example.com", Selector: strings.Repeat("a", 63), Signer: sizingSigner{}}
	placeholder := render("https://"+strings.Repeat("a", tracking.MaxOriginBytes-len("https://")), sizing)
	for _, origin := range []string{"https://t.example", "https://Track.Example.COM:8443", maxOrigin} {
		if final := render(origin, opts); len(final) > edgeprotocol.MaxMIMEBytes {
			t.Fatalf("preflight allowed MIME above delivery limit (%d bytes, origin %d)", len(final), len(origin))
		}
		if got := render(origin, sizing); len(got) > len(placeholder) {
			t.Fatalf("real origin rendered larger than the preflight placeholder (%d > %d, origin %d)", len(got), len(placeholder), len(origin))
		}
	}
}

// With no per-link reserve, a message with a large attachment fits click
// tracking nearly as well as it fits untracked.
func TestPreflightClickTrackingNeedsOnlyActualGrowth(t *testing.T) {
	fits := func(n int, tracked bool) bool {
		m := validRequest().Message
		m.HTML = `<a href="https://example.net/a">a</a><a href="https://example.net/b">b</a>`
		m.Text = strings.Repeat("x", n)
		m.Attachments = []submission.Attachment{{Filename: "r.pdf", ContentType: "application/pdf", Content: bytes.Repeat([]byte{1, 2, 3}, 300000)}}
		if tracked {
			m.Tracking = &submission.TrackingOptions{Opens: true, Clicks: true}
		}
		return preflight(m, false) == nil
	}
	limit := func(tracked bool) int {
		low, high := 1, 12<<20
		for low < high {
			mid := (low + high + 1) / 2
			if fits(mid, tracked) {
				low = mid
			} else {
				high = mid - 1
			}
		}
		return low
	}
	untracked, tracked := limit(false), limit(true)
	// The old reserve was 12288 bytes per rewritten link (three here).
	if gap := untracked - tracked; gap < 0 || gap > 4096 {
		t.Fatalf("click tracking costs %d bytes of headroom", gap)
	}
}

func TestBatchPreflightRejectsAllItemsBeforeAnyUpload(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	req := batchRequest()
	req.Messages[1].Subject = strings.Repeat("x", 1000)
	_, err := service.AcceptBatch(context.Background(), submission.App{ID: "app"}, req)
	if !errors.Is(err, submission.ErrInvalidMessage) || len(objects.objects) != 0 || repo.calls != 0 {
		t.Fatal("invalid batch item reached storage or commit", err)
	}
	if len(repo.reservations) != 1 || repo.reservations[0] != 2 {
		t.Fatal("batch preflight did not follow rate reservation")
	}
}

func TestPreflightAllowsTrackedMessageWithAttachments(t *testing.T) {
	m := validRequest().Message
	m.HTML = `<p><a href="https://example.net/order">order</a><img src="cid:logo"></p>`
	m.Tracking = &submission.TrackingOptions{Opens: true, Clicks: true}
	m.Attachments = []submission.Attachment{{Filename: "logo.png", ContentType: "image/png", Inline: true, ContentID: "logo", Content: []byte("synthetic")}}
	if err := preflight(m, false); err != nil {
		t.Fatal("valid tracked message rejected", err)
	}
}

// The one-click header Thinpost adds at preparation is measured by the
// preflight at its worst case, so an app with the toggle on is rejected at
// admission rather than failing later at preparation.
func TestPreflightReservesSpaceForOneClickHeader(t *testing.T) {
	firstRejected := func(listUnsubscribe bool) int {
		fits := func(n int) bool {
			m := validRequest().Message
			m.Text = strings.Repeat("x", n)
			return preflight(m, listUnsubscribe) == nil
		}
		low, high := 1, edgeprotocol.MaxMIMEBytes
		for low < high {
			mid := (low + high) / 2
			if fits(mid) {
				low = mid + 1
			} else {
				high = mid
			}
		}
		return low
	}
	without, with := firstRejected(false), firstRejected(true)
	// The header is a signed URL on a maximum-length origin: well over 600 bytes.
	if without-with < 600 || without-with > 1500 {
		t.Fatalf("one-click header reserved %d bytes", without-with)
	}
}

func TestAcceptRejectsAppHeadersWhenOneClickIsOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*submission.Message)
	}{
		{"list", func(m *submission.Message) { m.ListUnsubscribe = "<https://example.com/u>" }},
		{"list and post", func(m *submission.Message) {
			m.ListUnsubscribe, m.ListUnsubscribePost = "<https://example.com/u>", true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memoryRepository{}
			objects := &memoryObjects{}
			service := testService(t, repo, objects)
			req := validRequest()
			tc.edit(&req.Message)
			if _, err := service.Accept(context.Background(), submission.App{ID: "app", ListUnsubscribe: true}, req); !errors.Is(err, submission.ErrInvalidMessage) || len(objects.objects) != 0 || len(repo.records) != 0 || len(repo.reservations) != 0 {
				t.Fatal("conflicting headers were accepted or charged", err)
			}
			// The same request is accepted for an app without the toggle (U1 passthrough).
			if _, err := service.Accept(context.Background(), submission.App{ID: "app"}, req); err != nil {
				t.Fatal("passthrough rejected", err)
			}
		})
	}
	// A request without its own headers is fine with the toggle on.
	service := testService(t, &memoryRepository{}, &memoryObjects{})
	if _, err := service.Accept(context.Background(), submission.App{ID: "app", ListUnsubscribe: true}, validRequest()); err != nil {
		t.Fatal(err)
	}
}

func TestBatchRejectsAppHeadersWhenOneClickIsOn(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	req := batchRequest()
	req.Messages[1].ListUnsubscribe = "<https://example.com/u>"
	if _, err := service.AcceptBatch(context.Background(), submission.App{ID: "app", ListUnsubscribe: true}, req); !errors.Is(err, submission.ErrInvalidMessage) || len(objects.objects) != 0 || repo.calls != 0 || len(repo.reservations) != 0 {
		t.Fatal("conflicting batch item was accepted or charged", err)
	}
}

func TestPreflightListUnsubscribeV2FitsHeaderLimitAtMaxOrigin(t *testing.T) {
	origin := "https://" + strings.Repeat("a", tracking.MaxOriginBytes-len("https://"))
	link, err := preflightTrackingKeys.UnsubscribeLink(origin, strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	if line := "List-Unsubscribe: <" + link + ">"; len(line) > 998 {
		t.Fatal("List-Unsubscribe line exceeds 998 bytes", len(line))
	}
	if err := submission.ValidateListUnsubscribe("<"+link+">", true); err != nil {
		t.Fatal(err)
	}
	m := validRequest().Message
	if err := preflight(m, true); err != nil {
		t.Fatal("valid message with one-click header rejected", err)
	}
}

func TestPreflightMeasuresMaxLengthEncryptedClickLinks(t *testing.T) {
	m := validRequest().Message
	long := "https://example.com/" + strings.Repeat("x", 2048-len("https://example.com/"))
	m.HTML = strings.Repeat(`<a href="`+long+`">x</a>`, 100)
	m.Tracking = &submission.TrackingOptions{Opens: true, Clicks: true}
	err := preflight(m, true)
	if err != nil {
		t.Fatal(err)
	}
	m.HTML = strings.Repeat("é", 4_000_000) + m.HTML
	if err := preflight(m, true); err == nil {
		t.Fatal("oversized tracked message accepted")
	}
}

func attachmentMessage(size, htmlBytes int, tracked bool) submission.Message {
	m := validRequest().Message
	m.Text = "plain"
	m.HTML = `<p>` + strings.Repeat("x", htmlBytes) + `</p><a href="https://example.net/order">order</a>`
	m.Attachments = []submission.Attachment{{Filename: "big.bin", ContentType: "application/octet-stream", Content: bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, size/7+1)[:size]}}
	if tracked {
		m.Tracking = &submission.TrackingOptions{Opens: true, Clicks: true}
	}
	return m
}

// The default 5 MiB limit and the ceiling both fit the 8 MiB MIME limit with
// room for a realistic body, tracking and the one-click header.
func TestPreflightAcceptsDefaultAndCeilingAttachments(t *testing.T) {
	if err := preflight(attachmentMessage(submission.DefaultMaxAttachmentBytes, 1024, false), false); err != nil {
		t.Fatal("5 MiB attachment with small HTML rejected", err)
	}
	if err := preflight(attachmentMessage(submission.DefaultMaxAttachmentBytes, 512<<10, true), true); err != nil {
		t.Fatal("5 MiB attachment with 512 KiB tracked HTML rejected", err)
	}
	if err := preflight(attachmentMessage(submission.MaxAttachmentBytes, 256<<10, true), true); err != nil {
		t.Fatal("ceiling attachment with 256 KiB tracked HTML rejected", err)
	}
	// Documented boundary: the remaining room at the ceiling is under 485 KiB,
	// so a body beyond that is still rejected visibly.
	if err := preflight(attachmentMessage(submission.MaxAttachmentBytes, 600<<10, false), false); err == nil {
		t.Fatal("ceiling attachment with 600 KiB HTML unexpectedly fits")
	}
}

// Preflight renders the real message, so a maximum References chain (about 8 KiB
// plus folding and a longer DKIM h= list) shrinks the admissible body.
func TestPreflightCountsThreadingHeaders(t *testing.T) {
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = "<" + strings.Repeat("r", 60) + "@example.com>"
	}
	references, err := submission.NormalizeReferences(strings.Join(ids, " "))
	if err != nil {
		t.Fatal(err)
	}
	firstRejected := func(threaded bool) int {
		fits := func(n int) bool {
			m := validRequest().Message
			m.Text = strings.Repeat("x", n)
			if threaded {
				m.InReplyTo, m.References = ids[0], references
			}
			return preflight(m, false) == nil
		}
		low, high := 1, edgeprotocol.MaxMIMEBytes
		for low < high {
			mid := (low + high) / 2
			if fits(mid) {
				low = mid + 1
			} else {
				high = mid
			}
		}
		return low
	}
	without, with := firstRejected(false), firstRejected(true)
	if without-with < len(references) {
		t.Fatalf("threading headers reserved only %d bytes for %d", without-with, len(references))
	}
}
