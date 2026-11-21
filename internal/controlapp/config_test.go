package controlapp

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/zyno-io/thinpost/internal/submission"
)

func env() map[string]string {
	return map[string]string{"THINPOST_DATABASE_URL": "postgres://user:secret@db.example/thinpost?sslmode=verify-full", "THINPOST_DEPLOYMENT_GENERATION": "00000000000000000000000000000001", "THINPOST_OIDC_ISSUER": "https://issuer.example", "THINPOST_OIDC_AUDIENCE": "api://thinpost", "THINPOST_OIDC_JWKS_URL": "https://issuer.example/keys", "THINPOST_OIDC_ADMIN_SCOPE": "admin", "THINPOST_OIDC_ADMIN_ROLE": "Admin", "THINPOST_OIDC_CLIENT_ID": "client", "THINPOST_OIDC_SCOPES": "openid api://thinpost/admin"}
}

func TestSubmissionRequiresKeyFileAndRoutes(t *testing.T) {
	env := map[string]string{}
	get := func(key string) string { return env[key] }
	if key, err := SubmissionKey(get); err != nil || key != nil {
		t.Fatal("submission enabled by default", err)
	}
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	env["THINPOST_CONTROL_FINGERPRINT_KEY_FILE"] = path
	if _, err := SubmissionKey(get); err == nil {
		t.Fatal("short secret accepted")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := SubmissionKey(get)
	if err != nil || len(key) != 32 {
		t.Fatal("valid secret rejected", err)
	}
	operator := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	submission := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(202) })
	for _, test := range []struct {
		router http.Handler
		path   string
		code   int
	}{
		{Router(operator, nil, nil, nil), "/email", 404},
		{Router(operator, submission, nil, nil), "/email", 202},
		{Router(operator, submission, nil, nil), "/email/batch", 202},
		{Router(operator, submission, nil, nil), "/emails", 202},
		{Router(operator, submission, nil, nil), "/emails/batch", 202},
		{Router(operator, nil, nil, nil), "/emails/batch", 404},
		{Router(operator, submission, nil, nil), "/v3/mail/send", 202},
		{Router(operator, submission, nil, nil), "/readyz", 503},
	} {
		w := httptest.NewRecorder()
		test.router.ServeHTTP(w, httptest.NewRequest("POST", test.path, nil))
		if w.Code != test.code {
			t.Errorf("%s status %d", test.path, w.Code)
		}
	}
}
func load(values map[string]string) (Config, error) {
	return Load(func(key string) string { return values[key] })
}
func TestConfigRequiresAuthenticationAndVerifiedDatabaseTLS(t *testing.T) {
	values := env()
	if _, err := load(values); err != nil {
		t.Fatal(err)
	}
	delete(values, "THINPOST_DEPLOYMENT_GENERATION")
	if _, err := load(values); err == nil {
		t.Fatal("accepted missing deployment generation")
	}
	values = env()
	values["THINPOST_DEPLOYMENT_GENERATION"] = "000000000000000000000000000000AA"
	if _, err := load(values); err == nil {
		t.Fatal("accepted noncanonical deployment generation")
	}
	values = env()
	got, err := load(values)
	if err != nil || got.Auth.RolesClaim != "roles" || got.Auth.RequiredScope != "admin" {
		t.Fatalf("default roles claim: %q %v", got.Auth.RolesClaim, err)
	}
	values["THINPOST_OIDC_ROLES_CLAIM"] = "realm_access.roles"
	if got, err = load(values); err != nil || got.Auth.RolesClaim != "realm_access.roles" {
		t.Fatalf("configured roles claim: %q %v", got.Auth.RolesClaim, err)
	}
	for _, bad := range []string{"a b", "a..b", ".a", "a.", strings.Repeat("a", 257)} {
		values["THINPOST_OIDC_ROLES_CLAIM"] = bad
		if _, err := load(values); err == nil {
			t.Fatalf("accepted roles claim %q", bad)
		}
	}
	for raw, want := range map[string][]string{"": {"RS256"}, "ES256": {"ES256"}, "RS256,PS256": {"RS256", "PS256"}, "RS256 PS256\tEdDSA": {"RS256", "PS256", "EdDSA"}, "RS256, ES256": {"RS256", "ES256"}} {
		values = env()
		if raw != "" {
			values["THINPOST_OIDC_ALGORITHMS"] = raw
		}
		if got, err = load(values); err != nil || strings.Join(got.Auth.Algorithms, ",") != strings.Join(want, ",") {
			t.Fatalf("algorithms %q: %v %v", raw, got.Auth.Algorithms, err)
		}
	}
	for _, bad := range []string{"HS256", "RS256,none", "RS256,RS256", "RS256 RS256", "RS256,,ES256", "RS256,", ",RS256", "\t", "rs256"} {
		values = env()
		values["THINPOST_OIDC_ALGORITHMS"] = bad
		_, err := load(values)
		if err == nil || strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "THINPOST_OIDC_ALGORITHMS") {
			t.Fatalf("algorithms %q: unexpected error %v", bad, err)
		}
	}
	values = env()
	delete(values, "THINPOST_OIDC_ADMIN_SCOPE")
	if _, err = load(values); err == nil {
		t.Fatal("accepted missing admin scope without explicit opt-out")
	}
	values["THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL"] = "true"
	if got, err = load(values); err != nil || got.Auth.RequiredScope != "" {
		t.Fatalf("optional admin scope: %v", err)
	}
	for _, bad := range []string{"false", "1", "TRUE", "yes"} {
		values["THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL"] = bad
		if _, err = load(values); err == nil || !strings.Contains(err.Error(), "THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL") {
			t.Fatalf("optional flag %q: %v", bad, err)
		}
	}
	values = env()
	values["THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL"] = "true"
	if _, err = load(values); err == nil {
		t.Fatal("accepted both admin scope and optional flag")
	}
	values = env()
	values["THINPOST_OIDC_CLIENT_ID"] = values["THINPOST_OIDC_AUDIENCE"]
	if _, err = load(values); err == nil {
		t.Fatal("accepted audience equal to client ID")
	}
	values = env()
	delete(values, "THINPOST_OIDC_ADMIN_ROLE")
	if _, err := load(values); err == nil {
		t.Fatal("accepted missing operator role")
	}
	values = env()
	values["THINPOST_DATABASE_URL"] = "postgres://user:secret@db.example/thinpost?sslmode=disable"
	values["THINPOST_ALLOW_INSECURE_LOCAL_DB"] = "1"
	if _, err := load(values); err == nil {
		t.Fatal("allowed insecure remote database")
	}
	values["THINPOST_DATABASE_URL"] = "postgres://user:secret@127.0.0.1/thinpost?sslmode=disable"
	if _, err := load(values); err != nil {
		t.Fatal("rejected explicit local development override")
	}
	values["THINPOST_DATABASE_URL"] = "postgres://user:secret@db.example/thinpost?sslmode=verify-full&sslmode=disable"
	if _, err := load(values); err == nil {
		t.Fatal("accepted ambiguous TLS settings")
	}
}

