package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

func trackingOpts(spec string) *submission.TrackingOptions {
	if spec == "" {
		return nil
	}
	return &submission.TrackingOptions{Opens: strings.Contains(spec, "o"), Clicks: strings.Contains(spec, "c")}
}

func manyLinks(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `<a href="https://example.net/%d">l</a>`, i)
	}
	return b.String()
}

func TestResolveTrackingMatrix(t *testing.T) {
	// explicit on / explicit off use "o", "c", "oc"; "" means unset.
	for _, tc := range []struct {
		name, on, off string
		app           submission.App
		html          bool
		apply         bool
		want          string
	}{
		{"nothing", "", "", submission.App{}, true, true, ""},
		{"default opens", "", "", submission.App{DefaultTrackOpens: true}, true, true, "o"},
		{"default both", "", "", submission.App{DefaultTrackOpens: true, DefaultTrackClicks: true}, true, true, "oc"},
		{"default needs HTML", "", "", submission.App{DefaultTrackOpens: true, DefaultTrackClicks: true}, false, true, ""},
		{"default needs runtime", "", "", submission.App{DefaultTrackOpens: true, DefaultTrackClicks: true}, true, false, ""},
		{"explicit off wins", "", "oc", submission.App{DefaultTrackOpens: true, DefaultTrackClicks: true}, true, true, ""},
		{"explicit off one kind", "", "c", submission.App{DefaultTrackOpens: true, DefaultTrackClicks: true}, true, true, "o"},
		{"explicit on with default off", "o", "", submission.App{}, true, true, "o"},
		{"explicit on plus other default", "o", "", submission.App{DefaultTrackClicks: true}, true, true, "oc"},
		{"explicit on, other off, default on", "o", "c", submission.App{DefaultTrackOpens: true, DefaultTrackClicks: true}, true, true, "o"},
		{"explicit on without defaults applied", "c", "", submission.App{DefaultTrackOpens: true}, true, false, "c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Text: "t", Tracking: trackingOpts(tc.on), TrackingOff: trackingOpts(tc.off)}
			if tc.html {
				m.HTML = `<a href="https://example.net">x</a>`
			}
			got, _ := resolveTracking(m, tc.app, tc.apply, tc.apply)
			if specOf(got.Tracking) != tc.want || got.TrackingOff != nil {
				t.Fatalf("tracking %q off %v", specOf(got.Tracking), got.TrackingOff)
			}
		})
	}
}

func TestResolveTrackingDoesNotAliasOrMutate(t *testing.T) {
	shared := &submission.TrackingOptions{Opens: true}
	off := &submission.TrackingOptions{Clicks: true}
	m := submission.Message{HTML: "<b>x</b>", Tracking: shared, TrackingOff: off}
	got, _ := resolveTracking(m, submission.App{DefaultTrackOpens: true, DefaultTrackClicks: true}, true, true)
	if got.Tracking == shared || *shared != (submission.TrackingOptions{Opens: true}) || *off != (submission.TrackingOptions{Clicks: true}) || m.TrackingOff != off {
		t.Fatal("caller's options were aliased or mutated")
	}
}

func storedSource(t *testing.T, objects *memoryObjects, key string) (submission.Message, []byte) {
	t.Helper()
	raw := objects.objects[key]
	var source struct {
		Version int
		Message submission.Message
	}
	if err := json.Unmarshal(raw, &source); err != nil || source.Version != submission.SourceVersion {
		t.Fatal("bad stored object", err)
	}
	return source.Message, raw
}

func htmlRequest(html string) submission.Request {
	req := validRequest()
	req.Message.HTML = html
	return req
}

