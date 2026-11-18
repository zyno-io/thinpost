// Command devidp is a tiny OpenID Connect issuer for the Docker Compose
// quickstart only. It signs in one fixed development operator with a single
// button, with no password, and issues access tokens that carry the
// administrator scope and role. It must never be exposed beyond a developer's
// own machine, so it refuses to start without --insecure-dev-only. It is not
// built into any release image.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	codeTTL          = time.Minute
	tokenTTL         = time.Hour
	automationClient = "thinpost-quickstart-automation"
)

type config struct {
	issuer, prefix, audience, clientID string
	redirectURI, logoutURI             string
	adminScope, adminRole              string
	subject, email, name               string
	automationSecret                   string
	scopes                             []string
}

type authCode struct {
	clientID, redirectURI, challenge, nonce string
	scopes                                  []string
	expires                                 time.Time
}

type server struct {
	cfg   config
	key   *rsa.PrivateKey
	kid   string
	mu    sync.Mutex
	codes map[string]authCode
	page  *template.Template
}

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	var (
		insecure   = flag.Bool("insecure-dev-only", false, "acknowledge that this issuer signs anyone in and is for local development only")
		listen     = flag.String("listen", ":9000", "listen address")
		issuer     = flag.String("issuer", "", "public https issuer URL, including any path prefix (for example https://localhost:8443/idp)")
		audience   = flag.String("audience", "thinpost-api", "access-token audience (the API resource, not the console client)")
		clientID   = flag.String("client-id", "thinpost-console", "public console client ID")
		redirect   = flag.String("redirect-uri", "", "exact console redirect URI")
		logout     = flag.String("post-logout-redirect-uri", "", "exact console post-logout redirect URI")
		adminScope = flag.String("admin-scope", "thinpost-admin", "scope claim value Thinpost requires")
		adminRole  = flag.String("admin-role", "Thinpost.Admin", "value placed in the roles claim")
		keyFile    = flag.String("signing-key", "", "PEM RSA private key (PKCS#8 or PKCS#1), at least 2048 bits")
		secretFile = flag.String("automation-secret-file", "", "file holding the client_credentials secret for the smoke test")
		health     = flag.Bool("healthcheck", false, "probe the local listener and exit")
	)
	flag.Parse()
	if *health {
		return probe(*listen, *issuer)
	}
	if !*insecure {
		return errors.New("refusing to start: devidp issues administrator tokens without a password; pass --insecure-dev-only to confirm this is a disposable local development stack")
	}
	u, err := url.Parse(*issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(*issuer, "/") {
		return errors.New("--issuer must be an https URL without a trailing slash, query or fragment")
	}
	for _, value := range []string{*redirect, *logout} {
		if r, err := url.Parse(value); err != nil || r.Scheme != "https" || r.Host == "" || r.Fragment != "" {
			return errors.New("--redirect-uri and --post-logout-redirect-uri must be https URLs")
		}
	}
	if *audience == "" || *audience == *clientID || *adminScope == "" || *adminRole == "" {
		return errors.New("audience must be set and differ from the client ID; admin scope and role are required")
	}
	key, err := loadKey(*keyFile)
	if err != nil {
		return err
	}
	secret, err := os.ReadFile(*secretFile)
	if err != nil || len(strings.TrimSpace(string(secret))) < 16 {
		return errors.New("cannot read a sufficiently long automation secret")
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(der)
	s := &server{
		cfg: config{issuer: *issuer, prefix: strings.TrimSuffix(u.EscapedPath(), "/"), audience: *audience, clientID: *clientID,
			redirectURI: *redirect, logoutURI: *logout, adminScope: *adminScope, adminRole: *adminRole,
			subject: "dev-operator", email: "dev@quickstart.example", name: "Quickstart Operator",
			automationSecret: strings.TrimSpace(string(secret)), scopes: []string{"openid", "profile", "email", *adminScope}},
		key: key, kid: base64.RawURLEncoding.EncodeToString(sum[:]), codes: map[string]authCode{},
		page: template.Must(template.New("login").Parse(loginPage)),
	}
	mux := http.NewServeMux()
	p := s.cfg.prefix
	mux.HandleFunc("GET "+p+"/.well-known/openid-configuration", s.discovery)
	mux.HandleFunc("GET "+p+"/jwks", s.jwks)
	mux.HandleFunc("GET "+p+"/authorize", s.authorize)
	mux.HandleFunc("POST "+p+"/authorize", s.authorize)
	mux.HandleFunc("POST "+p+"/token", s.token)
	mux.HandleFunc("GET "+p+"/logout", s.endSession)
	mux.HandleFunc("GET "+p+"/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := &http.Server{Addr: *listen, Handler: secure(mux), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("devidp (INSECURE, development only) issuer=%s listening on %s", s.cfg.issuer, *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func probe(listen, issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil {
		return err
	}
	_, port, _ := strings.Cut(listen, ":")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + strings.TrimSuffix(u.EscapedPath(), "/") + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unhealthy: %d", resp.StatusCode)
	}
	return nil
}

func loadKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read signing key")
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("signing key is not PEM")
	}
	var parsed any
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		err = errors.New("unsupported key type")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if err != nil || !ok || key.N.BitLen() < 2048 {
		return nil, errors.New("signing key must be an RSA key of at least 2048 bits")
	}
	return key, nil
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https:; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *server) discovery(w http.ResponseWriter, _ *http.Request) {
	i := s.cfg.issuer
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": i, "authorization_endpoint": i + "/authorize", "token_endpoint": i + "/token", "jwks_uri": i + "/jwks",
		"end_session_endpoint": i + "/logout", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "client_credentials"},
		"subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported": []string{"S256"}, "scopes_supported": s.cfg.scopes,
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"claims_supported":                      []string{"sub", "iss", "aud", "exp", "iat", "nonce", "scope", "roles", "name", "email"},
	})
}

func (s *server) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := s.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": s.kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

type authRequest struct {
	ClientID, RedirectURI, State, Nonce, Challenge, Scope string
}

// validate checks a request from either the query (GET) or the form (POST).
// A bad client or redirect URI is never redirected to.
func (s *server) validate(r *http.Request) (authRequest, []string, string, bool) {
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		if r.ParseForm() != nil {
			return authRequest{}, nil, "bad form", false
		}
		q = r.PostForm
	}
	a := authRequest{ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), State: q.Get("state"), Nonce: q.Get("nonce"), Challenge: q.Get("code_challenge"), Scope: q.Get("scope")}
	if a.ClientID != s.cfg.clientID || subtle.ConstantTimeCompare([]byte(a.RedirectURI), []byte(s.cfg.redirectURI)) != 1 {
		return a, nil, "unknown client or redirect URI", false
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || len(a.Challenge) < 43 || len(a.Challenge) > 128 {
		return a, nil, "this issuer requires the authorization code flow with PKCE (S256)", false
	}
	var granted []string
	for _, scope := range strings.Fields(a.Scope) {
		if slices.Contains(s.cfg.scopes, scope) && !slices.Contains(granted, scope) {
			granted = append(granted, scope)
		}
	}
	if !slices.Contains(granted, "openid") {
		return a, nil, "the openid scope is required", false
	}
	return a, granted, "", true
}

func (s *server) authorize(w http.ResponseWriter, r *http.Request) {
	a, scopes, problem, ok := s.validate(r)
	if !ok {
		http.Error(w, problem, http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.page.Execute(w, map[string]any{"Action": s.cfg.prefix + "/authorize", "Req": r.URL.Query(), "Email": s.cfg.email})
		return
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "entropy unavailable", http.StatusInternalServerError)
		return
	}
	code := hex.EncodeToString(raw)
	s.mu.Lock()
	for k, v := range s.codes {
		if time.Now().After(v.expires) {
			delete(s.codes, k)
		}
	}
	s.codes[code] = authCode{clientID: a.ClientID, redirectURI: a.RedirectURI, challenge: a.Challenge, nonce: a.Nonce, scopes: scopes, expires: time.Now().Add(codeTTL)}
	s.mu.Unlock()
	target, _ := url.Parse(a.RedirectURI)
	v := target.Query()
	v.Set("code", code)
	if a.State != "" {
		v.Set("state", a.State)
	}
	v.Set("iss", s.cfg.issuer)
	target.RawQuery = v.Encode()
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

func oauthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func (s *server) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r)
	case "client_credentials":
		s.automation(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type")
	}
}

