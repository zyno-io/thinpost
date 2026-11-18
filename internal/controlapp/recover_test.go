package controlapp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoverPanicsLogsOnlyClassAndStack(t *testing.T) {
	for path, class := range map[string]string{
		"/t/secretcapability": "tracking",
		"/api/v1/apps":        "operator_api",
		"/emails":             "submission",
		"/suppressions":       "submission",
		"/message-streams/outbound/suppressions/dump": "submission",
		"/readyz":        "health",
		"/auth/callback": "console",
	} {
		var logged string
		h := RecoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("victim@example.com")
		}), func(format string, args ...any) { logged = fmt.Sprintf(format, args...) })
		func() {
			defer func() {
				if recovered := recover(); recovered != http.ErrAbortHandler {
					t.Fatalf("%s: expected ErrAbortHandler, got %v", path, recovered)
				}
			}()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path+"?q=victim@example.com", nil))
		}()
		if !strings.HasPrefix(logged, "HTTP handler panic class="+class+"\n") || !strings.Contains(logged, "goroutine") {
			t.Fatalf("%s: unexpected log %q", path, logged)
		}
		for _, forbidden := range []string{"victim", "example.com", "secretcapability", "192.0.2.1"} {
			if strings.Contains(logged, forbidden) {
				t.Fatalf("%s: log leaked %q", path, forbidden)
			}
		}
	}
}

func TestRouterRecoversPanics(t *testing.T) {
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("victim@example.com") })
	router := RouterWithRuntime(boom, nil, nil, nil, MailRuntime{})
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("expected ErrAbortHandler, got %v", recovered)
		}
	}()
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))
}
