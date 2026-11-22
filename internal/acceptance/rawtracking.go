package acceptance

import (
	"strings"

	"github.com/zyno-io/thinpost/internal/rawtracking"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/tracking"
)

// rawTracking is the tracking decision for an SMTP message: the kinds that will
// apply, stored in the source for preparation, and why less than was asked for
// applies, if so.
type rawTracking struct {
	Opens, Clicks bool
	// Skip is the reason a requested kind is withdrawn, "" when none is. Report
	// says it is worth an operator log line: the message asked for tracking in a
	// header, or a default met a problem beyond "there is no HTML to track".
	Skip   rawtracking.Reason
	Report bool
}

// resolveRawTracking decides open and click tracking for an SMTP submission.
// An explicit X-Thinpost-Track-* value wins over the app's default; a default,
// and an explicit true, need the tracking runtime (s.ApplyTrackingDefaults).
// Mail is never refused for tracking: what cannot be honored, or does not fit
// with tracking in place, is withdrawn, clicks first, then opens. fits judges a
// planned message against the size limits (one-click headers included).
func (s *Service) resolveRawTracking(req RawRequest, app submission.App, fits func(rawtracking.Result) (bool, error)) (rawTracking, error) {
	opens, explicitOpens := req.wanted(req.TrackOpens, app.DefaultTrackOpens, s.ApplyTrackingDefaults)
	clicks, explicitClicks := req.wanted(req.TrackClicks, app.DefaultTrackClicks, s.ApplyTrackingDefaults)
	explicit := explicitOpens && opens || explicitClicks && clicks
	if req.TrackOpens != nil && *req.TrackOpens && !s.ApplyTrackingDefaults || req.TrackClicks != nil && *req.TrackClicks && !s.ApplyTrackingDefaults {
		return rawTracking{Skip: rawtracking.ReasonNotConfigured, Report: true}, nil
	}
	if !opens && !clicks {
		return rawTracking{}, nil
	}
	res, reason, err := rawtracking.Choose(req.Raw, req.Scan, rawtracking.Options{
		Opens: opens, Clicks: clicks, Origin: sizingTrackingOrigin(), Links: preflightTrackingKeys,
	}, fits)
	if err != nil {
		return rawTracking{}, err
	}
	report := reason != rawtracking.ReasonNone &&
		(explicit || reason != rawtracking.ReasonNoHTML && reason != rawtracking.ReasonNotMIME)
	return rawTracking{Opens: res.Opens, Clicks: res.Clicks, Skip: reason, Report: report}, nil
}

// wanted is the kind's effective request: an explicit header value wins, else
// the app's default when defaults apply. The bool says the request was explicit.
func (RawRequest) wanted(explicit *bool, appDefault, defaultsApply bool) (want, wasExplicit bool) {
	if explicit != nil {
		return *explicit, true
	}
	return appDefault && defaultsApply, false
}

// sizingTrackingOrigin is the longest origin tracking can use.
func sizingTrackingOrigin() string {
	return "https://" + strings.Repeat("a", tracking.MaxOriginBytes-len("https://"))
}
