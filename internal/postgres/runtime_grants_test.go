package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Exercise the full repository integration suite through the same grants used
// for deployment, while keeping its fixture and inspection connection as the
// maintenance role. The enclosing test requires a disposable database.
func restrictedRuntimeForTest(t *testing.T, ctx context.Context, owner *sql.DB, dsn string) (*sql.DB, func()) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if u.Hostname() == "" {
		q, queryErr := url.ParseQuery(u.RawQuery)
		if queryErr != nil || os.Getenv("THINPOST_TEST_DISPOSABLE_DB") != "1" || u.Path != "/thinpost_fixture" || q.Get("host") != "/controller/run" || q.Get("sslmode") != "disable" || len(q["host"]) != 1 || len(q["sslmode"]) != 1 {
			t.Fatal("Unix-socket runtime fixture is not explicitly isolated")
		}
		for key := range q {
			if key != "host" && key != "sslmode" && key != "search_path" {
				t.Fatal("unexpected Unix fixture option")
			}
		}
		u.Host = "127.0.0.1:5432"
		q.Del("host")
		u.RawQuery = q.Encode()
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	role := "Thinpost_test_" + hex.EncodeToString(random[:])
	quotedRole := pgx.Identifier{role}.Sanitize()
	if _, err := owner.ExecContext(ctx, "CREATE ROLE "+quotedRole+" LOGIN PASSWORD 'disposable-runtime-password'"); err != nil {
		t.Fatal(err)
	}
	var database string
	var publicTemp bool
	if err := owner.QueryRowContext(ctx, `SELECT datname, EXISTS (SELECT 1 FROM aclexplode(COALESCE(datacl,acldefault('d',datdba))) a WHERE a.grantee=0 AND a.privilege_type='TEMPORARY') FROM pg_database WHERE datname=current_database()`).Scan(&database, &publicTemp); err != nil {
		t.Fatal(err)
	}
	quotedDatabase := pgx.Identifier{database}.Sanitize()
	if publicTemp {
		if err := GrantRuntimeRole(ctx, owner, role); err == nil {
			t.Fatal("accepted runtime role inheriting PUBLIC temporary privilege")
		}
		var leaked bool
		if err := owner.QueryRowContext(ctx, `SELECT has_table_privilege(oid, 'runtime_gate', 'SELECT') FROM pg_roles WHERE rolname=$1`, role).Scan(&leaked); err != nil || leaked {
			t.Fatal("failed grant transaction leaked privileges", err)
		}
		if _, err := owner.ExecContext(ctx, "REVOKE TEMPORARY ON DATABASE "+quotedDatabase+" FROM PUBLIC"); err != nil {
			t.Fatal(err)
		}
	}
	if err := GrantRuntimeRole(ctx, owner, role); err != nil {
		t.Fatal(err)
	}
	if err := GrantRuntimeRole(ctx, owner, role); err != nil {
		t.Fatal("repeated grant failed", err)
	}
	for _, table := range []string{"recovery_holds", "recovery_events"} {
		if _, err := owner.ExecContext(ctx, "GRANT SELECT (recovery_id) ON "+table+" TO "+quotedRole); err != nil {
			t.Fatal(err)
		}
		if err := GrantRuntimeRole(ctx, owner, role); err == nil {
			t.Fatalf("accepted pre-existing recovery evidence read on %s", table)
		}
		if _, err := owner.ExecContext(ctx, "REVOKE SELECT (recovery_id) ON "+table+" FROM "+quotedRole); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.ExecContext(ctx, "GRANT SELECT (actor) ON delivery_retry_policy_events TO "+quotedRole); err != nil {
		t.Fatal(err)
	}
	if err := GrantRuntimeRole(ctx, owner, role); err == nil {
		t.Fatal("accepted retry policy audit read")
	}
	if _, err := owner.ExecContext(ctx, "REVOKE SELECT (actor) ON delivery_retry_policy_events FROM "+quotedRole); err != nil {
		t.Fatal(err)
	}
	var checksum string
	if err := owner.QueryRowContext(ctx, `SELECT checksum FROM thinpost_schema_migrations WHERE version=1`).Scan(&checksum); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ExecContext(ctx, `UPDATE thinpost_schema_migrations SET checksum='changed' WHERE version=1`); err != nil {
		t.Fatal(err)
	}
	if err := GrantRuntimeRole(ctx, owner, role); err == nil {
		t.Fatal("accepted schema with modified migration")
	}
	if _, err := owner.ExecContext(ctx, `UPDATE thinpost_schema_migrations SET checksum=$1 WHERE version=1`, checksum); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ExecContext(ctx, "GRANT UPDATE ON runtime_gate TO "+quotedRole); err != nil {
		t.Fatal(err)
	}
	if err := GrantRuntimeRole(ctx, owner, role); err == nil {
		t.Fatal("accepted unsafe existing gate grant")
	}
	if _, err := owner.ExecContext(ctx, "REVOKE UPDATE ON runtime_gate FROM "+quotedRole); err != nil {
		t.Fatal(err)
	}
	if err := GrantRuntimeRole(ctx, owner, role); err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "disposable-runtime-password")
	runtimeDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckRuntimeGatePrivileges(ctx, runtimeDB); err != nil {
		t.Fatal("runtime login was not accepted", err)
	}
	if _, err = owner.ExecContext(ctx, "GRANT SELECT (recovery_id) ON recovery_holds TO "+quotedRole); err != nil {
		t.Fatal(err)
	}
	if err = CheckRuntimeGatePrivileges(ctx, runtimeDB); err == nil {
		t.Fatal("runtime startup accepted recovery evidence read access")
	}
	if _, err = owner.ExecContext(ctx, "REVOKE SELECT (recovery_id) ON recovery_holds FROM "+quotedRole); err != nil {
		t.Fatal(err)
	}
	if err = CheckRuntimeGatePrivileges(ctx, runtimeDB); err != nil {
		t.Fatal("runtime did not recover after evidence read was revoked", err)
	}
	if _, err = runtimeDB.ExecContext(ctx, `SELECT count(*) FROM message_recipients`); err != nil {
		t.Fatal("runtime cannot read recorded recipients", err)
	}
	for _, statement := range []string{
		`UPDATE message_recipients SET address='x'`,
		`DELETE FROM message_recipients`,
		`SELECT * FROM recovery_holds`,
		`SELECT * FROM recovery_events`,
		`SELECT * FROM delivery_retry_policy_events`,
		`UPDATE runtime_gate SET mode='open'`,
		`UPDATE runtime_gate SET revision=1`,
		`INSERT INTO delivery_retry_policy(max_attempts,initial_ms,max_delay_ms,max_age_ms) VALUES (1,60000,60000,3600000)`,
		`DELETE FROM delivery_retry_policy`,
		`INSERT INTO delivery_retry_policy_events(revision,actor,old_policy,new_policy) VALUES (1,'x','{}','{}')`,
		`UPDATE runtime_gate SET object_gc_horizon=clock_timestamp()`,
		`INSERT INTO recovery_events(id,recovery_id,generation,action,actor,revision) VALUES ('x','x','x','x','x',1)`,
		`UPDATE thinpost_schema_migrations SET checksum='changed'`,
		`CREATE TEMP TABLE runtime_gate(id int)`,
	} {
		if _, err = runtimeDB.ExecContext(ctx, statement); err == nil {
			t.Fatalf("runtime unexpectedly permitted %q", statement)
		}
	}
	if _, err = runtimeDB.ExecContext(ctx, `UPDATE delivery_retry_policy SET max_attempts=max_attempts WHERE singleton`); err != nil {
		t.Fatal("runtime cannot update the retry policy", err)
	}
	if _, err = runtimeDB.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id) VALUES ('retry-audit-probe','x',NULL,'grant.probe','delivery_retry_policy')`); err != nil {
		t.Fatal("runtime cannot write a system-scoped audit row", err)
	}
	return runtimeDB, func() {
		runtimeDB.Close()
		if _, err := owner.ExecContext(ctx, "DROP OWNED BY "+quotedRole); err != nil {
			t.Error(err)
		}
		if _, err := owner.ExecContext(ctx, "DROP ROLE "+quotedRole); err != nil {
			t.Error(err)
		}
		if publicTemp {
			if _, err := owner.ExecContext(ctx, "GRANT TEMPORARY ON DATABASE "+quotedDatabase+" TO PUBLIC"); err != nil {
				t.Error(err)
			}
		}
	}
}
