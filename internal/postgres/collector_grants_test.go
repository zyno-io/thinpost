package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The caller has already revoked PUBLIC TEMP through restrictedRuntimeForTest.
func restrictedCollectorForTest(t *testing.T, ctx context.Context, owner *sql.DB, dsn string) (*sql.DB, func()) {
	t.Helper()
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	role := "Thinpost_gc_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{role}.Sanitize()
	if _, err := owner.ExecContext(ctx, "CREATE ROLE "+quoted+" LOGIN PASSWORD 'disposable-collector-password'"); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ExecContext(ctx, "GRANT SELECT ON messages TO "+quoted); err != nil {
		t.Fatal(err)
	}
	if err := GrantCollectorRole(ctx, owner, role); err == nil {
		t.Fatal("collector inherited broad message read permission")
	}
	var leaked bool
	if err := owner.QueryRowContext(ctx, `SELECT has_table_privilege(oid,'runtime_gate','SELECT') FROM pg_roles WHERE rolname=$1`, role).Scan(&leaked); err != nil || leaked {
		t.Fatal("failed collector grant left privileges behind", err)
	}
	if _, err := owner.ExecContext(ctx, "REVOKE SELECT ON messages FROM "+quoted); err != nil {
		t.Fatal(err)
	}
	if err := GrantCollectorRole(ctx, owner, role); err != nil {
		t.Fatal("collector grants failed", err)
	}
	if err := GrantCollectorRole(ctx, owner, role); err != nil {
		t.Fatal("repeated collector grants failed", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "disposable-collector-password")
	collector, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err != nil {
		t.Fatal("collector role rejected", err)
	}
	for _, query := range []string{
		`SELECT object_key FROM messages LIMIT 1`,
		`SELECT final_object_key FROM deliveries LIMIT 1`,
		`SELECT object_key FROM bounce_receipts LIMIT 1`,
		`SELECT object_key FROM tracking_objects LIMIT 1`,
		`SELECT object_key,content_expires_at FROM inbound_messages LIMIT 1`,
		`SELECT mode FROM runtime_gate WHERE singleton FOR SHARE`,
		`SELECT version,checksum FROM thinpost_schema_migrations LIMIT 1`,
	} {
		rows, err := collector.QueryContext(ctx, query)
		if err != nil {
			t.Fatal("collector lacks a required permission", err)
		}
		rows.Close()
	}
	for _, query := range []string{
		`SELECT * FROM recovery_holds`,
		`SELECT * FROM recovery_events`,
		`SELECT app_id FROM messages LIMIT 1`,
		`SELECT message_id FROM deliveries LIMIT 1`,
		`SELECT delivery_id FROM bounce_receipts LIMIT 1`,
		`SELECT delivery_id FROM tracking_objects LIMIT 1`,
		`SELECT app_id FROM inbound_messages LIMIT 1`,
		`SELECT address FROM inbound_recipients LIMIT 1`,
		`SELECT domain FROM inbound_domains LIMIT 1`,
		`SELECT * FROM apps LIMIT 1`,
		`SELECT message_id FROM message_recipients LIMIT 1`,
		`SELECT subject FROM messages LIMIT 1`,
		`SELECT sender_domain FROM messages LIMIT 1`,
		`UPDATE runtime_gate SET mode='closed'`,
		`UPDATE runtime_gate SET object_gc_horizon=clock_timestamp()`,
		`INSERT INTO audit_events(id,actor_id,app_id,action,resource_id) VALUES ('x','x','x','x','x')`,
		`CREATE TEMP TABLE private_shadow(id int)`,
	} {
		if _, err := collector.ExecContext(ctx, query); err == nil {
			t.Fatal("collector unexpectedly obtained a prohibited permission", query)
		}
	}
	if _, err = owner.ExecContext(ctx, "GRANT SELECT ON messages TO "+quoted); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
		t.Fatal("collector checker accepted broad message reads")
	}
	if err = GrantCollectorRole(ctx, owner, role); err == nil {
		t.Fatal("grant accepted unsafe existing permissions")
	}
	if _, err = owner.ExecContext(ctx, "REVOKE SELECT ON messages FROM "+quoted); err != nil {
		t.Fatal(err)
	}
	if err = GrantCollectorRole(ctx, owner, role); err != nil {
		t.Fatal("collector grants did not recover after unsafe grant revoked", err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err != nil {
		t.Fatal("collector role did not recover after unsafe grant revoked", err)
	}
	if _, err = owner.ExecContext(ctx, "GRANT UPDATE (mode) ON runtime_gate TO "+quoted); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
		t.Fatal("collector checker accepted gate mutation")
	}
	if _, err = owner.ExecContext(ctx, "REVOKE UPDATE (mode) ON runtime_gate FROM "+quoted); err != nil {
		t.Fatal(err)
	}
	if err = CheckSchema(ctx, collector); err != nil {
		t.Fatal("collector could not verify schema checksums", err)
	}
	if _, err = owner.ExecContext(ctx, "GRANT SELECT (object_key) ON messages TO "+quoted+" WITH GRANT OPTION"); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
		t.Fatal("collector checker accepted column grant option")
	}
	if _, err = owner.ExecContext(ctx, "REVOKE GRANT OPTION FOR SELECT (object_key) ON messages FROM "+quoted); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "GRANT pg_read_server_files TO "+quoted); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
		t.Fatal("collector checker accepted privileged built-in role")
	}
	if _, err = owner.ExecContext(ctx, "REVOKE pg_read_server_files FROM "+quoted); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "GRANT EXECUTE ON FUNCTION pg_catalog.pg_read_file(text) TO "+quoted); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
		t.Fatal("collector checker accepted privileged catalog function")
	}
	if _, err = owner.ExecContext(ctx, "REVOKE EXECUTE ON FUNCTION pg_catalog.pg_read_file(text) FROM "+quoted); err != nil {
		t.Fatal(err)
	}
	var publicFileRead bool
	if err = owner.QueryRowContext(ctx, `SELECT has_function_privilege('public','pg_catalog.pg_read_file(text)','EXECUTE')`).Scan(&publicFileRead); err != nil || publicFileRead {
		t.Fatal("unexpected public file-read privilege in disposable database", err)
	}
	if _, err = owner.ExecContext(ctx, `GRANT EXECUTE ON FUNCTION pg_catalog.pg_read_file(text) TO PUBLIC`); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
		t.Fatal("collector checker accepted public server-file access")
	}
	if _, err = owner.ExecContext(ctx, `REVOKE EXECUTE ON FUNCTION pg_catalog.pg_read_file(text) FROM PUBLIC`); err != nil {
		t.Fatal(err)
	}
	extra := pgx.Identifier{"Thinpost_gc_extra_" + hex.EncodeToString(random[:])}.Sanitize()
	if _, err = owner.ExecContext(ctx, "CREATE ROLE "+extra+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "GRANT "+extra+" TO "+quoted+" WITH ADMIN OPTION"); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
		t.Fatal("collector checker accepted role administration privilege")
	}
	if _, err = owner.ExecContext(ctx, "REVOKE "+extra+" FROM "+quoted); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "GRANT SELECT (app_id) ON messages TO "+extra); err != nil {
		t.Fatal(err)
	}
	for _, options := range []string{"INHERIT FALSE, SET TRUE", "INHERIT TRUE, SET FALSE"} {
		if _, err = owner.ExecContext(ctx, "GRANT "+extra+" TO "+quoted+" WITH "+options); err != nil {
			t.Fatal(err)
		}
		if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
			t.Fatal("collector checker accepted inherited or SET-reachable message access", options)
		}
		if _, err = owner.ExecContext(ctx, "REVOKE "+extra+" FROM "+quoted); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = owner.ExecContext(ctx, "DROP OWNED BY "+extra); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "DROP ROLE "+extra); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err = owner.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	schemaSQL := pgx.Identifier{schema}.Sanitize()
	if _, err = owner.ExecContext(ctx, "GRANT USAGE ON SCHEMA "+schemaSQL+" TO "+quoted+" WITH GRANT OPTION"); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
		t.Fatal("collector checker accepted schema grant option")
	}
	if _, err = owner.ExecContext(ctx, "REVOKE GRANT OPTION FOR USAGE ON SCHEMA "+schemaSQL+" FROM "+quoted); err != nil {
		t.Fatal(err)
	}
	function := pgx.Identifier{schema, "collector_test_" + hex.EncodeToString(random[:])}.Sanitize()
	if _, err = owner.ExecContext(ctx, "CREATE FUNCTION "+function+"() RETURNS integer LANGUAGE sql AS 'SELECT 1'"); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
		t.Fatal("collector checker accepted an executable non-system function")
	}
	if _, err = owner.ExecContext(ctx, "REVOKE ALL ON FUNCTION "+function+"() FROM PUBLIC"); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err != nil {
		t.Fatal("collector checker rejected inaccessible function", err)
	}
	if _, err = owner.ExecContext(ctx, "DROP FUNCTION "+function+"()"); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "CREATE FUNCTION "+function+"() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS 'BEGIN RETURN NEW; END'"); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
		t.Fatal("collector checker accepted a security-definer trigger function")
	}
	if _, err = owner.ExecContext(ctx, "DROP FUNCTION "+function+"()"); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "CREATE FUNCTION "+function+"() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'"); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "GRANT CREATE ON SCHEMA "+schemaSQL+" TO "+quoted); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "ALTER FUNCTION "+function+"() OWNER TO "+quoted); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "REVOKE CREATE ON SCHEMA "+schemaSQL+" FROM "+quoted); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
		t.Fatal("collector checker accepted ownership of a user function")
	}
	if _, err = owner.ExecContext(ctx, "DROP FUNCTION "+function+"()"); err != nil {
		t.Fatal(err)
	}
	otherSchema := pgx.Identifier{"collector_foreign_" + hex.EncodeToString(random[:])}.Sanitize()
	if _, err = owner.ExecContext(ctx, "CREATE SCHEMA "+otherSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "CREATE TABLE "+otherSchema+".private_data (value text)"); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "GRANT USAGE ON SCHEMA "+otherSchema+" TO "+quoted); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "GRANT SELECT ON "+otherSchema+".private_data TO "+quoted); err != nil {
		t.Fatal(err)
	}
	if err = CheckCollectorRolePrivileges(ctx, collector); err == nil {
		t.Fatal("collector checker accepted access to another schema")
	}
	if _, err = owner.ExecContext(ctx, "DROP SCHEMA "+otherSchema+" CASCADE"); err != nil {
		t.Fatal(err)
	}
	conn, err := owner.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, "SET ROLE "+quoted); err != nil {
		t.Fatal(err)
	}
	if err = checkCollectorRolePrivileges(ctx, conn, "", false); err == nil {
		t.Fatal("collector checker accepted a session hidden behind SET ROLE")
	}
	if _, err = conn.ExecContext(ctx, "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	return collector, func() {
		collector.Close()
		if _, err := owner.ExecContext(ctx, "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := owner.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	}
}
