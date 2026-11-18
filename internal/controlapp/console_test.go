package controlapp

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	indexBody   = "<!doctype html><title>console-index</title>"
	outsideBody = "outside-secret"
)

func writeFile(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// consoleFixture builds a dist-like directory next to a file and directory
// that must never be reachable, including through symlinks.
func consoleFixture(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "dist")
	writeFile(t, filepath.Join(dir, "index.html"), indexBody)
	writeFile(t, filepath.Join(dir, "assets", "index-abc123.js"), "console.log('app')")
	writeFile(t, filepath.Join(dir, "assets", "index-abc123.css"), "body{}")
	writeFile(t, filepath.Join(dir, "assets", "fa-solid-900-xyz.woff2"), "wOF2")
	writeFile(t, filepath.Join(dir, "robots.txt"), "User-agent: *")
	writeFile(t, filepath.Join(dir, ".env"), "hidden")
	writeFile(t, filepath.Join(dir, "nested", "file.txt"), "nested")
	writeFile(t, filepath.Join(base, "secret.txt"), outsideBody)
	writeFile(t, filepath.Join(base, "private", "secret.txt"), outsideBody)
	if err := os.Symlink(filepath.Join(base, "secret.txt"), filepath.Join(dir, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "private"), filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "private"), filepath.Join(dir, "assets", "escape")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newTestConsole(t *testing.T) *Console {
	t.Helper()
	console, err := NewConsole(consoleFixture(t), ConsolePolicy{ConnectOrigins: []string{"https://id.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = console.Close() })
	return console
}

func stubHandler(code int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	})
}

func TestConsoleRouting(t *testing.T) {
	console := newTestConsole(t)
	operator := stubHandler(http.StatusNotFound, "operator")
	submission := stubHandler(http.StatusAccepted, "submission")
	withoutSubmission := Router(operator, nil, console, nil)
	withSubmission := Router(operator, submission, console, nil)
	for _, test := range []struct {
		router       http.Handler
		method, path string
		code         int
		body         string
		cache        string
		contentType  string
	}{
		// SPA navigation, including the OIDC callback with its query.
		{withoutSubmission, "GET", "/", 200, indexBody, "no-cache", "text/html; charset=utf-8"},
		{withoutSubmission, "GET", "/index.html", 200, indexBody, "no-cache", "text/html; charset=utf-8"},
		{withoutSubmission, "GET", "/auth/callback?code=abc&state=def", 200, indexBody, "no-cache", "text/html; charset=utf-8"},
		{withoutSubmission, "GET", "/apps", 200, indexBody, "no-cache", ""},
		{withoutSubmission, "GET", "/apps/0123456789abcdef0123456789abcdef/messages/0123456789abcdef0123456789abcdef", 200, indexBody, "no-cache", ""},
		{withoutSubmission, "GET", "/domains", 200, indexBody, "no-cache", ""},
		{withoutSubmission, "GET", "/nested", 200, indexBody, "no-cache", ""},
		{withoutSubmission, "HEAD", "/auth/callback", 200, "", "no-cache", "text/html; charset=utf-8"},
		// Built files.
		{withoutSubmission, "GET", "/assets/index-abc123.js", 200, "console.log('app')", "public, max-age=31536000, immutable", "text/javascript; charset=utf-8"},
		{withoutSubmission, "GET", "/assets/index-abc123.css", 200, "body{}", "public, max-age=31536000, immutable", "text/css; charset=utf-8"},
		{withoutSubmission, "GET", "/assets/fa-solid-900-xyz.woff2", 200, "wOF2", "public, max-age=31536000, immutable", "font/woff2"},
		{withoutSubmission, "GET", "/robots.txt", 200, "User-agent: *", "no-cache", "text/plain; charset=utf-8"},
		{withoutSubmission, "GET", "/nested/file.txt", 200, "nested", "no-cache", ""},
		// Missing files and assets never fall back to index.html.
		{withoutSubmission, "GET", "/assets/index-missing.js", 404, "", "", ""},
		{withoutSubmission, "GET", "/assets/missing", 404, "", "", ""},
		{withoutSubmission, "GET", "/assets", 404, "", "", ""},
		{withoutSubmission, "GET", "/assets/", 404, "", "", ""},
		{withoutSubmission, "GET", "/favicon.ico", 404, "", "", ""},
		{withoutSubmission, "GET", "/apps/main.js", 404, "", "", ""},
		{withoutSubmission, "GET", "/.env", 404, "", "", ""},
		{withoutSubmission, "GET", "/nested/.env", 404, "", "", ""},
		// Server routes keep their behavior; unknown API-like paths are 404.
		{withoutSubmission, "GET", "/api/v1/", 404, "operator", "", ""},
		{withoutSubmission, "GET", "/api/v1/missing", 404, "operator", "", ""},
		{withoutSubmission, "POST", "/api/v1/apps", 404, "operator", "", ""},
		{withoutSubmission, "GET", "/api", 404, "", "", ""},
		{withoutSubmission, "GET", "/api/", 404, "", "", ""},
		{withoutSubmission, "GET", "/api/v2/apps", 404, "", "", ""},
		{withoutSubmission, "GET", "/livez", 204, "", "", ""},
		{withoutSubmission, "GET", "/readyz", 503, "delivery subsystem is not ready\n", "", ""},
		{withoutSubmission, "GET", "/operator-readyz", 503, "operator subsystem is not ready\n", "", ""},
		{withoutSubmission, "GET", "/livez/extra", 404, "", "", ""},
		{withoutSubmission, "GET", "/readyz/extra", 404, "", "", ""},
		{withoutSubmission, "GET", "/operator-readyz/extra", 404, "", "", ""},
		// Experimental submission routes stay disabled by default.
		{withoutSubmission, "POST", "/email", 404, "", "", ""},
		{withoutSubmission, "GET", "/email", 404, "", "", ""},
		{withoutSubmission, "POST", "/email/batch", 404, "", "", ""},
		{withoutSubmission, "POST", "/emails", 404, "", "", ""},
		{withoutSubmission, "POST", "/v3/mail/send", 404, "", "", ""},
		{withoutSubmission, "GET", "/v3", 404, "", "", ""},
		// Reserved provider suppression routes never fall back to the console.
		{withoutSubmission, "GET", "/suppressions", 404, "", "", ""},
		{withoutSubmission, "GET", "/suppressions/x", 404, "", "", ""},
		{withoutSubmission, "GET", "/message-streams/outbound/suppressions/dump", 404, "", "", ""},
		{withSubmission, "GET", "/suppressions", 202, "submission", "", ""},
		{withSubmission, "POST", "/message-streams/outbound/suppressions", 202, "submission", "", ""},
		{withSubmission, "GET", "/suppressions/x", 404, "", "", ""},
		{withSubmission, "POST", "/email", 202, "submission", "", ""},
		{withSubmission, "POST", "/email/batch", 202, "submission", "", ""},
		{withSubmission, "POST", "/emails", 202, "submission", "", ""},
		{withSubmission, "POST", "/v3/mail/send", 202, "submission", "", ""},
		{withSubmission, "POST", "/email/other", 404, "", "", ""},
		{withSubmission, "GET", "/readyz", 503, "delivery subsystem is not ready\n", "", ""},
		// Only reads reach the console.
		{withoutSubmission, "POST", "/apps", 405, "", "", ""},
		{withoutSubmission, "DELETE", "/", 405, "", "", ""},
		// Symlinks that resolve outside the directory are absent.
		{withoutSubmission, "GET", "/escape.txt", 404, "", "", ""},
		{withoutSubmission, "GET", "/escape/secret.txt", 404, "", "", ""},
		{withoutSubmission, "GET", "/assets/escape/secret.txt", 404, "", "", ""},
	} {
		w := httptest.NewRecorder()
		test.router.ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		name := test.method + " " + test.path
		body := w.Body.String()
		if w.Code != test.code {
			t.Errorf("%s: status %d, want %d", name, w.Code, test.code)
		}
		if test.body != "" && body != test.body {
			t.Errorf("%s: body %q, want %q", name, body, test.body)
		}
		if test.body != indexBody && strings.Contains(body, "console-index") {
			t.Errorf("%s: received index.html", name)
		}
		if strings.Contains(body, outsideBody) {
			t.Errorf("%s: served a file outside the console directory", name)
		}
		if test.cache != "" && w.Header().Get("Cache-Control") != test.cache {
			t.Errorf("%s: Cache-Control %q, want %q", name, w.Header().Get("Cache-Control"), test.cache)
		}
		if test.contentType != "" && w.Header().Get("Content-Type") != test.contentType {
			t.Errorf("%s: Content-Type %q, want %q", name, w.Header().Get("Content-Type"), test.contentType)
		}
		if test.code == 200 {
			for header, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "SAMEORIGIN", "Referrer-Policy": "no-referrer"} {
				if got := w.Header().Get(header); got != want {
					t.Errorf("%s: %s %q, want %q", name, header, got, want)
				}
			}
			if got := w.Header().Get("Content-Security-Policy"); got != wantConsoleCSP {
				t.Errorf("%s: Content-Security-Policy %q", name, got)
			}

		}
	}
}

func TestConsoleRedirectsKeepServerRoutes(t *testing.T) {
	router := Router(stubHandler(200, "operator"), stubHandler(202, "submission"), newTestConsole(t), nil)
	for path, location := range map[string]string{"/api/v1": "/api/v1/", "/v3": "/v3/"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code < 300 || w.Code > 399 || w.Header().Get("Location") != location {
			t.Errorf("%s: status %d location %q", path, w.Code, w.Header().Get("Location"))
		}
	}
}

// TestConsoleConfinesTraversal bypasses ServeMux path cleaning so the handler
// itself must resolve dot segments within its directory.
func TestConsoleConfinesTraversal(t *testing.T) {
	console := newTestConsole(t)
	for _, raw := range []string{"/../secret.txt", "/assets/../../secret.txt", "/../private/secret.txt", "../secret.txt", "/assets/../../../../etc/passwd", "/..", "/../"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.URL.Path = raw
		w := httptest.NewRecorder()
		console.ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), outsideBody) || strings.Contains(w.Body.String(), "root:") {
			t.Errorf("%q: served a file outside the console directory", raw)
		}
		// Dot segments resolve to a path inside the directory: an absent file
		// with an extension is 404; an extensionless one is a client route.
		if strings.Contains(raw, ".txt") && w.Code != http.StatusNotFound {
			t.Errorf("%q: status %d", raw, w.Code)
		}
		if !strings.Contains(raw, ".txt") && w.Body.String() != indexBody {
			t.Errorf("%q: body %q", raw, w.Body.String())
		}
	}
	// ServeMux cleans only the escaped path, so encoded dot segments reach the
	// console decoded; it must still never serve outside its directory.
	router := Router(stubHandler(200, "operator"), nil, console, nil)
	for _, target := range []string{"/%2e%2e/secret.txt", "/assets/%2e%2e/%2e%2e/secret.txt", "/assets/..%2f..%2fsecret.txt"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		if w.Code == http.StatusOK || strings.Contains(w.Body.String(), outsideBody) {
			t.Errorf("%q: status %d", target, w.Code)
		}
	}
}