func (s *server) exchangeCode(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	s.mu.Lock()
	c, found := s.codes[f.Get("code")]
	delete(s.codes, f.Get("code")) // single use
	s.mu.Unlock()
	sum := sha256.Sum256([]byte(f.Get("code_verifier")))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if !found || time.Now().After(c.expires) || c.clientID != f.Get("client_id") || c.redirectURI != f.Get("redirect_uri") ||
		subtle.ConstantTimeCompare([]byte(challenge), []byte(c.challenge)) != 1 {
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	now := time.Now()
	access, err := s.sign(jwt.MapClaims{
		"iss": s.cfg.issuer, "sub": s.cfg.subject, "aud": s.cfg.audience, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(tokenTTL).Unix(),
		"jti": randomID(), "client_id": c.clientID, "scope": strings.Join(c.scopes, " "), "roles": []string{s.cfg.adminRole},
		"name": s.cfg.name, "email": s.cfg.email,
	}, "at+jwt")
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	idClaims := jwt.MapClaims{
		"iss": s.cfg.issuer, "sub": s.cfg.subject, "aud": c.clientID, "iat": now.Unix(), "exp": now.Add(tokenTTL).Unix(),
		"auth_time": now.Unix(), "name": s.cfg.name, "email": s.cfg.email,
	}
	if c.nonce != "" {
		idClaims["nonce"] = c.nonce
	}
	id, err := s.sign(idClaims, "JWT")
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "id_token": id, "token_type": "Bearer", "expires_in": int(tokenTTL.Seconds()), "scope": strings.Join(c.scopes, " ")})
}

// automation serves the smoke test: client_credentials with a generated secret.
func (s *server) automation(w http.ResponseWriter, r *http.Request) {
	id, secret, basic := r.BasicAuth()
	if !basic {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != automationClient || subtle.ConstantTimeCompare([]byte(secret), []byte(s.cfg.automationSecret)) != 1 {
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	now := time.Now()
	scope := s.cfg.adminScope
	access, err := s.sign(jwt.MapClaims{
		"iss": s.cfg.issuer, "sub": "quickstart-automation", "aud": s.cfg.audience, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"jti": randomID(), "client_id": id, "scope": scope, "roles": []string{s.cfg.adminRole},
	}, "at+jwt")
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 600, "scope": scope})
}

func (s *server) sign(claims jwt.MapClaims, typ string) (string, error) {
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	t.Header["kid"] = s.kid
	t.Header["typ"] = typ
	return t.SignedString(s.key)
}

func (s *server) endSession(w http.ResponseWriter, r *http.Request) {
	target := s.cfg.logoutURI
	if requested := r.URL.Query().Get("post_logout_redirect_uri"); requested != "" && requested != s.cfg.logoutURI {
		http.Error(w, "unknown post-logout redirect URI", http.StatusBadRequest)
		return
	}
	if state := r.URL.Query().Get("state"); state != "" {
		u, _ := url.Parse(target)
		q := u.Query()
		q.Set("state", state)
		u.RawQuery = q.Encode()
		target = u.String()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func randomID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

const loginPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Thinpost quickstart sign-in</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:28rem;margin:4rem auto;padding:0 1rem}
.warn{background:#fff4d6;border:1px solid #d9a400;padding:.75rem;border-radius:.4rem;font-size:.9rem}
button{font:inherit;padding:.6rem 1.2rem;margin-top:1rem;cursor:pointer}</style></head>
<body><h1>Thinpost quickstart</h1>
<p class="warn"><strong>Development sign-in.</strong> This issuer has no password and signs in a fixed administrator. It exists only for the local Docker Compose quickstart.</p>
<form method="post" action="{{.Action}}">
{{range $k, $v := .Req}}{{range $v}}<input type="hidden" name="{{$k}}" value="{{.}}">{{end}}{{end}}
<p>Continue as <strong>{{.Email}}</strong>.</p>
<button type="submit">Sign in as the dev operator</button></form></body></html>`
