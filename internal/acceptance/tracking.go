package acceptance

import "github.com/zyno-io/thinpost/internal/submission"

// resolveTracking applies the app's tracking defaults to a request: an
// explicit choice (on in Tracking, off in TrackingOff) always wins, and a
// default fills only a kind the request left unset, only for an HTML message,
// and only for a kind whose flag (opens, clicks) is true. The result never
// carries TrackingOff and never aliases m.Tracking (SendGrid personalizations
// share those pointers). The bool reports whether a default changed the
// effective tracking.
func resolveTracking(m submission.Message, app submission.App, opens, clicks bool) (submission.Message, bool) {
	var explicit, off submission.TrackingOptions
	if m.Tracking != nil {
		explicit = *m.Tracking
	}
	if m.TrackingOff != nil {
		off = *m.TrackingOff
	}
	effective := explicit
	if m.HTML != "" {
		effective.Opens = effective.Opens || opens && app.DefaultTrackOpens && !off.Opens
		effective.Clicks = effective.Clicks || clicks && app.DefaultTrackClicks && !off.Clicks
	}
	out := m
	out.Tracking, out.TrackingOff = nil, nil
	if effective.Opens || effective.Clicks {
		out.Tracking = &effective
	}
	return out, effective != explicit
}

// resolve resolves tracking and runs the size and rewrite preflight. A default
// never rejects a message: when the preflight fails for a message that a
// default changed, the clicks default is withdrawn first (the opens default
// stays), then the opens default, leaving only the explicit choices (untracked
// if it has none). Explicit opt-ins keep today's rejection.
func (s *Service) resolve(m submission.Message, app submission.App) (submission.Message, error) {
	on := s.ApplyTrackingDefaults
	resolved, defaulted := resolveTracking(m, app, on, on)
	err := preflight(resolved, app.ListUnsubscribe)
	if err != nil && defaulted {
		opensOnly, _ := resolveTracking(m, app, on, false)
		explicitOnly, _ := resolveTracking(m, app, false, false)
		for _, candidate := range []submission.Message{opensOnly, explicitOnly} {
			if specOf(candidate.Tracking) == specOf(resolved.Tracking) {
				continue
			}
			resolved = candidate
			if err = preflight(resolved, app.ListUnsubscribe); err == nil {
				break
			}
		}
	}
	if err == nil && resolved.Validate() != nil {
		err = submission.ErrInvalidMessage
	}
	return resolved, err
}

func specOf(t *submission.TrackingOptions) string {
	if t == nil {
		return ""
	}
	out := ""
	if t.Opens {
		out += "o"
	}
	if t.Clicks {
		out += "c"
	}
	return out
}