// TestConsoleIndexRevalidatesByContent covers a rollback: the browser holds a
// newer index.html than the one now deployed and must not get 304 by mtime,
// with or without an ETag.
func TestConsoleIndexRevalidatesByContent(t *testing.T) {
	router := Router(stubHandler(404, ""), nil, newTestConsole(t), nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/auth/callback", nil))
	etag := w.Header().Get("ETag")
	if w.Code != 200 || etag == "" {
		t.Fatalf("status %d etag %q", w.Code, etag)
	}
	if lastModified := w.Header().Get("Last-Modified"); lastModified != "" {
		t.Errorf("index.html sent Last-Modified %q", lastModified)
	}
	future := time.Now().Add(24 * time.Hour).UTC().Format(http.TimeFormat)
	for _, test := range []struct {
		path, ifNoneMatch string
		want              int
	}{
		{"/", etag, http.StatusNotModified},
		{"/", `"stale-build"`, http.StatusOK},
		// No ETag held: a future If-Modified-Since must not yield a stale 304.
		{"/", "", http.StatusOK},
		{"/auth/callback", "", http.StatusOK},
		{"/index.html", "", http.StatusOK},
		{"/robots.txt", "", http.StatusOK},
	} {
		r := httptest.NewRequest("GET", test.path, nil)
		if test.ifNoneMatch != "" {
			r.Header.Set("If-None-Match", test.ifNoneMatch)
		}
		r.Header.Set("If-Modified-Since", future)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != test.want {
			t.Errorf("%s If-None-Match %q: status %d, want %d", test.path, test.ifNoneMatch, w.Code, test.want)
		}
		if test.want == http.StatusOK && w.Header().Get("Last-Modified") != "" {
			t.Errorf("%s: unhashed file sent Last-Modified", test.path)
		}
	}
	// Hashed assets keep immutable caching and mtime validation, without hashing.
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/assets/index-abc123.js", nil))
	if w.Header().Get("ETag") != "" || w.Header().Get("Last-Modified") == "" || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("asset headers %v", w.Header())
	}
	r := httptest.NewRequest("GET", "/assets/index-abc123.js", nil)
	r.Header.Set("If-Modified-Since", future)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified {
		t.Errorf("asset conditional request: status %d", w.Code)
	}
}

