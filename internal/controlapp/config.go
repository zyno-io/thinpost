// Package controlapp configures the development-stage control-plane process.
// It exposes the operator API and an optional same-origin operator console;
// mail delivery is not yet wired end to end.
package controlapp

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/operatorauth"
	"github.com/zyno-io/thinpost/internal/submission"
)

type Config struct {
	Listen, DatabaseURL, Generation string
	// TLSListen, TLSCertFile and TLSKeyFile configure the optional second,
	// HTTPS listener (THINPOST_CONTROL_TLS_LISTEN, _CERT, _KEY). All empty
	// means plain HTTP only.
	TLSListen, TLSCertFile, TLSKeyFile string
	// MaxAttachmentBytes is the admission limit on total decoded attachment
	// bytes per message (THINPOST_MAX_ATTACHMENT_BYTES).
	MaxAttachmentBytes int
	// UIDir optionally holds the built operator console served on the same
	// origin as the operator API.
	UIDir string
	// ConsoleConnectOrigins are extra https origins the console may fetch (for
	// example an IdP token endpoint on a different origin than the issuer).
	ConsoleConnectOrigins []string
	// AdminHost is the hostname of THINPOST_ADMIN_ORIGIN, normalized for
	// matching. Empty means the console and operator API answer on every host.
	AdminHost  string
	Auth       operatorauth.Config
	PublicAuth admin.PublicAuthConfig
	S3         *S3Config
	Tracking   *TrackingConfig
	// WebhookDestinations is the operator's exact-match internal webhook
	// exception list (THINPOST_WEBHOOK_INTERNAL_DESTINATIONS), constrained at
	// dial time to THINPOST_SERVICE_CIDRS. Nil means public HTTPS only.
	WebhookDestinations *admin.WebhookDestinations
}
type S3Config struct {
	Endpoint, Bucket, Region, AccessKey, SecretKey, KeyringFile string
	Secure                                                      bool
}

