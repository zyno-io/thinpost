// Package rawtracking plans open and click tracking for SMTP mail: it rewrites
// the eligible text/html parts of a raw MIME message (rawmime.HTMLParts) with
// the unchanged tracking.Rewrite, the same function HTTP mail uses, and returns
// the result as byte splices over the source for rawmime.Prepare and the
// prepared recipe. Acceptance runs it with sizing link keys to measure what
// fits; preparation runs it with real link keys, once per recipient, because
// tracking tokens are per recipient.
//
// Tracking is never a reason to refuse mail. Anything that cannot be honored is
// reported as a constant Reason and the message goes out with less tracking:
// Choose drops click tracking first, then open tracking.
package rawtracking

import (
	"errors"
	"sort"

	preparedrecipe "github.com/zyno-io/thinpost/internal/prepared"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/tracking"
)

// Reason says why a requested kind of tracking was not applied. The values are
// constants safe for the operator log (oplog.TrackingSkipped); none quotes
// content. The first group is rawmime.Reason, passed through.
type Reason string

const (
	ReasonNone                Reason = ""
	ReasonNotMIME             Reason = Reason(rawmime.ReasonNotMIME)
	ReasonSignedOrEncrypted   Reason = Reason(rawmime.ReasonSignedOrEncrypted)
	ReasonMalformed           Reason = Reason(rawmime.ReasonMalformed)
	ReasonNoHTML              Reason = Reason(rawmime.ReasonNoHTML)
	ReasonUnsupportedEncoding Reason = Reason(rawmime.ReasonUnsupportedEncoding)
	ReasonUnsupportedCharset  Reason = Reason(rawmime.ReasonUnsupportedCharset)
	ReasonTooLarge            Reason = Reason(rawmime.ReasonTooLarge)
	ReasonTooManyParts        Reason = Reason(rawmime.ReasonTooManyParts)
	// ReasonTooManyLinks: more than MaxClickLinks trackable links in all parts.
	ReasonTooManyLinks Reason = "too_many_links"
	// ReasonRewriteFailed: tracking.Rewrite refused a part (a link too long, a
	// document too complex, ...).
	ReasonRewriteFailed Reason = "rewrite_failed"
	// ReasonMessageTooLarge: the tracked message does not fit the size limit.
	ReasonMessageTooLarge Reason = "message_too_large"
	// ReasonNotConfigured: the deployment has no tracking runtime.
	ReasonNotConfigured Reason = "not_configured"
)

// MaxClickLinks is the most click links one message can carry: the mapping
// object of tracking.ProtectedStore holds at most 100 click links and one
// open pixel.
const MaxClickLinks = 100

// Error is returned by Plan when a requested kind cannot be applied.
type Error struct{ Reason Reason }

func (e *Error) Error() string { return "tracking not applied: " + string(e.Reason) }

// ErrInvalid is returned for options that cannot be planned.
var ErrInvalid = errors.New("invalid tracking plan options")

// Options selects what to plan.
type Options struct {
	Opens, Clicks bool
	// Origin is the tracking origin (tracking.NormalizeOrigin form) of the links
	// and the pixel.
	Origin string
	// Links seals the links. Acceptance passes sizing keys, preparation the
	// deployment's.
	Links *tracking.LinkKeys
}

// Result is a plan. Opens and Clicks are the kinds in effect: Clicks is true for
// a message with no trackable link, too (nothing to rewrite, nothing failed).
// With no Mappings the message is unchanged and Body is empty.
type Result struct {
	// Body are the splices over the source, in source order; TopHTML the new
	// top-level fields of a single-part HTML message.
	Body    []rawmime.Splice
	TopHTML *rawmime.TopLevelHTML
	// Mappings are this plan's tokens and destinations, for the mapping object.
	Mappings      []tracking.Mapping
	Opens, Clicks bool
}