func TestAcceptStoresResolvedTrackingNeverTrackingOff(t *testing.T) {
	ctx := context.Background()
	app := submission.App{ID: "app", DefaultTrackOpens: true, DefaultTrackClicks: true}
	for _, tc := range []struct {
		name  string
		apply bool
		mut   func(*submission.Request)
		want  string
	}{
		{"default applied", true, func(*submission.Request) {}, "oc"},
		{"defaults inert without runtime", false, func(*submission.Request) {}, ""},
		{"explicit off wins", true, func(r *submission.Request) {
			r.Message.TrackingOff = &submission.TrackingOptions{Opens: true, Clicks: true}
		}, ""},
		{"explicit mixed", true, func(r *submission.Request) {
			r.Message.Tracking = &submission.TrackingOptions{Opens: true}
			r.Message.TrackingOff = &submission.TrackingOptions{Clicks: true}
		}, "o"},
		{"text only is untracked", true, func(r *submission.Request) { r.Message.HTML = "" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, objects := &memoryRepository{}, &memoryObjects{}
			service := testService(t, repo, objects)
			service.ApplyTrackingDefaults = tc.apply
			req := htmlRequest(`<a href="https://example.net">x</a>`)
			tc.mut(&req)
			if _, err := service.Accept(ctx, app, req); err != nil {
				t.Fatal(err)
			}
			stored, raw := storedSource(t, objects, repo.records[0].ObjectKey)
			if specOf(stored.Tracking) != tc.want || bytes.Contains(raw, []byte("TrackingOff")) {
				t.Fatalf("stored tracking %q (TrackingOff present: %v)", specOf(stored.Tracking), bytes.Contains(raw, []byte("TrackingOff")))
			}
		})
	}
}

func TestTrackingFingerprintIgnoresDefaultsButNotExplicitOff(t *testing.T) {
	ctx := context.Background()
	repo, objects := &memoryRepository{}, &memoryObjects{}
	service := testService(t, repo, objects)
	service.ApplyTrackingDefaults = true
	req := htmlRequest("<b>x</b>")
	first, err := service.Accept(ctx, submission.App{ID: "app"}, req)
	if err != nil {
		t.Fatal(err)
	}
	// The default changed between the original and the retry: still a replay.
	again, err := service.Accept(ctx, submission.App{ID: "app", DefaultTrackOpens: true}, req)
	if err != nil || !again.Replayed || again.ID != first.ID {
		t.Fatal("retry after a default change did not replay", err)
	}
	// An explicit off is a different request.
	req.Message.TrackingOff = &submission.TrackingOptions{Opens: true}
	if _, err = service.Accept(ctx, submission.App{ID: "app"}, req); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("explicit off did not conflict with absent", err)
	}
}

func TestDefaultsNeverRejectButExplicitDoes(t *testing.T) {
	ctx := context.Background()
	html := manyLinks(101)
	app := submission.App{ID: "app", DefaultTrackClicks: true}
	repo, objects := &memoryRepository{}, &memoryObjects{}
	service := testService(t, repo, objects)
	service.ApplyTrackingDefaults = true
	// Default clicks on 101 links: accepted untracked.
	if _, err := service.Accept(ctx, app, htmlRequest(html)); err != nil {
		t.Fatal("default rejected a message", err)
	}
	if stored, _ := storedSource(t, objects, repo.records[0].ObjectKey); stored.Tracking != nil {
		t.Fatal("unrewritable message stored as tracked")
	}
	// Explicit clicks keep today's rejection.
	explicit := htmlRequest(html)
	explicit.IdempotencyKey = "other"
	explicit.Message.Tracking = &submission.TrackingOptions{Clicks: true}
	if _, err := service.Accept(ctx, app, explicit); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("explicit clicks on 101 links accepted", err)
	}
	// Explicit opens plus default clicks: opens only.
	mixed := htmlRequest(html)
	mixed.IdempotencyKey = "mixed"
	mixed.Message.Tracking = &submission.TrackingOptions{Opens: true}
	if _, err := service.Accept(ctx, app, mixed); err != nil {
		t.Fatal("explicit opens + default clicks rejected", err)
	}
	if stored, _ := storedSource(t, objects, repo.records[1].ObjectKey); specOf(stored.Tracking) != "o" {
		t.Fatalf("stored %q, want opens only", specOf(stored.Tracking))
	}
}