// Load requires verified database TLS by default. The insecure switch is for a
// disposable local database only, not staging or production.
func Load(getenv func(string) string) (Config, error) {
	databaseURL, err := DatabaseURL(getenv)
	if err != nil {
		return Config{}, err
	}
	config := Config{Listen: getenv("THINPOST_LISTEN"), DatabaseURL: databaseURL, Generation: getenv("THINPOST_DEPLOYMENT_GENERATION")}
	for _, name := range []string{"THINPOST_DELIVERY_MAX_ATTEMPTS", "THINPOST_DELIVERY_RETRY_INITIAL_DELAY", "THINPOST_DELIVERY_RETRY_MAX_DELAY", "THINPOST_DELIVERY_RETRY_MAX_AGE"} {
		if getenv(name) != "" {
			return Config{}, errors.New("delivery retry policy is managed in the database")
		}
	}
	config.MaxAttachmentBytes = submission.DefaultMaxAttachmentBytes
	if raw := getenv("THINPOST_MAX_ATTACHMENT_BYTES"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || strconv.Itoa(limit) != raw || limit < submission.MinAttachmentLimit || limit > submission.MaxAttachmentBytes {
			return Config{}, fmt.Errorf("THINPOST_MAX_ATTACHMENT_BYTES must be a plain integer from %d to %d bytes", submission.MinAttachmentLimit, submission.MaxAttachmentBytes)
		}
		config.MaxAttachmentBytes = limit
	}
	// THINPOST_INBOUND_MX_HOST from an earlier release is never read: the inbound
	// mail host is the main IP's HELO name as the Edge reports it. It may stay
	// set while new pods roll out.
	config.WebhookDestinations, err = admin.ParseWebhookDestinations(getenv("THINPOST_WEBHOOK_INTERNAL_DESTINATIONS"), getenv("THINPOST_SERVICE_CIDRS"))
	if err != nil {
		return Config{}, err
	}
	config.Tracking, err = loadTracking(getenv)
	if err != nil {
		return Config{}, err
	}
	generation, generationErr := hex.DecodeString(config.Generation)
	if generationErr != nil || len(generation) != 16 || len(config.Generation) != 32 || config.Generation != strings.ToLower(config.Generation) {
		return Config{}, errors.New("deployment generation must be a lowercase 128-bit identifier")
	}
	if config.Listen == "" {
		config.Listen = "127.0.0.1:8080"
	}
	config.TLSListen, config.TLSCertFile, config.TLSKeyFile = getenv("THINPOST_CONTROL_TLS_LISTEN"), getenv("THINPOST_CONTROL_TLS_CERT"), getenv("THINPOST_CONTROL_TLS_KEY")
	if config.TLSListen != "" || config.TLSCertFile != "" || config.TLSKeyFile != "" {
		if !filepath.IsAbs(config.TLSCertFile) || !filepath.IsAbs(config.TLSKeyFile) {
			return Config{}, errors.New("THINPOST_CONTROL_TLS_CERT and THINPOST_CONTROL_TLS_KEY must both be absolute paths when control TLS is configured")
		}
		if config.TLSListen == "" {
			config.TLSListen = ":8443"
		}
		if config.TLSListen == config.Listen {
			return Config{}, errors.New("THINPOST_CONTROL_TLS_LISTEN must differ from THINPOST_LISTEN")
		}
	}
	config.UIDir = getenv("THINPOST_UI_DIR")
	if config.UIDir != "" && !filepath.IsAbs(config.UIDir) {
		return Config{}, errors.New("operator console directory must be absolute")
	}
	if raw := getenv("THINPOST_CONSOLE_CONNECT_ORIGINS"); raw != "" {
		for _, origin := range strings.Split(raw, ",") {
			if !validConnectOrigin(strings.TrimSpace(origin)) {
				return Config{}, errors.New("invalid console connect origin")
			}
			config.ConsoleConnectOrigins = append(config.ConsoleConnectOrigins, strings.TrimSpace(origin))
		}
	}
	if raw := getenv("THINPOST_ADMIN_ORIGIN"); raw != "" {
		host, ok := adminHostname(raw)
		if !ok {
			return Config{}, errors.New("invalid admin origin")
		}
		config.AdminHost = host
	}
	scopeOptional := getenv("THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL")
	if scopeOptional != "" && scopeOptional != "true" {
		return Config{}, errors.New("THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL must be exactly true when set")
	}
	if scopeOptional == "true" && getenv("THINPOST_OIDC_ADMIN_SCOPE") != "" {
		return Config{}, errors.New("THINPOST_OIDC_ADMIN_SCOPE and THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL are mutually exclusive")
	}
	if scopeOptional != "true" && getenv("THINPOST_OIDC_ADMIN_SCOPE") == "" {
		return Config{}, errors.New("operator authentication configuration is incomplete")
	}
	algorithms, err := oidcAlgorithms(getenv("THINPOST_OIDC_ALGORITHMS"))
	if err != nil {
		return Config{}, err
	}
	config.Auth = operatorauth.Config{Issuer: getenv("THINPOST_OIDC_ISSUER"), Audience: getenv("THINPOST_OIDC_AUDIENCE"), JWKSURL: getenv("THINPOST_OIDC_JWKS_URL"), RequiredScope: getenv("THINPOST_OIDC_ADMIN_SCOPE"), AdminRole: getenv("THINPOST_OIDC_ADMIN_ROLE"), RolesClaim: getenv("THINPOST_OIDC_ROLES_CLAIM"), Algorithms: algorithms}
	if config.Auth.RolesClaim == "" {
		config.Auth.RolesClaim = operatorauth.DefaultRolesClaim
	}
	config.PublicAuth = admin.PublicAuthConfig{Issuer: config.Auth.Issuer, ClientID: getenv("THINPOST_OIDC_CLIENT_ID"), Scopes: strings.Fields(getenv("THINPOST_OIDC_SCOPES"))}
	if config.Auth.Issuer == "" || config.Auth.Audience == "" || config.Auth.JWKSURL == "" || config.Auth.AdminRole == "" || operatorauth.ValidateRolesClaim(config.Auth.RolesClaim) != nil || !admin.ValidPublicAuth(&config.PublicAuth) {
		return Config{}, errors.New("operator authentication configuration is incomplete")
	}
	if config.PublicAuth.ClientID != "" && config.Auth.Audience == config.PublicAuth.ClientID {
		return Config{}, errors.New("operator authentication configuration is invalid")
	}
	s3, err := objectStorageConfig(getenv, databaseURL)
	if err != nil {
		return Config{}, err
	}
	config.S3 = s3
	return config, nil
}

// oidcAlgorithms parses THINPOST_OIDC_ALGORITHMS: comma- and/or
// whitespace-separated, defaulting to RS256 when unset. Empty entries,
// duplicates and algorithms outside the operatorauth allowed set are rejected.
func oidcAlgorithms(raw string) ([]string, error) {
	if raw == "" {
		return []string{"RS256"}, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		fields := strings.Fields(item)
		if len(fields) == 0 {
			return nil, errors.New("THINPOST_OIDC_ALGORITHMS is invalid")
		}
		for _, algorithm := range fields {
			if !operatorauth.AllowedAlgorithm(algorithm) || seen[algorithm] {
				return nil, errors.New("THINPOST_OIDC_ALGORITHMS is invalid")
			}
			seen[algorithm] = true
			out = append(out, algorithm)
		}
	}
	return out, nil
}

