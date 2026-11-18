package controlsession

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zyno-io/thinpost/internal/postgres"
)

func validatePipelineFixtureURL(dsn string, marked bool) (*url.URL, error) {
	databaseURL, err := url.Parse(dsn)
	if err != nil || databaseURL == nil || (databaseURL.Scheme != "postgres" && databaseURL.Scheme != "postgresql") || databaseURL.User == nil || databaseURL.User.Username() != "postgres" || databaseURL.Fragment != "" {
		return nil, errors.New("invalid pipeline fixture URL")
	}
	if !marked || databaseURL.Path != "/thinpost_fixture" {
		return nil, errors.New("pipeline requires the explicitly marked thinpost_fixture database")
	}
	query, err := url.ParseQuery(databaseURL.RawQuery)
	if err != nil {
		return nil, errors.New("invalid pipeline fixture URL options")
	}
	for key, values := range query {
		if (key != "host" && key != "sslmode") || len(values) != 1 {
			return nil, errors.New("invalid pipeline fixture URL options")
		}
	}
	if query.Get("sslmode") != "disable" {
		return nil, errors.New("pipeline fixture requires local-only transport")
	}
	host := databaseURL.Hostname()
	unixHost := query.Get("host")
	if host == "" {
		if !strings.HasPrefix(unixHost, "/") {
			return nil, errors.New("pipeline fixture requires a local Unix socket")
		}
	} else {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.IsLoopback() || unixHost != "" {
			return nil, errors.New("pipeline fixture requires a numeric loopback database")
		}
	}
	return databaseURL, nil
}

func TestPipelineFixtureDatabaseGuard(t *testing.T) {
	for _, test := range []struct {
		name, dsn     string
		marked, valid bool
	}{
		{"loopback", "postgres://postgres:fixture@127.0.0.1:5432/thinpost_fixture?sslmode=disable", true, true},
		{"unix socket", "postgres://postgres@/thinpost_fixture?host=%2Fcontroller%2Frun&sslmode=disable", true, true},
		{"unmarked", "postgres://postgres@127.0.0.1/thinpost_fixture?sslmode=disable", false, false},
		{"different database", "postgres://postgres@127.0.0.1/postgres?sslmode=disable", true, false},
		{"remote", "postgres://postgres@198.51.100.1/thinpost_fixture?sslmode=disable", true, false},
		{"hostname", "postgres://postgres@localhost/thinpost_fixture?sslmode=disable", true, false},
		{"host override", "postgres://postgres@127.0.0.1/thinpost_fixture?sslmode=disable&host=remote", true, false},
		{"hostaddr override", "postgres://postgres@127.0.0.1/thinpost_fixture?sslmode=disable&hostaddr=198.51.100.1", true, false},
		{"service override", "postgres://postgres@127.0.0.1/thinpost_fixture?sslmode=disable&service=other", true, false},
		{"malformed options", "postgres://postgres@127.0.0.1/thinpost_fixture?sslmode=disable&hostaddr=127.0.0.1;service=other", true, false},
		{"ambiguous TLS", "postgres://postgres@127.0.0.1/thinpost_fixture?sslmode=disable&sslmode=require", true, false},
		{"wrong role", "postgres://app@127.0.0.1/thinpost_fixture?sslmode=disable", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := validatePipelineFixtureURL(test.dsn, test.marked)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
		})
	}
}

// restrictedPipelineRuntime changes privileges only on an explicitly marked,
// disposable local database. It restores PUBLIC TEMP and drops its role.
func restrictedPipelineRuntime(t *testing.T, ctx context.Context, owner *sql.DB, databaseURL *url.URL) (*sql.DB, func()) {
	t.Helper()
	host := databaseURL.Hostname()
	var database string
	var publicTemp bool
	if err := owner.QueryRowContext(ctx, `SELECT datname, EXISTS (SELECT 1 FROM aclexplode(COALESCE(datacl,acldefault('d',datdba))) a WHERE a.grantee=0 AND a.privilege_type='TEMPORARY') FROM pg_database WHERE datname=current_database()`).Scan(&database, &publicTemp); err != nil || database != "thinpost_fixture" {
		t.Fatal("unexpected pipeline fixture database", err)
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	role := "thinpost_pipeline_" + hex.EncodeToString(random[:8])
	password := hex.EncodeToString(random[8:])
	roleSQL := pgx.Identifier{role}.Sanitize()
	databaseSQL := pgx.Identifier{database}.Sanitize()
	var runtime *sql.DB
	roleCreated, revoked := false, false
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if runtime != nil {
			_ = runtime.Close()
		}
		if roleCreated {
			if _, err := owner.ExecContext(cleanupCtx, "DROP OWNED BY "+roleSQL); err != nil {
				t.Error("fixture runtime grant cleanup failed", err)
			}
			if _, err := owner.ExecContext(cleanupCtx, "DROP ROLE "+roleSQL); err != nil {
				t.Error("fixture runtime role cleanup failed", err)
			}
		}
		if revoked {
			if _, err := owner.ExecContext(cleanupCtx, "GRANT TEMPORARY ON DATABASE "+databaseSQL+" TO PUBLIC"); err != nil {
				t.Error("fixture TEMP grant restore failed", err)
			}
		}
	}
	ok := false
	defer func() {
		if !ok {
			cleanup()
		}
	}()
	if _, err := owner.ExecContext(ctx, "CREATE ROLE "+roleSQL+" LOGIN PASSWORD '"+password+"'"); err != nil {
		t.Fatal("cannot create fixture runtime role", err)
	}
	roleCreated = true
	if publicTemp {
		if _, err := owner.ExecContext(ctx, "REVOKE TEMPORARY ON DATABASE "+databaseSQL+" FROM PUBLIC"); err != nil {
			t.Fatal("cannot restrict fixture database", err)
		}
		revoked = true
	}
	if err := postgres.GrantRuntimeRole(ctx, owner, role); err != nil {
		t.Fatal("cannot grant fixture runtime role", err)
	}
	u := *databaseURL
	u.User = url.UserPassword(role, password)
	if host == "" {
		// The isolated PostgreSQL fixture's peer-authenticated Unix socket accepts
		// only the postgres OS user; the restricted role uses pod-local TCP.
		u.Host = "127.0.0.1:5432"
		q := u.Query()
		q.Del("host")
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
	}
	var err error
	runtime, err = sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal("cannot open fixture runtime", err)
	}
	if err = postgres.CheckRuntimeGatePrivileges(ctx, runtime); err != nil {
		t.Fatal("fixture runtime privileges rejected", err)
	}
	ok = true
	return runtime, cleanup
}