func TestBatchResolvesDefaultsPerItemWithoutMutatingSharedOptions(t *testing.T) {
	ctx := context.Background()
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	service.ApplyTrackingDefaults = true
	shared := &submission.TrackingOptions{Opens: true}
	off := &submission.TrackingOptions{Clicks: true}
	base := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Tracking: shared, TrackingOff: off, HTML: "<b>x</b>"}
	textOnly := base
	textOnly.HTML, textOnly.Text, textOnly.Tracking, textOnly.TrackingOff = "", "plain", nil, nil
	plain := base
	plain.Tracking, plain.TrackingOff = nil, nil
	app := submission.App{ID: "app", DefaultTrackOpens: true, DefaultTrackClicks: true}
	if _, err := service.AcceptBatch(ctx, app, submission.BatchRequest{Adapter: "sendgrid", Messages: []submission.Message{base, textOnly, plain}}); err != nil {
		t.Fatal(err)
	}
	if *shared != (submission.TrackingOptions{Opens: true}) || *off != (submission.TrackingOptions{Clicks: true}) {
		t.Fatal("shared options mutated")
	}
	for i, want := range []string{"o", "", "oc"} {
		stored, raw := storedSource(t, objects, repo.last.Items[i].ObjectKey)
		if specOf(stored.Tracking) != want || bytes.Contains(raw, []byte("TrackingOff")) {
			t.Fatalf("item %d stored %q", i, specOf(stored.Tracking))
		}
	}
}

func TestBatchDefaultsNeverRejectUnrewritableItem(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	service.ApplyTrackingDefaults = true
	item := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", HTML: manyLinks(101)}
	app := submission.App{ID: "app", DefaultTrackClicks: true}
	if _, err := service.AcceptBatch(context.Background(), app, submission.BatchRequest{Adapter: "resend", Messages: []submission.Message{item}}); err != nil {
		t.Fatal("default rejected a batch", err)
	}
	if stored, _ := storedSource(t, objects, repo.last.Items[0].ObjectKey); stored.Tracking != nil {
		t.Fatal("unrewritable item stored as tracked")
	}
}

// paddedMessage is an HTML message with a few links whose plain-text part is
// padded to n bytes, so n tunes how close the message sits to the MIME cap.
func paddedMessage(n int) submission.Message {
	return submission.Message{
		From: "a@example.com", To: []string{"b@example.net"}, Subject: "s",
		HTML: manyLinks(3), Text: strings.Repeat("x", n),
		Attachments: []submission.Attachment{{Filename: "report.pdf", ContentType: "application/pdf", Content: bytes.Repeat([]byte{0xAB, 0x01, 0xFE}, 300000)}},
	}
}

