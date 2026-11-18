package operatorauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestDelegatedAccessTokenValidation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Issuer: "https://issuer.example/tenant", Audience: "api://thinpost", JWKSURL: "https://issuer.example/keys", RequiredScope: "thinpost.admin", AdminRole: "Thinpost.Admin", Algorithms: []string{"RS256"}}
	auth := &Authenticator{config: config, keys: func(context.Context) jwt.Keyfunc { return func(*jwt.Token) (any, error) { return &key.PublicKey, nil } }}
	now := time.Now()
	base := func() jwt.MapClaims {
		return jwt.MapClaims{"iss": config.Issuer, "aud": config.Audience, "sub": "operator-1", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "scp": "openid thinpost.admin", "roles": []string{"Thinpost.Admin"}}
	}
	sign := func(claims jwt.MapClaims) string {
		token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	principal, err := auth.Authenticate(context.Background(), sign(base()))
	if err != nil || !principal.Administrator || principal.Subject != "operator-1" {
		t.Fatal("valid delegated admin token rejected")
	}
	for _, tc := range []struct {
		name      string
		mutate    func(jwt.MapClaims)
		wantError bool
	}{
		{"wrong audience", func(c jwt.MapClaims) { c["aud"] = "browser-client-id" }, true},
		{"wrong issuer", func(c jwt.MapClaims) { c["iss"] = "https://other.example" }, true},
		{"expired", func(c jwt.MapClaims) { c["exp"] = now.Add(-time.Hour).Unix() }, true},
		{"missing issued-at", func(c jwt.MapClaims) { delete(c, "iat") }, true},
		{"missing subject", func(c jwt.MapClaims) { delete(c, "sub") }, true},
		{"missing scope", func(c jwt.MapClaims) { delete(c, "scp") }, false},
		{"missing role", func(c jwt.MapClaims) { delete(c, "roles") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			p, err := auth.Authenticate(context.Background(), sign(c))
			if tc.wantError && err == nil {
				t.Fatal("invalid token accepted")
			}
			if !tc.wantError && (err != nil || p.Administrator) {
				t.Fatal("unauthorized token received admin rights")
			}
		})
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := jwt.NewWithClaims(jwt.SigningMethodRS256, base()).SignedString(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = auth.Authenticate(context.Background(), forged); err == nil {
		t.Fatal("forged signature accepted")
	}
}
func TestConfigurationFailsClosed(t *testing.T) {
	valid := Config{Issuer: "https://issuer.example", Audience: "api://thinpost", JWKSURL: "https://issuer.example/keys", RequiredScope: "admin", AdminRole: "admin", Algorithms: []string{"RS256"}}
	if err := validateConfig(valid); err != nil {
		t.Fatal(err)
	}
	bad := valid
	bad.JWKSURL = "http://issuer.example/keys"
	if validateConfig(bad) == nil {
		t.Fatal("accepted insecure JWKS")
	}
	bad = valid
	bad.Algorithms = []string{"HS256"}
	if validateConfig(bad) == nil {
		t.Fatal("accepted symmetric algorithm")
	}
	bad = valid
	bad.Audience = ""
	if validateConfig(bad) == nil {
		t.Fatal("accepted missing audience")
	}
}

func TestHTTPSJWKSValidation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyN := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	keyE := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"RSA","kid":"key-1","alg":"RS256","use":"sig","n":%q,"e":%q}]}`, keyN, keyE)
	}))
	defer server.Close()
	config := Config{Issuer: "https://issuer.example", Audience: "api://thinpost", JWKSURL: server.URL, RequiredScope: "admin", AdminRole: "Admin", Algorithms: []string{"RS256"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth, err := newWithClient(ctx, config, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": config.Issuer, "aud": config.Audience, "sub": "operator", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "scp": "admin", "roles": []string{"Admin"}})
	token.Header["kid"] = "key-1"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.Authenticate(ctx, raw)
	if err != nil || !principal.Administrator {
		t.Fatalf("JWKS token: %v", err)
	}
}

func newTestAuth(t *testing.T, config Config, method jwt.SigningMethod) (*Authenticator, func(jwt.MapClaims) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	auth := &Authenticator{config: config, keys: func(context.Context) jwt.Keyfunc { return func(*jwt.Token) (any, error) { return &key.PublicKey, nil } }}
	return auth, func(c jwt.MapClaims) string {
		now := time.Now()
		c["iss"], c["aud"], c["sub"], c["iat"], c["exp"] = config.Issuer, config.Audience, "operator-1", now.Unix(), now.Add(time.Hour).Unix()
		raw, err := jwt.NewWithClaims(method, c).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
}

func genericConfig() Config {
	return Config{Issuer: "https://issuer.example", Audience: "api://thinpost", JWKSURL: "https://issuer.example/keys", RequiredScope: "thinpost.admin", AdminRole: "admin", Algorithms: []string{"RS256"}}
}

func TestScopeAndRoleClaims(t *testing.T) {
	arr := []any{"thinpost.admin"}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		claims jwt.MapClaims
		admin  bool
	}{
		{"scope string", nil, jwt.MapClaims{"scope": "openid thinpost.admin", "roles": []string{"admin"}}, true},
		{"scp string", nil, jwt.MapClaims{"scp": "openid thinpost.admin", "roles": []string{"admin"}}, true},
		{"scp array", nil, jwt.MapClaims{"scp": arr, "roles": []string{"admin"}}, true},
		{"scp array wins when scope does not match", nil, jwt.MapClaims{"scope": "other", "scp": arr, "roles": []string{"admin"}}, true},
		{"both present, scope matches", nil, jwt.MapClaims{"scope": "thinpost.admin", "scp": "other", "roles": []string{"admin"}}, true},
		{"both present, neither matches", nil, jwt.MapClaims{"scope": "a", "scp": "b", "roles": []string{"admin"}}, false},
		{"scope wrong type number", nil, jwt.MapClaims{"scope": 5, "roles": []string{"admin"}}, false},
		{"scp wrong type object", nil, jwt.MapClaims{"scp": map[string]any{"x": "thinpost.admin"}, "roles": []string{"admin"}}, false},
		{"scp array with non-strings", nil, jwt.MapClaims{"scp": []any{1, "thinpost.admin"}, "roles": []string{"admin"}}, true},
		{"scope substring is not a match", nil, jwt.MapClaims{"scope": "thinpost.admin2", "roles": []string{"admin"}}, false},
		{"roles wrong type", nil, jwt.MapClaims{"scope": "thinpost.admin", "roles": 7}, false},
		{"roles object", nil, jwt.MapClaims{"scope": "thinpost.admin", "roles": map[string]any{"admin": true}}, false},
		{"roles string is one role", nil, jwt.MapClaims{"scope": "thinpost.admin", "roles": "admin"}, true},
		{"roles string is not split", nil, jwt.MapClaims{"scope": "thinpost.admin", "roles": "admin other"}, false},
		{"groups", func(c *Config) { c.RolesClaim = "groups" }, jwt.MapClaims{"scope": "thinpost.admin", "groups": []string{"admin"}}, true},
		{"groups ignores roles", func(c *Config) { c.RolesClaim = "groups" }, jwt.MapClaims{"scope": "thinpost.admin", "roles": []string{"admin"}}, false},
		{"nested realm_access.roles", func(c *Config) { c.RolesClaim = "realm_access.roles" }, jwt.MapClaims{"scope": "thinpost.admin", "realm_access": map[string]any{"roles": []string{"admin"}}}, true},
		{"nested missing", func(c *Config) { c.RolesClaim = "realm_access.roles" }, jwt.MapClaims{"scope": "thinpost.admin", "realm_access": map[string]any{"other": []string{"admin"}}}, false},
		{"nested through non-object", func(c *Config) { c.RolesClaim = "realm_access.roles" }, jwt.MapClaims{"scope": "thinpost.admin", "realm_access": "admin"}, false},
		{"namespaced URL claim top-level", func(c *Config) { c.RolesClaim = "https://example.com/roles" }, jwt.MapClaims{"scope": "thinpost.admin", "https://example.com/roles": []string{"admin"}}, true},
		{"top-level dotted wins over path", func(c *Config) { c.RolesClaim = "a.b" }, jwt.MapClaims{"scope": "thinpost.admin", "a.b": []string{"other"}, "a": map[string]any{"b": []string{"admin"}}}, false},
		{"path walked when top-level absent", func(c *Config) { c.RolesClaim = "a.b" }, jwt.MapClaims{"scope": "thinpost.admin", "a": map[string]any{"b": []string{"admin"}}}, true},
		{"top-level wrong type does not fall back to path", func(c *Config) { c.RolesClaim = "a.b" }, jwt.MapClaims{"scope": "thinpost.admin", "a.b": 1, "a": map[string]any{"b": []string{"admin"}}}, false},
		{"optional scope empty, role only", func(c *Config) { c.RequiredScope = "" }, jwt.MapClaims{"scope": "openid", "roles": []string{"admin"}}, true},
		{"role-only scp array", func(c *Config) { c.RequiredScope = "" }, jwt.MapClaims{"scp": []any{"x"}, "roles": []string{"admin"}}, true},
		{"role-only ID token", func(c *Config) { c.RequiredScope = "" }, jwt.MapClaims{"nonce": "n", "roles": []string{"admin"}}, false},
		{"role-only app-only token", func(c *Config) { c.RequiredScope = "" }, jwt.MapClaims{"roles": []string{"admin"}}, false},
		{"role-only empty scope string", func(c *Config) { c.RequiredScope = "" }, jwt.MapClaims{"scope": "  ", "roles": []string{"admin"}}, false},
		{"role-only empty scp entries", func(c *Config) { c.RequiredScope = "" }, jwt.MapClaims{"scp": []any{"", 1}, "roles": []string{"admin"}}, false},
		{"role-only scope array is not a scope", func(c *Config) { c.RequiredScope = "" }, jwt.MapClaims{"scope": []any{"x"}, "roles": []string{"admin"}}, false},
		{"Entra scope array is non-admin", func(c *Config) { c.RequiredScope, c.AdminRole = "access", "Thinpost.Admin" }, jwt.MapClaims{"scope": []any{"access"}, "roles": []string{"Thinpost.Admin"}}, false},
		{"optional scope empty, no role", func(c *Config) { c.RequiredScope = "" }, jwt.MapClaims{"scope": "thinpost.admin"}, false},
		{"scope set, role only", nil, jwt.MapClaims{"roles": []string{"admin"}}, false},
		{"scope set, scope only", nil, jwt.MapClaims{"scope": "thinpost.admin"}, false},
		{"Entra regression", func(c *Config) { c.RequiredScope, c.AdminRole = "access", "Thinpost.Admin" }, jwt.MapClaims{"scp": "openid access", "roles": []string{"Thinpost.Admin"}}, true},
		{"Entra regression missing role", func(c *Config) { c.RequiredScope, c.AdminRole = "access", "Thinpost.Admin" }, jwt.MapClaims{"scp": "access"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := genericConfig()
			if tc.mutate != nil {
				tc.mutate(&config)
			}
			auth, sign := newTestAuth(t, config, jwt.SigningMethodRS256)
			p, err := auth.Authenticate(context.Background(), sign(tc.claims))
			if err != nil {
				t.Fatalf("token rejected: %v", err)
			}
			if p.Administrator != tc.admin || p.Subject != "operator-1" {
				t.Fatalf("administrator = %v, want %v", p.Administrator, tc.admin)
			}
		})
	}
}

func TestOptionalScopeKeepsMandatoryChecks(t *testing.T) {
	config := genericConfig()
	config.RequiredScope = ""
	auth, sign := newTestAuth(t, config, jwt.SigningMethodRS256)
	bad := sign(jwt.MapClaims{"scope": "openid", "roles": []string{"admin"}})
	// Re-sign with a wrong audience via a mutated config on the same key.
	wrong := *auth
	wrong.config.Audience = "other"
	if _, err := wrong.Authenticate(context.Background(), bad); err == nil {
		t.Fatal("wrong audience accepted without scope")
	}
	wrong = *auth
	wrong.config.Issuer = "https://other.example"
	if _, err := wrong.Authenticate(context.Background(), bad); err == nil {
		t.Fatal("wrong issuer accepted without scope")
	}
}

func TestPS256(t *testing.T) {
	config := genericConfig()
	config.Algorithms = []string{"PS256"}
	if err := validateConfig(config); err != nil {
		t.Fatal(err)
	}
	auth, sign := newTestAuth(t, config, jwt.SigningMethodPS256)
	p, err := auth.Authenticate(context.Background(), sign(jwt.MapClaims{"scope": "thinpost.admin", "roles": []string{"admin"}}))
	if err != nil || !p.Administrator {
		t.Fatalf("PS256 token rejected: %v", err)
	}
	// RS256 token must be refused when only PS256 is allowed.
	rsAuth, rsSign := newTestAuth(t, genericConfig(), jwt.SigningMethodRS256)
	rsAuth.config.Algorithms = []string{"PS256"}
	if _, err := rsAuth.Authenticate(context.Background(), rsSign(jwt.MapClaims{"roles": []string{"admin"}})); err == nil {
		t.Fatal("RS256 accepted when only PS256 configured")
	}
}

func TestRolesClaimConfigValidation(t *testing.T) {
	long := ""
	for i := 0; i < 257; i++ {
		long += "a"
	}
	for claim, ok := range map[string]bool{
		"": true, "roles": true, "groups": true, "realm_access.roles": true, "https://example.com/roles": true,
		"a b": false, "a\tb": false, "a\nb": false, ".roles": false, "roles.": false, "a..b": false, ".": false, long: false, long[:256]: true,
	} {
		config := genericConfig()
		config.RolesClaim = claim
		if err := validateConfig(config); (err == nil) != ok {
			t.Fatalf("claim %q: err=%v want ok=%v", claim, err, ok)
		}
	}
	config := genericConfig()
	config.RolesClaim = "bad claim"
	if _, err := New(context.Background(), config); err == nil {
		t.Fatal("New accepted invalid roles claim")
	}
	config = genericConfig()
	config.RequiredScope = ""
	if err := validateConfig(config); err != nil {
		t.Fatal("empty scope must be allowed")
	}
}

func TestHS256WithPublicKeyPEMRejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	config := genericConfig()
	for _, algorithms := range [][]string{{"RS256"}, {"RS256", "ES256", "EdDSA", "PS256"}} {
		config.Algorithms = algorithms
		auth := &Authenticator{config: config, keys: func(context.Context) jwt.Keyfunc { return func(*jwt.Token) (any, error) { return &key.PublicKey, nil } }}
		now := time.Now()
		for _, secret := range []any{pemBytes, der} {
			raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": config.Issuer, "aud": config.Audience, "sub": "x", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "scope": "thinpost.admin", "roles": []string{"admin"}}).SignedString(secret)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := auth.Authenticate(context.Background(), raw); err == nil {
				t.Fatal("HS256 token signed with the public key was accepted")
			}
		}
	}
}

func TestJWKSKeyUse(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
	for _, tc := range []struct {
		use string
		ok  bool
	}{{`"use":"sig",`, true}, {``, true}, {`"use":"enc",`, false}} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"RSA","kid":"key-1",%s"n":%q,"e":%q}]}`, tc.use, n, e)
		}))
		config := genericConfig()
		config.JWKSURL = server.URL
		ctx, cancel := context.WithCancel(context.Background())
		auth, err := newWithClient(ctx, config, server.Client())
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": config.Issuer, "aud": config.Audience, "sub": "op", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "scope": "thinpost.admin", "roles": []string{"admin"}})
		token.Header["kid"] = "key-1"
		raw, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		p, err := auth.Authenticate(ctx, raw)
		if tc.ok != (err == nil && p.Administrator) {
			t.Fatalf("use %q: ok=%v err=%v", tc.use, tc.ok, err)
		}
		cancel()
		server.Close()
	}
}