func TestRemovedExperimentalFlagsAreIgnored(t *testing.T) {
	values := env()
	for _, name := range []string{"THINPOST_EXPERIMENTAL_ATTACHMENTS", "THINPOST_EXPERIMENTAL_REPLY_TO", "THINPOST_EXPERIMENTAL_SUBMISSION", "THINPOST_EXPERIMENTAL_TRACKING"} {
		values[name] = "true"
	}
	if _, err := load(values); err != nil {
		t.Fatal("removed experimental flags should be ignored", err)
	}
}

func TestReadinessFailsUntilDeliveryIsIntegrated(t *testing.T) {
	router := Router(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), nil, nil, nil)
	for path, want := range map[string]int{"/livez": 204, "/readyz": 503, "/api/v1/config": 204} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Fatalf("%s status %d", path, w.Code)
		}
	}
}

func TestOperatorReadinessDoesNotSignalMailReadiness(t *testing.T) {
	operator := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, tc := range []struct {
		name  string
		check func() bool
		want  int
	}{
		{"healthy database", func() bool { return true }, http.StatusNoContent},
		{"unavailable database", func() bool { return false }, http.StatusServiceUnavailable},
		{"missing check", nil, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := Router(operator, nil, nil, tc.check)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/operator-readyz", nil))
			if w.Code != tc.want || strings.Contains(w.Body.String(), "private database detail") {
				t.Fatalf("operator readiness: %d %q", w.Code, w.Body.String())
			}
			w = httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatal("operator readiness changed mail readiness")
			}
		})
	}
}

