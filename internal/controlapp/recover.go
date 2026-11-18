package controlapp

import (
	"net/http"
	"path"
	"runtime/debug"
	"strings"
)

// routeClass maps a request to a fixed label. Request URLs and values are never
// logged: tracking paths are bearer capabilities and panic values may carry
// message content.
func routeClass(rawPath string) string {
	p := path.Clean("/" + rawPath)
	within := func(prefix string) bool { return p == prefix || strings.HasPrefix(p, prefix+"/") }
	switch {
	case within("/livez"), within("/readyz"), within("/operator-readyz"):
		return "health"
	case within("/api"):
		return "operator_api"
	case within("/t"):
		return "tracking"
	case within("/email"), within("/emails"), within("/v3"), within("/suppressions"), within("/message-streams"):
		return "submission"
	default:
		return "console"
	}
}

// RecoverPanics replaces net/http's default panic logging, which would print
// the remote address and the panic value. It logs only a route class and the
// stack, then aborts the connection with http.ErrAbortHandler.
func RecoverPanics(next http.Handler, logf func(format string, args ...any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			value := recover()
			if value == nil {
				return
			}
			if value != http.ErrAbortHandler {
				logf("HTTP handler panic class=%s\n%s", routeClass(r.URL.Path), debug.Stack())
			}
			panic(http.ErrAbortHandler)
		}()
		next.ServeHTTP(w, r)
	})
}