func TestNewConsoleRequiresBuiltIndex(t *testing.T) {
	if _, err := NewConsole("relative/dist", ConsolePolicy{}); err == nil {
		t.Error("relative directory accepted")
	}
	base := t.TempDir()
	if _, err := NewConsole(filepath.Join(base, "missing"), ConsolePolicy{}); err == nil {
		t.Error("missing directory accepted")
	}
	empty := filepath.Join(base, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewConsole(empty, ConsolePolicy{}); err == nil {
		t.Error("directory without index.html accepted")
	}
	dirIndex := filepath.Join(base, "dir-index")
	if err := os.MkdirAll(filepath.Join(dirIndex, "index.html"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewConsole(dirIndex, ConsolePolicy{}); err == nil {
		t.Error("index.html directory accepted")
	}
	file := filepath.Join(base, "file")
	writeFile(t, file, "x")
	if _, err := NewConsole(file, ConsolePolicy{}); err == nil {
		t.Error("regular file accepted as directory")
	}
	linked := filepath.Join(base, "linked")
	writeFile(t, filepath.Join(base, "outside-index.html"), outsideBody)
	if err := os.Mkdir(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside-index.html"), filepath.Join(linked, "index.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewConsole(linked, ConsolePolicy{}); err == nil {
		t.Error("index.html symlink outside the directory accepted")
	}
}

func TestConsoleNotServedWithoutConfiguration(t *testing.T) {
	router := Router(stubHandler(204, ""), nil, nil, nil)
	for _, path := range []string{"/", "/auth/callback", "/assets/index.js"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", path, w.Code)
		}
	}
}

func TestConfigConsoleDirectory(t *testing.T) {
	values := env()
	config, err := load(values)
	if err != nil || config.UIDir != "" {
		t.Fatal("console enabled by default", err)
	}
	values["THINPOST_UI_DIR"] = "ui/dist"
	if _, err := load(values); err == nil {
		t.Fatal("relative console directory accepted")
	}
	values["THINPOST_UI_DIR"] = "/usr/share/thinpost/ui"
	config, err = load(values)
	if err != nil || config.UIDir != "/usr/share/thinpost/ui" {
		t.Fatal("absolute console directory rejected", err)
	}
}

const wantConsoleCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
	"font-src 'self'; connect-src 'self' https://id.example.com; frame-src 'none'; child-src 'none'; worker-src 'none'; " +
	"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'; manifest-src 'self'"

func TestConsolePolicy(t *testing.T) {
	csp, err := consoleCSP(ConsolePolicy{ConnectOrigins: []string{"https://id.example.com"}})
	if err != nil || csp != wantConsoleCSP {
		t.Fatalf("unexpected CSP %q %v", csp, err)
	}
	for _, forbidden := range []string{"unsafe-eval", "https:", "*"} {
		if strings.Contains(strings.ReplaceAll(csp, "https://id.example.com", ""), forbidden) {
			t.Errorf("CSP contains %q", forbidden)
		}
	}
	for _, bad := range []string{"", "id.example.com", "https://id.example.com/tenant", "https://u:p@id.example.com", "ftp://id.example.com", "https://id.example.com?x=1"} {
		if _, err := consoleCSP(ConsolePolicy{ConnectOrigins: []string{bad}}); err == nil {
			t.Errorf("accepted connect origin %q", bad)
		}
	}
	if got := OriginOf("https://login.example.com/tenant/v2.0?x=1"); got != "https://login.example.com" {
		t.Errorf("OriginOf = %q", got)
	}
	if got := OriginOf("not a url"); got != "" {
		t.Errorf("OriginOf = %q", got)
	}
}