// largestPadding returns the largest padding for which the message still
// passes the preflight with the given explicit tracking spec.
func largestPadding(t *testing.T, spec string) int {
	t.Helper()
	fits := func(n int) bool {
		m := paddedMessage(n)
		m.Tracking = trackingOpts(spec)
		return preflight(m, false) == nil
	}
	low, high := 1, 12<<20
	if !fits(low) {
		t.Fatalf("spec %q does not fit even a tiny message", spec)
	}
	for low < high {
		mid := (low + high + 1) / 2
		if fits(mid) {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return low
}

func preflightsWith(m submission.Message, spec string) bool {
	m.Tracking = trackingOpts(spec)
	return preflight(m, false) == nil
}

func TestDefaultsFallBackToOpensWhenClicksCannotFit(t *testing.T) {
	ctx := context.Background()
	app := submission.App{ID: "app", DefaultTrackOpens: true, DefaultTrackClicks: true}
	untracked, opens, clicks := largestPadding(t, ""), largestPadding(t, "o"), largestPadding(t, "oc")
	if !(clicks < opens && opens < untracked) {
		t.Fatalf("expected a size window between kinds, got untracked=%d opens=%d clicks=%d", untracked, opens, clicks)
	}
	for _, tc := range []struct {
		name string
		msg  submission.Message
		want string
		pre  func(m submission.Message) // asserts the case really exercises the fallback
	}{
		{"too many links", func() submission.Message {
			m := paddedMessage(10)
			m.HTML = manyLinks(101)
			return m
		}(), "o", func(m submission.Message) {
			if preflightsWith(m, "oc") || preflightsWith(m, "c") || !preflightsWith(m, "o") {
				t.Fatal("precondition: 101 links must fail with clicks and pass with opens only")
			}
		}},
		{"clicks reservation does not fit", paddedMessage(opens), "o", func(m submission.Message) {
			if preflightsWith(m, "oc") || !preflightsWith(m, "o") || !preflightsWith(m, "") {
				t.Fatal("precondition: message must fail with clicks and pass with opens only")
			}
		}},
		// The opens pixel adds a few hundred bytes, so a narrow window exists
		// between the untracked and opens-only limits.
		{"nothing tracked fits", paddedMessage(untracked), "", func(m submission.Message) {
			if preflightsWith(m, "o") || preflightsWith(m, "oc") || !preflightsWith(m, "") {
				t.Fatal("precondition: message must fit only untracked")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.pre(tc.msg)
			repo, objects := &memoryRepository{}, &memoryObjects{}
			service := testService(t, repo, objects)
			service.ApplyTrackingDefaults = true
			req := validRequest()
			req.Message = tc.msg
			if _, err := service.Accept(ctx, app, req); err != nil {
				t.Fatal("default rejected a message", err)
			}
			stored, _ := storedSource(t, objects, repo.records[0].ObjectKey)
			if specOf(stored.Tracking) != tc.want {
				t.Fatalf("stored tracking %q, want %q", specOf(stored.Tracking), tc.want)
			}
			if tc.want == "" && stored.Tracking != nil {
				t.Fatal("untracked message carries tracking options")
			}
		})
	}
}

func TestExplicitChoicesStandWhenDefaultsFallBack(t *testing.T) {
	ctx := context.Background()
	app := submission.App{ID: "app", DefaultTrackOpens: true, DefaultTrackClicks: true}
	tooMany := manyLinks(101)
	t.Run("explicit clicks still rejected", func(t *testing.T) {
		repo, objects := &memoryRepository{}, &memoryObjects{}
		service := testService(t, repo, objects)
		service.ApplyTrackingDefaults = true
		req := htmlRequest(tooMany)
		req.Message.Tracking = &submission.TrackingOptions{Clicks: true}
		if _, err := service.Accept(ctx, app, req); !errors.Is(err, submission.ErrInvalidMessage) {
			t.Fatal("explicit clicks plus default opens was accepted", err)
		}
		if len(repo.records) != 0 || len(objects.objects) != 0 {
			t.Fatal("rejected message reached storage")
		}
	})
	t.Run("explicit opens off is never overridden", func(t *testing.T) {
		repo, objects := &memoryRepository{}, &memoryObjects{}
		service := testService(t, repo, objects)
		service.ApplyTrackingDefaults = true
		req := htmlRequest(tooMany)
		req.Message.TrackingOff = &submission.TrackingOptions{Opens: true}
		if _, err := service.Accept(ctx, app, req); err != nil {
			t.Fatal("default rejected a message", err)
		}
		if stored, _ := storedSource(t, objects, repo.records[0].ObjectKey); stored.Tracking != nil {
			t.Fatalf("stored tracking %q, want untracked", specOf(stored.Tracking))
		}
	})
}

// A batch's 2 MiB source cap sits well under the 8 MiB MIME cap, so only the
// link-count limit can force the fallback inside a batch.
func TestBatchDefaultsFallBackPerItem(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	service.ApplyTrackingDefaults = true
	fits := paddedMessage(10)
	fits.Attachments = nil
	tooMany := fits
	tooMany.HTML = manyLinks(101)
	if preflightsWith(tooMany, "oc") || !preflightsWith(tooMany, "o") {
		t.Fatal("precondition: 101 links must fail with clicks and pass with opens only")
	}
	app := submission.App{ID: "app", DefaultTrackOpens: true, DefaultTrackClicks: true}
	msgs := []submission.Message{fits, tooMany, fits}
	if _, err := service.AcceptBatch(context.Background(), app, submission.BatchRequest{Adapter: "resend", Messages: msgs}); err != nil {
		t.Fatal("default rejected a batch", err)
	}
	for i, want := range []string{"oc", "o", "oc"} {
		if stored, _ := storedSource(t, objects, repo.last.Items[i].ObjectKey); specOf(stored.Tracking) != want {
			t.Fatalf("item %d stored %q, want %q", i, specOf(stored.Tracking), want)
		}
	}
}
