package smtpintake

import (
	"context"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/submission"
)

type captureObjects struct{ bodies [][]byte }

func (o *captureObjects) Put(_ context.Context, _ string, body []byte) error {
	o.bodies = append(o.bodies, body)
	return nil
}

func trackingRig(t *testing.T, defaults, runtime bool) (*rig, *captureObjects) {
	t.Helper()
	r := newRig(t)
	objects := &captureObjects{}
	accept, err := acceptance.New(r.repo, objects, []byte("0123456789abcdef0123456789abcdef"), allow(true))
	if err != nil {
		t.Fatal(err)
	}
	accept.ApplyTrackingDefaults = runtime
	r.service.Acceptance = accept
	r.repo.policy.DefaultTrackOpens, r.repo.policy.DefaultTrackClicks = defaults, defaults
	return r, objects
}

const htmlOnly = "From: sender@example.com\r\nTo: rcpt@example.net\r\nMIME-Version: 1.0\r\nContent-Type: text/html\r\nMessage-ID: <t@client.example.org>\r\n"

func storedChoice(t *testing.T, objects *captureObjects) (opens, clicks bool) {
	t.Helper()
	if len(objects.bodies) != 1 {
		t.Fatalf("%d sources stored", len(objects.bodies))
	}
	source, err := submission.DecodeSource(objects.bodies[0], false)
	if err != nil {
		t.Fatal(err)
	}
	return source.RawHeader.TrackOpens, source.RawHeader.TrackClicks
}

func TestSubmitRejectsInvalidTrackingHeaders(t *testing.T) {
	for name, headers := range map[string]string{
		"maybe":          "X-Thinpost-Track-Opens: maybe\r\n",
		"empty":          "X-Thinpost-Track-Clicks:\r\n",
		"twice":          "X-Thinpost-Track-Opens: true\r\nX-Thinpost-Track-Opens: false\r\n",
		"one is invalid": "X-Thinpost-Track-Opens: true\r\nX-Thinpost-Track-Clicks: 1\r\n",
	} {
		r, objects := trackingRig(t, true, true)
		if got := r.submit(htmlOnly + headers + "\r\n<a href=\"https://a.example.org/\">x</a>\r\n"); got.Outcome != MessageInvalid || len(objects.bodies) != 0 {
			t.Errorf("%s: %v", name, got.Outcome)
		}
	}
	// Valid spellings are case-insensitive.
	r, objects := trackingRig(t, false, true)
	if got := r.submit(htmlOnly + "X-Thinpost-Track-Opens: TRUE\r\nx-thinpost-track-clicks: False\r\n\r\n<a href=\"https://a.example.org/\">x</a>\r\n"); got.Outcome != Accepted {
		t.Fatal(got.Outcome)
	}
	if opens, clicks := storedChoice(t, objects); !opens || clicks {
		t.Fatal(opens, clicks)
	}
}

func TestSubmitAppliesTheAppsTrackingDefaults(t *testing.T) {
	body := "\r\n<a href=\"https://a.example.org/\">x</a>\r\n"
	for name, tc := range map[string]struct {
		defaults, runtime bool
		headers           string
		opens, clicks     bool
	}{
		"defaults":               {true, true, "", true, true},
		"no runtime":             {true, false, "", false, false},
		"explicit off":           {true, true, "X-Thinpost-Track-Opens: false\r\nX-Thinpost-Track-Clicks: false\r\n", false, false},
		"explicit on":            {false, true, "X-Thinpost-Track-Clicks: true\r\n", false, true},
		"explicit needs runtime": {false, false, "X-Thinpost-Track-Clicks: true\r\n", false, false},
	} {
		t.Run(name, func(t *testing.T) {
			r, objects := trackingRig(t, tc.defaults, tc.runtime)
			if got := r.submit(htmlOnly + tc.headers + body); got.Outcome != Accepted {
				t.Fatal(got.Outcome)
			}
			if opens, clicks := storedChoice(t, objects); opens != tc.opens || clicks != tc.clicks {
				t.Fatalf("opens %v clicks %v", opens, clicks)
			}
		})
	}
}

// A retry of an accepted message gets the original answer, whatever the
// tracking settings have become since, and nothing is stored again.
func TestSubmitReplayReturnsTheOriginalMessage(t *testing.T) {
	r, objects := trackingRig(t, true, true)
	raw := htmlOnly + "X-Thinpost-Track-Opens: true\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\n\r\n<a href=\"https://a.example.org/\">x</a>\r\n"
	first := r.submit(raw)
	r.repo.policy.DefaultTrackOpens, r.repo.policy.DefaultTrackClicks = false, false
	second := r.submit(raw)
	if first.Outcome != Accepted || second.Outcome != Accepted || first.MessageID != second.MessageID || len(objects.bodies) != 1 || r.repo.commitCount != 1 {
		t.Fatalf("%+v %+v", first, second)
	}
}

// A message that an older pod accepted with a junk tracking header (which it
// did not judge) is answered from the replay record on retry; only a fresh
// submission with that header is refused.
func TestSubmitReplayPrecedesTrackingHeaderValidation(t *testing.T) {
	r, objects := trackingRig(t, true, true)
	raw := htmlOnly + "X-Thinpost-Track-Opens: junk\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\n\r\n<a href=\"https://a.example.org/\">x</a>\r\n"
	if got := r.submit(raw); got.Outcome != MessageInvalid {
		t.Fatalf("fresh: %v", got.Outcome)
	}
	scanned, err := rawmime.Scan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := RecipientKeys([]string{"rcpt@example.net"})
	kind := ReplayKindOf(scanned)
	digest := Digest([]byte("0123456789abcdef0123456789abcdef"), kind, appID, "sender@example.com", keys, scanned.MessageID, []byte(raw))
	r.repo.replays[replayKey(kind, digest)] = submission.Receipt{ID: strings.Repeat("7", 32)}
	got := r.submit(raw)
	if got.Outcome != Accepted || got.MessageID != strings.Repeat("7", 32) || len(objects.bodies) != 0 {
		t.Fatalf("retry: %+v", got)
	}
}
