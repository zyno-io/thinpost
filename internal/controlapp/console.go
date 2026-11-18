package controlapp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// consoleTypes pins content types for the built console so responses never
// depend on the host MIME database or content sniffing.
var consoleTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".html":  "text/html; charset=utf-8",
	".ico":   "image/x-icon",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".mjs":   "text/javascript; charset=utf-8",
	".png":   "image/png",
	".svg":   "image/svg+xml",
	".txt":   "text/plain; charset=utf-8",
	".webp":  "image/webp",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

// reservedPrefixes are server routes. They and their subpaths never fall back
// to the console, whether or not the corresponding handler is enabled.
var reservedPrefixes = []string{"/api", "/app", "/livez", "/readyz", "/operator-readyz", "/email", "/emails", "/v3", "/suppressions", "/message-streams", "/t"}

func reserved(p string) bool {
	for _, prefix := range reservedPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// Console serves a built single-page console from dir. Existing files are
// served as-is; extensionless client routes such as /auth/callback receive
// index.html. Missing files with an extension, anything under /assets/,
// dotfiles and reserved server routes are 404. Access is confined to dir by
// os.Root, which also refuses symlinks that resolve outside it.
type Console struct {
	root *os.Root
	csp  string
}

// ConsolePolicy configures the Content-Security-Policy sent with every
// console response.
type ConsolePolicy struct {
	// ConnectOrigins are extra origins (scheme and host only) for connect-src,
	// such as the OIDC issuer whose discovery and token endpoints the console
	// fetches. Entries with a path, query or userinfo are rejected.
	ConnectOrigins []string
}

// consoleCSP builds the policy once. A srcdoc iframe inherits its parent's
// CSP, so img-src deliberately lacks https: and style-src permits the inline
// styles that email HTML depends on, while script-src stays 'self'.
func consoleCSP(policy ConsolePolicy) (string, error) {
	connect := "'self'"
	for _, origin := range policy.ConnectOrigins {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || strings.Contains(u.Host, "*") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return "", errors.New("console connect origin is invalid")
		}
		connect += " " + u.Scheme + "://" + u.Host
	}
	return "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
		"font-src 'self'; connect-src " + connect + "; frame-src 'none'; child-src 'none'; worker-src 'none'; " +
		"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'; manifest-src 'self'", nil
}

// OriginOf returns the scheme and host of an absolute URL, or "" if it has none.
func OriginOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// NewConsole opens dir, which must be an absolute directory containing a
// regular index.html file.
func NewConsole(dir string, policy ConsolePolicy) (*Console, error) {
	csp, err := consoleCSP(policy)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(dir) {
		return nil, errors.New("console directory must be absolute")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.New("console directory is unavailable")
	}
	info, err := root.Stat("index.html")
	if err != nil || !info.Mode().IsRegular() {
		_ = root.Close()
		return nil, errors.New("console index.html is unavailable")
	}
	return &Console{root: root, csp: csp}, nil
}

func (c *Console) Close() error { return c.root.Close() }

func (c *Console) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	clean := path.Clean("/" + r.URL.Path)
	if reserved(clean) || strings.Contains(clean, "/.") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if clean != "/" && c.serve(w, r, clean[1:]) {
		return
	}
	if clean == "/assets" || strings.HasPrefix(clean, "/assets/") || path.Ext(clean) != "" {
		http.NotFound(w, r)
		return
	}
	if !c.serve(w, r, "index.html") {
		http.Error(w, "console unavailable", http.StatusServiceUnavailable)
	}
}

// serve writes name if it is a regular file inside the root and reports
// whether it did. Errors other than a successful open are treated as absent.
func (c *Console) serve(w http.ResponseWriter, r *http.Request, name string) bool {
	if !fs.ValidPath(name) {
		return false
	}
	file, err := c.root.Open(name)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	ext := path.Ext(name)
	contentType := consoleTypes[ext]
	if contentType == "" {
		contentType = mime.TypeByExtension(ext)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	cacheControl := "public, max-age=31536000, immutable" // Vite content-hashes assets/.
	etag := ""
	modTime := info.ModTime()
	if !strings.HasPrefix(name, "assets/") {
		// Unhashed files such as index.html revalidate by content only, so a
		// rollback to an older build reaches browsers that cached a newer one.
		// A zero modification time makes ServeContent omit Last-Modified and
		// ignore If-Modified-Since/If-Unmodified-Since.
		cacheControl = "no-cache"
		modTime = time.Time{}
		sum := sha256.New()
		if _, err := io.Copy(sum, file); err != nil {
			return false
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return false
		}
		etag = `"` + hex.EncodeToString(sum.Sum(nil)[:16]) + `"`
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", cacheControl)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Content-Security-Policy", c.csp)
	if etag != "" {
		h.Set("ETag", etag)
	}
	http.ServeContent(w, r, "", modTime, file)
	return true
}