func objectStorageConfig(getenv func(string) string, databaseURL string) (*S3Config, error) {
	config := &S3Config{Endpoint: getenv("THINPOST_S3_ENDPOINT"), Bucket: getenv("THINPOST_S3_BUCKET"), Region: getenv("THINPOST_S3_REGION"), AccessKey: getenv("THINPOST_S3_ACCESS_KEY"), SecretKey: getenv("THINPOST_S3_SECRET_KEY"), KeyringFile: getenv("THINPOST_OBJECT_KEYRING_FILE"), Secure: true}
	if config.Endpoint == "" && config.Bucket == "" && config.Region == "" && config.AccessKey == "" && config.SecretKey == "" && config.KeyringFile == "" {
		return nil, nil
	}
	if config.Endpoint == "" || config.Bucket == "" || !validS3Region(config.Region) || config.AccessKey == "" || config.SecretKey == "" || !filepath.IsAbs(config.KeyringFile) {
		return nil, errors.New("object storage configuration is incomplete")
	}
	endpoint, err := url.Parse("https://" + config.Endpoint)
	if err != nil || endpoint.Host != config.Endpoint || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("invalid object storage endpoint")
	}
	if getenv("THINPOST_ALLOW_INSECURE_LOCAL_S3") == "1" {
		address, err := netip.ParseAddr(endpoint.Hostname())
		if err != nil || !address.IsLoopback() {
			return nil, errors.New("insecure object storage is local-only")
		}
		db, err := url.Parse(databaseURL)
		if err != nil {
			return nil, errors.New("invalid database configuration")
		}
		dbAddress, err := netip.ParseAddr(db.Hostname())
		if err != nil || !dbAddress.IsLoopback() {
			return nil, errors.New("insecure object storage requires a local database")
		}
		config.Secure = false
	}
	return config, nil
}

// ObjectStorageConfig validates storage configuration for isolated maintenance
// commands without requiring operator-console configuration.
func ObjectStorageConfig(getenv func(string) string, databaseURL string) (*S3Config, error) {
	return objectStorageConfig(getenv, databaseURL)
}

func validS3Region(region string) bool {
	if len(region) == 0 || len(region) > 64 {
		return false
	}
	for _, c := range region {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// DatabaseURL validates database transport independently for migration jobs.
func DatabaseURL(getenv func(string) string) (string, error) {
	raw, err := configuredDatabaseURL(getenv)
	if err != nil {
		return "", err
	}
	db, err := url.Parse(raw)
	if err != nil || (db.Scheme != "postgres" && db.Scheme != "postgresql") || db.Host == "" || db.Hostname() == "" || strings.Contains(db.Hostname(), ",") || db.Fragment != "" || len(db.EscapedPath()) < 2 {
		return "", errors.New("invalid database configuration")
	}
	// The driver's URI parser looks for the first raw @ before the path,
	// whereas net/url uses the last. Require an explicit database path and
	// reject ambiguous authorities so both parsers see the same host.
	withoutScheme := strings.TrimPrefix(raw, db.Scheme+"://")
	authority, _, found := strings.Cut(withoutScheme, "/")
	if !found || strings.Count(authority, "@") > 1 {
		return "", errors.New("invalid database configuration")
	}
	query, err := url.ParseQuery(db.RawQuery)
	if err != nil {
		return "", errors.New("invalid database configuration")
	}
	// pgx applies URI query parameters after the authority and supports an
	// ssl=true alias that can override sslmode. Restrict the connection string
	// to settings that cannot redirect the connection or weaken TLS.
	allowed := map[string]bool{"sslmode": true, "sslrootcert": true, "default_query_exec_mode": true, "connect_timeout": true, "application_name": true, "search_path": true}
	for key, values := range query {
		if !allowed[key] || len(values) != 1 {
			return "", errors.New("unsupported or ambiguous database parameter")
		}
	}
	// simple_protocol inlines bind values (subjects, recipients, search terms)
	// into SQL text, which PostgreSQL can log.
	if mode, ok := query["default_query_exec_mode"]; ok {
		switch mode[0] {
		case "cache_statement", "cache_describe", "describe_exec", "exec":
		default:
			return "", errors.New("unsupported database query execution mode")
		}
	}
	if schema, ok := query["search_path"]; ok && !validDatabaseSchema(schema[0]) {
		return "", errors.New("invalid database schema")
	}
	if query.Get("sslmode") != "verify-full" {
		address, parseErr := netip.ParseAddr(db.Hostname())
		if getenv("THINPOST_ALLOW_INSECURE_LOCAL_DB") != "1" || parseErr != nil || !address.IsLoopback() {
			return "", errors.New("verified database TLS is required")
		}
	}
	return raw, nil
}

func validDatabaseSchema(value string) bool {
	if len(value) == 0 || len(value) > 63 {
		return false
	}
	for i, c := range value {
		if !(c == '_' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// adminHostname returns the hostname of a bare https origin in the form
// requests carry it: lowercase ASCII without trailing dots, or a canonical
// bracketed IPv6 address. It refuses names that could never match a request
// (non-ASCII, empty after normalization), so a typo cannot silently leave the
// console on every host or lock operators out.
func adminHostname(origin string) (string, bool) {
	if !validConnectOrigin(origin) {
		return "", false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return "", false
	}
	host := u.Hostname()
	if strings.Contains(host, ":") {
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", false
		}
		return "[" + addr.String() + "]", true
	}
	host = normalizeHostname(host)
	if host == "" {
		return "", false
	}
	for i := 0; i < len(host); i++ {
		if ch := host[i]; !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '.') {
			return "", false
		}
	}
	return host, true
}

// validConnectOrigin accepts only a bare https scheme://host[:port] origin.
func validConnectOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if strings.ContainsAny(origin, "*;' \t\r\n") || strings.HasSuffix(origin, "/") || strings.ContainsAny(u.Host, "?#") {
		return false
	}
	return origin == "https://"+u.Host
}
