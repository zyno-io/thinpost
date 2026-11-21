package controlapp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminHostServesConsoleAndOperatorOnlyThere(t *testing.T) {
	marker := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(name)) })
	}
	build := func(admin string) http.Handler {
		return RouterWithRuntime(marker("operator"), marker("submission"), marker("console"), func() bool { return true },
			MailRuntime{Tracking: marker("tracking"), App: marker("app"), Ready: func() bool { return true }, AdminHost: admin})
	}
	serve := func(router http.Handler, method, host, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Host = host
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	router := build("admin.example.com")

	// The admin hostname, on any port and in any case, serves everything.
	for _, host := range []string{"admin.example.com", "Admin.Example.COM", "admin.example.com.", "admin.example.com:443", "admin.example.com:8443", "admin.example.com:1", "admin.example.com:"} {
		for target, want := range map[string]string{"/": "console", "/index.html": "console", "/apps/x": "console", "/api/v1/apps": "operator", "/t/abc": "tracking", "/emails": "submission"} {
			if w := serve(router, http.MethodGet, host, target); w.Body.String() != want {
				t.Fatalf("%s %s: %d %q, want %q", host, target, w.Code, w.Body.String(), want)
			}
		}
	}

	// Every other hostname hides the console and operator API, including the
	// root and ServeMux redirects, but keeps tracking, health and sending.
	for _, host := range []string{"t.example.com", "t.example.com:443", "", "198.51.100.7", "198.51.100.7:8080", "[2001:db8::1]:443", "admin.example.com.evil", "xadmin.example.com", "example.com"} {
		for _, tc := range []struct{ method, target string }{
			{http.MethodGet, "/"}, {http.MethodGet, "/index.html"}, {http.MethodGet, "/assets/app.js"},
			{http.MethodGet, "/api/v1/apps"}, {http.MethodPost, "/api/v1/apps"}, {http.MethodGet, "/api/v1"},
			{http.MethodGet, "/api/v1/../v1/apps"}, {http.MethodGet, "//api/v1/apps"}, {http.MethodGet, "/x/../"},
			{http.MethodOptions, "/api/v1/apps"},
			// Paths that clean into the console or API through a non-admin
			// prefix must not redirect there either.
			{http.MethodGet, "/v3/../api/v1/apps"}, {http.MethodGet, "/app/../"}, {http.MethodGet, "/emails/../index.html"},
			{http.MethodGet, "/livez/../api/v1/x"}, {http.MethodGet, "/api%2Fv1/apps"},
		} {
			w := serve(router, tc.method, host, tc.target)
			if w.Code != http.StatusNotFound || w.Body.Len() != 0 || w.Header().Get("Location") != "" ||
				w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("%q %s %s: %d %q %v", host, tc.method, tc.target, w.Code, w.Body.String(), w.Header())
			}
		}
		for target, want := range map[string]string{"/t/abc": "tracking", "/t": "tracking", "/emails": "submission", "/v3/x/messages": "submission", "/app/v1/x": "app", "/suppressions": "submission"} {
			if w := serve(router, http.MethodPost, host, target); w.Body.String() != want {
				t.Fatalf("%q %s: %d %q, want %q", host, target, w.Code, w.Body.String(), want)
			}
		}
		for _, probe := range []string{"/livez", "/readyz", "/operator-readyz"} {
			if w := serve(router, http.MethodGet, host, probe); w.Code != http.StatusNoContent {
				t.Fatalf("%q %s: %d", host, probe, w.Code)
			}
		}
	}

	// Without a console the unmatched catch-all is still an empty 404, so a
	// non-admin host cannot tell whether a console is deployed.
	noConsole := RouterWithRuntime(marker("operator"), marker("submission"), nil, nil, MailRuntime{AdminHost: "admin.example.com"})
	for _, target := range []string{"/", "/whatever", "/api/v1/apps"} {
		if w := serve(noConsole, http.MethodGet, "t.example.com", target); w.Code != http.StatusNotFound || w.Body.Len() != 0 {
			t.Fatalf("no console %s: %d %q", target, w.Code, w.Body.String())
		}
	}

	// Unset, every hostname serves the console as before.
	for _, host := range []string{"t.example.com", "admin.example.com", ""} {
		if w := serve(build(""), http.MethodGet, host, "/"); w.Body.String() != "console" {
			t.Fatalf("%q without admin origin: %q", host, w.Body.String())
		}
	}
}
