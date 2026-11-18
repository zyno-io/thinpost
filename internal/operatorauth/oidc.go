// Package operatorauth validates delegated JWT access tokens for the operator
// API. Runtime identity-provider configuration and UI login wiring are pending.
package operatorauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/zyno-io/thinpost/internal/admin"
)

// DefaultRolesClaim is the role claim read when Config.RolesClaim is empty.
const DefaultRolesClaim = "roles"

// Config describes operator token validation. RequiredScope is optional: when
// empty, the administrator role alone grants administrator status. RolesClaim
// is a top-level claim name or a dotted path into nested objects (for example
// realm_access.roles); it defaults to DefaultRolesClaim.
type Config struct {
	Issuer, Audience, JWKSURL, RequiredScope, AdminRole, RolesClaim string
	Algorithms                                                      []string
}
type Authenticator struct {
	config Config
	keys   func(context.Context) jwt.Keyfunc
}

// New loads and refreshes keys from an operator-configured HTTPS JWKS URL. The
// context controls the background refresh lifecycle. Configure Audience to the
// API resource, not the browser's OIDC client ID, and use delegated access
// tokens carrying the required scope and administrator role.
func New(ctx context.Context, config Config) (*Authenticator, error) {
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("JWKS redirects are not allowed") }}
	return newWithClient(ctx, config, client)
}

func newWithClient(ctx context.Context, config Config, client *http.Client) (*Authenticator, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	allowInitialFailure := false
	keys, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{config.JWKSURL}, keyfunc.Override{Client: client, HTTPTimeout: 5 * time.Second, NoErrorReturnFirstHTTPReq: &allowInitialFailure, RefreshInterval: 5 * time.Minute, RateLimitWaitMax: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	return &Authenticator{config: config, keys: signatureKeys(keys)}, nil
}
func validateConfig(config Config) error {
	if !httpsURL(config.Issuer) || !httpsURL(config.JWKSURL) || config.Audience == "" || config.AdminRole == "" || len(config.Algorithms) == 0 {
		return errors.New("incomplete operator token configuration")
	}
	for _, algorithm := range config.Algorithms {
		if !AllowedAlgorithm(algorithm) {
			return errors.New("unsupported token algorithm")
		}
	}
	if strings.ContainsAny(config.Audience+config.RequiredScope+config.AdminRole, "\r\n\t ") {
		return errors.New("invalid operator token configuration")
	}
	return ValidateRolesClaim(config.RolesClaim)
}

// AllowedAlgorithm reports whether a JWS algorithm may be configured.
func AllowedAlgorithm(algorithm string) bool {
	return algorithm == "RS256" || algorithm == "ES256" || algorithm == "EdDSA" || algorithm == "PS256"
}

// ValidateRolesClaim checks a role claim name or dotted path. An empty value
// selects the default and is accepted.
func ValidateRolesClaim(claim string) error {
	if claim == "" {
		return nil
	}
	if len(claim) > 256 || strings.IndexFunc(claim, unicode.IsSpace) >= 0 {
		return errors.New("invalid operator roles claim")
	}
	for _, segment := range strings.Split(claim, ".") {
		if segment == "" {
			return errors.New("invalid operator roles claim")
		}
	}
	return nil
}

func stringList(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// scopeClaim reads the standard scope claim, which is a space-delimited
// string only; any other type yields no scopes.
func scopeClaim(value any) []string {
	if v, ok := value.(string); ok {
		return strings.Fields(v)
	}
	return nil
}

// scpClaim reads the scp claim: a space-delimited string or an array of
// strings. Empty entries and other types are ignored.
func scpClaim(value any) []string {
	switch v := value.(type) {
	case string:
		return strings.Fields(v)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// roleValues resolves the configured claim. The full string is tried as a
// top-level claim first; only when absent, and the string contains dots, is it
// walked as a path through nested objects. A string value is a single role.
func roleValues(claims jwt.MapClaims, claim string) []string {
	if value, ok := claims[claim]; ok {
		return stringList(value)
	}
	if !strings.Contains(claim, ".") {
		return nil
	}
	var current any = map[string]any(claims)
	for _, segment := range strings.Split(claim, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		if current, ok = object[segment]; !ok {
			return nil
		}
	}
	return stringList(current)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func httpsURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == "" && u.RawQuery == "" && !u.ForceQuery
}
func (a *Authenticator) Authenticate(ctx context.Context, raw string) (admin.Principal, error) {
	if a == nil || a.keys == nil || validateConfig(a.config) != nil || len(raw) == 0 || len(raw) > 8192 {
		return admin.Principal{}, errors.New("invalid operator token")
	}
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, a.keys(ctx), jwt.WithValidMethods(a.config.Algorithms), jwt.WithIssuer(a.config.Issuer), jwt.WithAudience(a.config.Audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(30*time.Second))
	if err != nil || !token.Valid {
		return admin.Principal{}, errors.New("invalid operator token")
	}
	subject, subjectErr := claims.GetSubject()
	issuedAt, issuedAtErr := claims.GetIssuedAt()
	if subjectErr != nil || subject == "" || issuedAtErr != nil || issuedAt == nil {
		return admin.Principal{}, errors.New("invalid operator token")
	}
	scopes := append(scopeClaim(claims["scope"]), scpClaim(claims["scp"])...)
	// Role-only mode still requires a non-empty scope or scp claim. That
	// excludes ID tokens (and Entra app-only tokens), but not client-credentials
	// tokens from providers that put scopes on them; the admin role must not be
	// granted to service accounts.
	allowedScope := len(scopes) > 0
	if a.config.RequiredScope != "" {
		allowedScope = contains(scopes, a.config.RequiredScope)
	}
	rolesClaim := a.config.RolesClaim
	if rolesClaim == "" {
		rolesClaim = DefaultRolesClaim
	}
	allowedRole := contains(roleValues(claims, rolesClaim), a.config.AdminRole)
	return admin.Principal{Subject: subject, Administrator: allowedScope && allowedRole}, nil
}

// signatureKeys restricts verification to keys whose JWK "use" is "sig" or
// omitted (many identity providers omit it); keys marked for encryption or any
// other use are refused. The library's whitelist cannot be set through its
// default-override constructor, so the check is applied here. A token without
// a kid is verified against the whole set, so that path requires every key in
// the set to be permitted.
func signatureKeys(keys keyfunc.Keyfunc) func(context.Context) jwt.Keyfunc {
	permitted := func(use string) bool { return use == "" || use == "sig" }
	return func(ctx context.Context) jwt.Keyfunc {
		inner := keys.KeyfuncCtx(ctx)
		return func(token *jwt.Token) (any, error) {
			if kid, ok := token.Header["kid"].(string); ok {
				jwk, err := keys.Storage().KeyRead(ctx, kid)
				if err != nil || !permitted(string(jwk.Marshal().USE)) {
					return nil, errors.New("unusable signing key")
				}
			} else {
				all, err := keys.Storage().KeyReadAll(ctx)
				if err != nil {
					return nil, errors.New("unusable signing key")
				}
				for _, jwk := range all {
					if !permitted(string(jwk.Marshal().USE)) {
						return nil, errors.New("unusable signing key")
					}
				}
			}
			return inner(token)
		}
	}
}
