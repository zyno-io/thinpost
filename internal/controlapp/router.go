package controlapp

import (
	"log"
	"net/http"
	"path"
	"strings"
)

// MailRuntime supplies explicitly configured mail admission and tracking routes.
// Admission readiness is independent of a worker's current SMTP connection:
// submissions can remain durably queued while the worker restarts.
type MailRuntime struct {
	Ready    func() bool
	Tracking http.Handler
	// App serves the app-key read routes under /app/v1/. Like the submission
	// routes it is served only when submission is configured.
	App http.Handler
}

// Router leaves mail readiness and public tracking disabled. Operator readiness
// only tests its configured dependency; it is not mail-admission readiness.
func Router(operator, experimentalSubmission, console http.Handler, operatorReady func() bool) http.Handler {
	return RouterWithRuntime(operator, experimentalSubmission, console, operatorReady, MailRuntime{})
}

func RouterWithRuntime(operator, experimentalSubmission, console http.Handler, operatorReady func() bool, runtime MailRuntime) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/operator-readyz", func(w http.ResponseWriter, _ *http.Request) {
		if operatorReady == nil || !operatorReady() {
			http.Error(w, "operator subsystem is not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if experimentalSubmission != nil && runtime.Ready != nil && runtime.Ready() {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "delivery subsystem is not ready", http.StatusServiceUnavailable)
	})
	mux.Handle("/api/v1/", operator)
	if experimentalSubmission != nil {
		mux.Handle("/email", experimentalSubmission)
		mux.Handle("/email/batch", experimentalSubmission)
		mux.Handle("/emails", experimentalSubmission)
		mux.Handle("/emails/batch", experimentalSubmission)
		mux.Handle("/v3/", experimentalSubmission)
		if runtime.App != nil {
			mux.Handle("/app/", runtime.App)
		}
		// Provider suppression APIs: Resend GET /suppressions and Postmark
		// /message-streams/{stream}/suppressions[/dump|/delete].
		mux.Handle("/suppressions", experimentalSubmission)
		mux.Handle("/message-streams/", experimentalSubmission)
	}
	if console != nil {
		mux.Handle("/", console)
	}
	return RecoverPanics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Intercept before ServeMux's path-cleaning redirects can echo a bearer
		// capability in a Location header or the SPA fallback. The tracking
		// handler validates the original path, including malformed variants.
		isTracking := func(p string) bool { return p == "/t" || strings.HasPrefix(p, "/t/") }
		if isTracking(r.URL.Path) || isTracking(path.Clean("/"+r.URL.Path)) {
			if runtime.Tracking != nil {
				runtime.Tracking.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}), log.Printf)
}