func TestObjectStorageRequiresCompleteSecureConfiguration(t *testing.T) {
	values := env()
	values["THINPOST_OBJECT_KEYRING_FILE"] = "/run/secrets/thinpost/object-keys.json"
	if _, err := load(values); err == nil {
		t.Fatal("keyring without object storage accepted")
	}
	delete(values, "THINPOST_OBJECT_KEYRING_FILE")
	values["THINPOST_S3_ENDPOINT"] = "s3.example:443"
	if _, err := load(values); err == nil {
		t.Fatal("partial object storage configuration accepted")
	}
	values["THINPOST_S3_BUCKET"] = "thinpost"
	values["THINPOST_S3_ACCESS_KEY"] = "test"
	values["THINPOST_S3_SECRET_KEY"] = "test"
	values["THINPOST_S3_REGION"] = "us-east-1"
	if _, err := load(values); err == nil {
		t.Fatal("object storage without an encryption keyring accepted")
	}
	values["THINPOST_OBJECT_KEYRING_FILE"] = "relative"
	if _, err := load(values); err == nil {
		t.Fatal("relative object encryption keyring path accepted")
	}
	values["THINPOST_OBJECT_KEYRING_FILE"] = "/run/secrets/thinpost/object-keys.json"
	values["THINPOST_S3_REGION"] = ""
	if _, err := load(values); err == nil {
		t.Fatal("object storage without an explicit region accepted")
	}
	values["THINPOST_S3_REGION"] = "us-east-1"
	config, err := load(values)
	if err != nil || config.S3 == nil || !config.S3.Secure {
		t.Fatal("secure object storage rejected")
	}
	values["THINPOST_ALLOW_INSECURE_LOCAL_S3"] = "1"
	if _, err = load(values); err == nil {
		t.Fatal("allowed insecure remote object storage")
	}
	values["THINPOST_S3_ENDPOINT"] = "127.0.0.1:9000"
	if _, err = load(values); err == nil {
		t.Fatal("allowed insecure local storage with remote database")
	}
	values["THINPOST_DATABASE_URL"] = "postgres://user:secret@127.0.0.1/thinpost?sslmode=disable"
	values["THINPOST_ALLOW_INSECURE_LOCAL_DB"] = "1"
	config, err = load(values)
	if err != nil || config.S3 == nil || config.S3.Secure {
		t.Fatal("local test storage not allowed")
	}
}

