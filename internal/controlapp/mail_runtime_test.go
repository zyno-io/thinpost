package controlapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/tracking"
)

func TestMailReadinessRequiresSubmissionAndAdmission(t *testing.T) {
	noop := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, tc := range []struct {
		name       string
		submission http.Handler
		ready      func() bool
		want       int
	}{
		{"configured and open", noop, func() bool { return true }, http.StatusNoContent},
		{"closed admission", noop, func() bool { return false }, http.StatusServiceUnavailable},
		{"no admission check", noop, nil, http.StatusServiceUnavailable},
		{"operator only", nil, func() bool { return true }, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := RouterWithRuntime(noop, tc.submission, nil, func() bool { return true }, MailRuntime{Ready: tc.ready})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if w.Code != tc.want {
				t.Fatalf("readiness %d", w.Code)
			}
			w = httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/operator-readyz", nil))
			if w.Code != http.StatusNoContent {
				t.Fatal("mail admission disabled operator access")
			}
		})
	}
}

func TestEnabledTrackingRejectsMalformedPathsBeforeMuxRedirects(t *testing.T) {
	console := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("console")) })
	router := RouterWithRuntime(console, nil, console, nil, MailRuntime{Tracking: &tracking.Handler{}})
	for _, path := range []string{"/t//c/token", "/other/../t/c/token", "/t/c/token?secret=fixture", "/t/c/%2Ffixture"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" || strings.Contains(w.Body.String(), "fixture") || strings.Contains(w.Body.String(), "console") {
			t.Fatalf("tracking path normalized or exposed: %d", w.Code)
		}
	}
}

type recordedUnsubscribes struct{ ids []string }

func (r *recordedUnsubscribes) RecordUnsubscribe(_ context.Context, id string) error {
	r.ids = append(r.ids, id)
	return nil
}

func TestUnsubscribePostReachesTrackingHandlerAndMalformedVariantsDoNot(t *testing.T) {
	keys, err := tracking.NewLinkKeys([]byte("0123456789abcdef0123456789abcdef"), nil)
	if err != nil {
		t.Fatal(err)
	}
	const origin = "https://track.example.com"
	const delivery = "00112233445566778899aabbccddeeff"
	link, err := keys.UnsubscribeLink(origin, delivery)
	if err != nil {
		t.Fatal(err)
	}
	recorded := &recordedUnsubscribes{}
	console := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("console")) })
	router := RouterWithRuntime(console, nil, console, nil, MailRuntime{Tracking: &tracking.Handler{Origin: origin, Links: keys, Unsubscribes: recorded}})
	post := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("List-Unsubscribe=One-Click"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	path := strings.TrimPrefix(link, origin)
	if w := post(path); w.Code != http.StatusOK || len(recorded.ids) != 1 || recorded.ids[0] != delivery || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("POST did not reach the tracking handler", w.Code)
	}
	for _, malformed := range []string{strings.Replace(path, "/t/u/", "/t//u/", 1), "/other/.." + path, path + "?x=y", strings.Replace(path, "/t/u/", "/t/u/%2e/", 1)} {
		if w := post(malformed); w.Code == http.StatusOK || strings.Contains(w.Body.String(), "console") || len(recorded.ids) != 1 {
			t.Fatal("malformed variant was recorded or reached the console", malformed, w.Code)
		}
	}
}

func TestAppReadRoutesFollowSubmissionAndNeverFallToConsole(t *testing.T) {
	console := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("console")) })
	operator := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("operator")) })
	submission := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("submission")) })
	app := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("app")) })
	get := func(router http.Handler, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	enabled := RouterWithRuntime(operator, submission, console, nil, MailRuntime{App: app})
	if got := get(enabled, "/app/v1/messages/x"); got.Body.String() != "app" {
		t.Fatal("app route not served", got.Body.String())
	}
	// Without submission configured, or without an app handler, the routes are
	// absent and the console's reserved-prefix rule answers 404, not the SPA.
	real := newTestConsole(t)
	for name, router := range map[string]http.Handler{
		"no submission": RouterWithRuntime(operator, nil, real, nil, MailRuntime{App: app}),
		"no app":        RouterWithRuntime(operator, submission, real, nil, MailRuntime{}),
	} {
		if got := get(router, "/app/v1/messages/x"); got.Code != http.StatusNotFound || strings.Contains(got.Body.String(), "app") {
			t.Fatalf("%s: %d", name, got.Code)
		}
	}
	if !reserved("/app") || !reserved("/app/v1/messages/x") || reserved("/application") {
		t.Fatal("/app is not a reserved console path")
	}
}

func TestNativeSendRouteFollowsSubmissionAndNeverFallsToAppReadsOrConsole(t *testing.T) {
	console := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("console")) })
	operator := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("operator")) })
	submission := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("submission")) })
	app := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("app")) })
	post := func(router http.Handler, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		return w
	}
	enabled := RouterWithRuntime(operator, submission, console, nil, MailRuntime{App: app})
	if got := post(enabled, "/app/v1/messages"); got.Body.String() != "submission" {
		t.Fatal("send route not served by submission", got.Body.String())
	}
	// Message reads under the same prefix still go to the app read handler.
	if got := post(enabled, "/app/v1/messages/x/release"); got.Body.String() != "app" {
		t.Fatal("read route moved", got.Body.String())
	}
	// Without submission the send route is absent, even when reads exist, and
	// the console's reserved-prefix rule answers 404 rather than the SPA.
	real := newTestConsole(t)
	for name, router := range map[string]http.Handler{
		"no submission":          RouterWithRuntime(operator, nil, real, nil, MailRuntime{App: app}),
		"no submission, no apps": RouterWithRuntime(operator, nil, real, nil, MailRuntime{}),
	} {
		if got := post(router, "/app/v1/messages"); got.Code != http.StatusNotFound || strings.Contains(got.Body.String(), "app") || strings.Contains(got.Body.String(), "submission") {
			t.Fatalf("%s: %d", name, got.Code)
		}
	}
}