// Plan rewrites the message's eligible HTML parts with tracking.Rewrite. The
// open pixel goes in the first eligible part only, click links in all of them.
// A part with nothing to rewrite is left as it was. It returns an *Error when
// the requested kinds cannot be applied, and ErrInvalid for unusable options.
func Plan(raw []byte, scan *rawmime.Scanned, o Options) (Result, error) {
	if !o.Opens && !o.Clicks {
		return Result{}, nil
	}
	if scan == nil || o.Links == nil {
		return Result{}, ErrInvalid
	}
	parts, reason := rawmime.HTMLParts(raw, scan)
	if reason != rawmime.ReasonNone {
		return Result{}, &Error{Reason(reason)}
	}
	res := Result{Opens: o.Opens, Clicks: o.Clicks}
	clicks, first := 0, true
	for _, part := range parts {
		if part.HTML == "" {
			continue // Rewrite takes no empty document, and there is nothing to track
		}
		html, mappings, err := tracking.Rewrite(part.HTML, tracking.Options{
			Origin: o.Origin, Links: o.Links, Opens: o.Opens && first, Clicks: o.Clicks,
		})
		first = false
		if errors.Is(err, tracking.ErrTooManyLinks) {
			return Result{}, &Error{ReasonTooManyLinks}
		}
		if err != nil {
			return Result{}, &Error{ReasonRewriteFailed}
		}
		if len(mappings) == 0 {
			continue
		}
		for _, m := range mappings {
			if m.Kind == "click" {
				clicks++
			}
		}
		if clicks > MaxClickLinks {
			return Result{}, &Error{ReasonTooManyLinks}
		}
		splices, top, err := part.Replace(html)
		if err != nil {
			return Result{}, &Error{ReasonRewriteFailed}
		}
		res.Body = append(res.Body, splices...)
		res.Mappings = append(res.Mappings, mappings...)
		if top != nil {
			res.TopHTML = top
		}
	}
	if o.Opens && first {
		return Result{}, &Error{ReasonNoHTML}
	}
	sort.SliceStable(res.Body, func(i, j int) bool { return res.Body[i].Start < res.Body[j].Start })
	for i := 1; i < len(res.Body); i++ {
		if res.Body[i].Start < res.Body[i-1].End {
			return Result{}, &Error{ReasonRewriteFailed}
		}
	}
	if len(res.Body) > preparedrecipe.MaxSplices {
		return Result{}, &Error{ReasonTooManyParts}
	}
	if len(res.Mappings) == 0 {
		return Result{Opens: o.Opens, Clicks: o.Clicks}, nil
	}
	return res, nil
}

// Choose plans the most tracking that applies and fits. It tries what was
// asked for, then without click tracking, then without open tracking, and
// returns the first plan that Plan accepts and fits approves (nil fits
// approves everything). Reason says why the result has less than was asked for,
// the first obstacle met; it is empty when everything asked for applies. The
// result is empty (no kinds) when nothing applies. A non-nil error is a failure
// of fits.
func Choose(raw []byte, scan *rawmime.Scanned, want Options, fits func(Result) (bool, error)) (Result, Reason, error) {
	candidates := []Options{want}
	if want.Opens && want.Clicks {
		candidates = append(candidates, Options{Opens: true, Origin: want.Origin, Links: want.Links})
	}
	var reason Reason
	for _, c := range candidates {
		res, err := Plan(raw, scan, c)
		var planErr *Error
		switch {
		case errors.As(err, &planErr):
			if reason == ReasonNone {
				reason = planErr.Reason
			}
			continue
		case err != nil:
			return Result{}, reason, err
		}
		if fits != nil {
			ok, err := fits(res)
			if err != nil {
				return Result{}, reason, err
			}
			if !ok {
				if reason == ReasonNone {
					reason = ReasonMessageTooLarge
				}
				continue
			}
		}
		return res, reason, nil
	}
	return Result{}, reason, nil
}