func TestFingerprintKeyringConfiguration(t *testing.T) {
	values := map[string]string{}
	get := func(key string) string { return values[key] }
	path := filepath.Join(t.TempDir(), "keyring")
	values["THINPOST_CONTROL_FINGERPRINT_KEYS_FILE"] = path
	keys := [][]byte{[]byte(strings.Repeat("a", 32)), []byte(strings.Repeat("b", 32))}
	encoded, err := json.Marshal([]string{base64.StdEncoding.EncodeToString(keys[0]), base64.StdEncoding.EncodeToString(keys[1])})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := SubmissionFingerprintKeys(get)
	if err != nil || len(got) != 2 || string(got[0]) != string(keys[0]) || string(got[1]) != string(keys[1]) {
		t.Fatal("valid keyring rejected", err)
	}
	values["THINPOST_CONTROL_FINGERPRINT_KEY_FILE"] = path
	if _, err = SubmissionFingerprintKeys(get); err == nil {
		t.Fatal("both key formats accepted")
	}
	delete(values, "THINPOST_CONTROL_FINGERPRINT_KEY_FILE")
	for _, invalid := range []string{"[]", "null", "[\"bad!\"]", "[\"YQ==\"]", "{}", string(encoded) + "{}"} {
		if err = os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = SubmissionFingerprintKeys(get); err == nil {
			t.Fatalf("accepted invalid keyring %q", invalid)
		}
	}
}

func TestDatabaseURLRejectsDriverOverrides(t *testing.T) {
	for _, raw := range []string{
		"postgres://user:secret@db.example/thinpost?sslmode=verify-full&ssl=true",
		"postgres://user:secret@db.example/thinpost?sslmode=verify-full&%20ssl%20=true",
		"postgres://user:secret@db.example/thinpost?sslmode=verify-full&host=%2Ftmp",
		"postgres://user:secret@127.0.0.1/thinpost?sslmode=disable&host=db.example",
		"postgres://user:secret@db.example/thinpost?sslmode=verify-full&service=other",
		"postgres://user:secret@db.example,other.example/thinpost?sslmode=verify-full",
		"postgres://user@other@db.example/thinpost?sslmode=verify-full",
		"postgres://127.0.0.1?sslmode=disable&application_name=x@remote.example",
		"postgres://user:secret@db.example/thinpost?sslmode=verify-full&sslrootcert=a&sslrootcert=b",
		"postgres://user:secret@db.example/thinpost?sslmode=verify-full&search_path=public,thinpost",
		"postgres://user:secret@db.example/thinpost?sslmode=verify-full;ssl=true",
		"postgres://user:secret@db.example/thinpost?sslmode=verify-full&default_query_exec_mode=simple_protocol",
		"postgres://user:secret@db.example/thinpost?sslmode=verify-full&default_query_exec_mode=unknown",
	} {
		t.Run(raw, func(t *testing.T) {
			get := func(key string) string {
				if key == "THINPOST_DATABASE_URL" {
					return raw
				}
				if key == "THINPOST_ALLOW_INSECURE_LOCAL_DB" {
					return "1"
				}
				return ""
			}
			if _, err := DatabaseURL(get); err == nil {
				t.Fatal("accepted driver override")
			}
		})
	}
	good := "postgres://user:secret@db.example/thinpost?sslmode=verify-full&sslrootcert=%2Frun%2Fca.crt&default_query_exec_mode=exec&search_path=thinpost"
	if _, err := DatabaseURL(func(key string) string {
		if key == "THINPOST_DATABASE_URL" {
			return good
		}
		return ""
	}); err != nil {
		t.Fatal("rejected supported pooler URL", err)
	}
	t.Setenv("PGSERVICE", "")
	verified := "postgres://user:secret@db.example/thinpost?sslmode=verify-full&default_query_exec_mode=exec&search_path=thinpost"
	if _, err := DatabaseURL(func(key string) string {
		if key == "THINPOST_DATABASE_URL" {
			return verified
		}
		return ""
	}); err != nil {
		t.Fatal(err)
	}
	parsed, err := pgx.ParseConfig(verified)
	if err != nil || parsed.Host != "db.example" || parsed.TLSConfig == nil || parsed.TLSConfig.InsecureSkipVerify || parsed.TLSConfig.ServerName != "db.example" || len(parsed.Fallbacks) != 0 || parsed.RuntimeParams["search_path"] != "thinpost" {
		t.Fatal("driver did not preserve verified host and TLS policy", err)
	}
}

