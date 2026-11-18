package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// GrantCollectorRole grants the collection reader only the gate lock and
// object-key columns. Horizon advancement requires a separate maintenance
// identity. Existing privileges are checked, not revoked.
func GrantCollectorRole(ctx context.Context, db *sql.DB, role string) error {
	if db == nil || !validIdentifier(role) || strings.EqualFold(role, "public") {
		return errors.New("invalid collector role")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if len(migrations) != runtimeGrantSchemaVersion {
		return errors.New("collector privileges need review for the current schema")
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return errors.New("cannot lock schema for collector grants")
	}
	count, err := appliedCount(ctx, tx)
	if err != nil || count != runtimeGrantSchemaVersion {
		return errors.New("database schema is not at the reviewed version")
	}
	var schema string
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT current_schema(),
		current_user=session_user AND pg_has_role(current_user,n.nspowner,'MEMBER') AND EXISTS (
			SELECT 1 FROM pg_roles r WHERE r.rolname=$1 AND r.rolcanlogin AND r.rolname<>current_user
			AND NOT pg_has_role(r.oid,(SELECT oid FROM pg_roles WHERE rolname=current_user),'SET'))
		FROM pg_namespace n WHERE n.nspname=current_schema()`, role).Scan(&schema, &valid)
	if err != nil || schema == "" || !valid {
		return errors.New("collector role or schema owner is invalid")
	}
	roleSQL := pgx.Identifier{role}.Sanitize()
	schemaSQL := pgx.Identifier{schema}.Sanitize()
	if _, err = tx.ExecContext(ctx, "GRANT USAGE ON SCHEMA "+schemaSQL+" TO "+roleSQL); err != nil {
		return errors.New("collector schema grant failed")
	}
	grants := []struct{ table, privileges string }{
		{"thinpost_schema_migrations", "SELECT"},
		{"runtime_gate", "SELECT, UPDATE (lock_guard)"},
		{"messages", "SELECT (object_key)"},
		{"deliveries", "SELECT (final_object_key)"},
		{"bounce_receipts", "SELECT (object_key)"},
		{"tracking_objects", "SELECT (object_key)"},
		{"inbound_messages", "SELECT (object_key, content_expires_at)"},
	}
	for _, grant := range grants {
		table := pgx.Identifier{schema, grant.table}.Sanitize()
		if _, err = tx.ExecContext(ctx, "GRANT "+grant.privileges+" ON TABLE "+table+" TO "+roleSQL); err != nil {
			return errors.New("collector table grant failed")
		}
	}
	if err = checkCollectorRolePrivileges(ctx, tx, role, true); err != nil {
		return err
	}
	return tx.Commit()
}

// CheckCollectorRolePrivileges must be called by a future collector at startup
// and periodically. It audits inherited and SET-reachable privileges in this
// database. Cross-database access and external identity grants require separate
// deployment controls.
func CheckCollectorRolePrivileges(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("collector database unavailable")
	}
	return checkCollectorRolePrivileges(ctx, db, "", false)
}

func checkCollectorRolePrivileges(ctx context.Context, db rowQueryer, role string, maintenance bool) error {
	var required, identity, unsafe bool
	err := db.QueryRowContext(ctx, `SELECT
		r.rolcanlogin AND has_database_privilege(r.oid,current_database(),'CONNECT') AND has_schema_privilege(r.oid,current_schema(),'USAGE')
			AND has_table_privilege(r.oid,'thinpost_schema_migrations','SELECT')
			AND has_table_privilege(r.oid,'runtime_gate','SELECT')
			AND has_column_privilege(r.oid,'runtime_gate','lock_guard','UPDATE')
			AND has_column_privilege(r.oid,'messages','object_key','SELECT')
			AND has_column_privilege(r.oid,'deliveries','final_object_key','SELECT')
			AND has_column_privilege(r.oid,'bounce_receipts','object_key','SELECT')
			AND has_column_privilege(r.oid,'tracking_objects','object_key','SELECT')
			AND has_column_privilege(r.oid,'inbound_messages','object_key','SELECT')
			AND has_column_privilege(r.oid,'inbound_messages','content_expires_at','SELECT'),
		($2 OR current_user=session_user),
		EXISTS (SELECT 1 FROM pg_roles candidate WHERE pg_has_role(r.oid,candidate.oid,'SET') AND (
			candidate.rolsuper OR candidate.rolcreatedb OR candidate.rolcreaterole OR candidate.rolreplication OR candidate.rolbypassrls OR left(candidate.rolname,3)='pg_'
			OR has_database_privilege(candidate.oid,current_database(),'TEMPORARY,CREATE')
			OR has_database_privilege(candidate.oid,current_database(),'CONNECT WITH GRANT OPTION')
			OR EXISTS (SELECT 1 FROM pg_roles target WHERE target.oid<>candidate.oid AND pg_has_role(candidate.oid,target.oid,'MEMBER WITH ADMIN OPTION'))
			OR EXISTS (SELECT 1 FROM pg_namespace n WHERE has_schema_privilege(candidate.oid,n.oid,'CREATE'))
			OR EXISTS (SELECT 1 FROM pg_namespace n WHERE has_schema_privilege(candidate.oid,n.oid,'USAGE WITH GRANT OPTION'))
			OR EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE (
				has_function_privilege(candidate.oid,p.oid,'EXECUTE WITH GRANT OPTION')
				OR
				(n.nspname IN ('pg_catalog','information_schema') AND has_function_privilege(candidate.oid,p.oid,'EXECUTE') AND NOT has_function_privilege('public',p.oid,'EXECUTE'))
				OR (n.nspname='pg_catalog' AND p.proname IN ('pg_read_file','pg_read_binary_file','pg_ls_dir','pg_stat_file','lo_import','lo_export') AND has_function_privilege(candidate.oid,p.oid,'EXECUTE'))
				OR (n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp_%'
					AND (pg_has_role(candidate.oid,p.proowner,'MEMBER') OR ((p.prosecdef OR p.prorettype<>'pg_catalog.trigger'::regtype) AND has_function_privilege(candidate.oid,p.oid,'EXECUTE'))))))
			OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
				WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'
				AND n.nspname NOT LIKE 'pg_temp_%' AND c.relkind IN ('r','p','v','m','f','S') AND (
					pg_has_role(candidate.oid,c.relowner,'MEMBER')
					OR (c.relkind='S' AND has_sequence_privilege(candidate.oid,c.oid,'USAGE,SELECT,UPDATE'))
					OR (c.relkind<>'S' AND (
						has_table_privilege(candidate.oid,c.oid,'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER,MAINTAIN')
						OR has_any_column_privilege(candidate.oid,c.oid,'INSERT,REFERENCES')
						OR has_any_column_privilege(candidate.oid,c.oid,'SELECT WITH GRANT OPTION,UPDATE WITH GRANT OPTION')
						OR (c.oid NOT IN ('runtime_gate'::regclass,'thinpost_schema_migrations'::regclass) AND has_table_privilege(candidate.oid,c.oid,'SELECT'))
						OR EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped AND (
							(has_column_privilege(candidate.oid,c.oid,a.attname,'UPDATE') AND NOT (c.oid='runtime_gate'::regclass AND a.attname='lock_guard'))
							OR (has_column_privilege(candidate.oid,c.oid,a.attname,'SELECT') AND NOT (
								c.oid IN ('runtime_gate'::regclass,'thinpost_schema_migrations'::regclass)
								OR (c.oid='messages'::regclass AND a.attname='object_key')
								OR (c.oid='deliveries'::regclass AND a.attname='final_object_key')
								OR (c.oid='bounce_receipts'::regclass AND a.attname='object_key')
								OR (c.oid='tracking_objects'::regclass AND a.attname='object_key')
								OR (c.oid='inbound_messages'::regclass AND a.attname IN ('object_key','content_expires_at'))))))))))))
		FROM pg_roles r WHERE r.rolname=COALESCE(NULLIF($1,''),current_user)`, role, maintenance).Scan(&required, &identity, &unsafe)
	if err != nil || !required || !identity || unsafe {
		return errors.New("collector database role has unsafe or missing privileges")
	}
	return nil
}