func TestConsoleConnectOrigins(t *testing.T) {
	values := env()
	config, err := load(values)
	if err != nil || len(config.ConsoleConnectOrigins) != 0 {
		t.Fatal("origins must default to none", err)
	}
	values["THINPOST_CONSOLE_CONNECT_ORIGINS"] = "https://token.example.com, https://idp.example.net:8443"
	config, err = load(values)
	if err != nil || len(config.ConsoleConnectOrigins) != 2 || config.ConsoleConnectOrigins[1] != "https://idp.example.net:8443" {
		t.Fatal("rejected valid origins", config.ConsoleConnectOrigins, err)
	}
	csp, err := consoleCSP(ConsolePolicy{ConnectOrigins: config.ConsoleConnectOrigins})
	if err != nil || !strings.Contains(csp, "connect-src 'self' https://token.example.com https://idp.example.net:8443;") {
		t.Fatal("origins missing from CSP", csp, err)
	}
	for _, bad := range []string{"http://token.example.com", "https://token.example.com/path", "https://token.example.com/", "https://*.example.com", "token.example.com", "https://u:p@token.example.com", "https://token.example.com?x=1", "https://a.example.com,", "*", "https://a.example.com;script-src *"} {
		values["THINPOST_CONSOLE_CONNECT_ORIGINS"] = bad
		if _, err := load(values); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestAdminOrigin(t *testing.T) {
	values := env()
	config, err := load(values)
	if err != nil || config.AdminHost != "" {
		t.Fatal("admin origin must default to none", err)
	}
	for origin, want := range map[string]string{
		"https://admin.example.com":      "admin.example.com",
		"https://Admin.Example.com:8443": "admin.example.com",
		"https://admin.example.com.:443": "admin.example.com",
		"https://[2001:db8::1]:8443":     "[2001:db8::1]",
		"https://[2001:DB8:0::1]":        "[2001:db8::1]",
		"https://xn--bcher-kva.example":  "xn--bcher-kva.example",
	} {
		values["THINPOST_ADMIN_ORIGIN"] = origin
		config, err = load(values)
		if err != nil || config.AdminHost != want {
			t.Errorf("%q: %q %v", origin, config.AdminHost, err)
		}
	}
	for _, bad := range []string{"http://admin.example.com", "https://admin.example.com/", "https://admin.example.com/console", "admin.example.com", "https://u:p@admin.example.com", "https://admin.example.com?x=1", "https://*.example.com", "https://a.example.com,https://b.example.com", "https://.", "https://...", "https://.:443", "https://bücher.example", "https://admin_x.example.com", "https://[fe80::1%25eth0]"} {
		values["THINPOST_ADMIN_ORIGIN"] = bad
		if _, err := load(values); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestMaxAttachmentBytesConfiguration(t *testing.T) {
	values := env()
	config, err := load(values)
	if err != nil || config.MaxAttachmentBytes != 5242880 {
		t.Fatal("default must be 5 MiB", config.MaxAttachmentBytes, err)
	}
	for raw, want := range map[string]int{"65536": 65536, "1048576": 1048576, "5767168": submission.MaxAttachmentBytes} {
		values["THINPOST_MAX_ATTACHMENT_BYTES"] = raw
		config, err = load(values)
		if err != nil || config.MaxAttachmentBytes != want {
			t.Fatalf("%s: %d %v", raw, config.MaxAttachmentBytes, err)
		}
	}
	for _, raw := range []string{"0", "-1", "65535", "5767169", "99999999999999999999", "5MiB", "1.5e6", " 1048576", "+1048576", "01048576"} {
		values["THINPOST_MAX_ATTACHMENT_BYTES"] = raw
		if _, err = load(values); err == nil || !strings.Contains(err.Error(), "THINPOST_MAX_ATTACHMENT_BYTES") {
			t.Fatalf("%q accepted or unclear error: %v", raw, err)
		}
	}
}
