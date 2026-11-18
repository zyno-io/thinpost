package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// GrantTrackingRetirementRole grants a pre-existing login only the privileges
// needed to retire mappings. Schema ownership and checksums are verified, and
// unsafe existing or inherited grants abort the transaction rather than being
// silently revoked. Database CONNECT must be provisioned separately.
func GrantTrackingRetirementRole(ctx context.Context, db *sql.DB, role string) error {
	if db == nil || !validIdentifier(role) || strings.EqualFold(role, "public") {
		return errors.New("invalid tracking retirement role")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if len(migrations) != runtimeGrantSchemaVersion {
		return errors.New("tracking retirement privileges need schema review")
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return errors.New("cannot lock schema for retirement grants")
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
		return errors.New("tracking retirement role or schema owner is invalid")
	}
	if err = checkTrackingRetirementAuditBoundary(ctx, tx); err != nil {
		return err
	}
	roleSQL := pgx.Identifier{role}.Sanitize()
	if _, err = tx.ExecContext(ctx, "GRANT USAGE ON SCHEMA "+pgx.Identifier{schema}.Sanitize()+" TO "+roleSQL); err != nil {
		return errors.New("tracking retirement schema grant failed")
	}
	grants := []struct{ table, privileges string }{
		{"thinpost_schema_migrations", "SELECT"},
		{"runtime_gate", "SELECT (singleton,generation,mode), UPDATE (lock_guard)"},
		{"tracking_objects", "SELECT (delivery_id,object_key,expires_at), UPDATE (object_key,retired_at)"},
		{"deliveries", "SELECT (id,state), UPDATE (tracking_retirement_lock_guard)"},
	}
	for _, grant := range grants {
		table := pgx.Identifier{schema, grant.table}.Sanitize()
		if _, err = tx.ExecContext(ctx, "GRANT "+grant.privileges+" ON TABLE "+table+" TO "+roleSQL); err != nil {
			return errors.New("tracking retirement table grant failed")
		}
	}
	if err = checkTrackingRetirementRolePrivileges(ctx, tx, role, true); err != nil {
		return err
	}
	return tx.Commit()
}

// CheckTrackingRetirementRolePrivileges audits inherited and SET-reachable
// privileges in the current database. External grants and access to other
// databases need separate controls. Call it before running maintenance and
// periodically in any future scheduler.
func CheckTrackingRetirementRolePrivileges(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("tracking retirement database unavailable")
	}
	return checkTrackingRetirementRolePrivileges(ctx, db, "", false)
}

func checkTrackingRetirementRolePrivileges(ctx context.Context, db rowQueryer, role string, maintenance bool) error {
	if err := checkTrackingRetirementAuditBoundary(ctx, db); err != nil {
		return err
	}
	var required, identity, unsafe bool
	err := db.QueryRowContext(ctx, `SELECT
		r.rolcanlogin AND has_database_privilege(r.oid,current_database(),'CONNECT') AND has_schema_privilege(r.oid,current_schema(),'USAGE')
			AND has_table_privilege(r.oid,'thinpost_schema_migrations','SELECT')
			AND has_column_privilege(r.oid,'runtime_gate','singleton','SELECT')
			AND has_column_privilege(r.oid,'runtime_gate','generation','SELECT')
			AND has_column_privilege(r.oid,'runtime_gate','mode','SELECT')
			AND has_column_privilege(r.oid,'runtime_gate','lock_guard','UPDATE')
			AND has_column_privilege(r.oid,'tracking_objects','delivery_id','SELECT')
			AND has_column_privilege(r.oid,'tracking_objects','object_key','SELECT')
			AND has_column_privilege(r.oid,'tracking_objects','object_key','UPDATE')
			AND has_column_privilege(r.oid,'tracking_objects','expires_at','SELECT')
			AND has_column_privilege(r.oid,'tracking_objects','retired_at','UPDATE')
			AND has_column_privilege(r.oid,'deliveries','id','SELECT')
			AND has_column_privilege(r.oid,'deliveries','state','SELECT')
			AND has_column_privilege(r.oid,'deliveries','tracking_retirement_lock_guard','UPDATE'),
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
				OR (n.nspname IN ('pg_catalog','information_schema') AND has_function_privilege(candidate.oid,p.oid,'EXECUTE') AND NOT has_function_privilege('public',p.oid,'EXECUTE'))
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
						OR (c.oid<>'thinpost_schema_migrations'::regclass AND has_table_privilege(candidate.oid,c.oid,'SELECT'))
						OR EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped AND (
							(has_column_privilege(candidate.oid,c.oid,a.attname,'UPDATE') AND NOT (
								(c.oid='runtime_gate'::regclass AND a.attname='lock_guard')
								OR (c.oid='tracking_objects'::regclass AND a.attname IN ('object_key','retired_at'))
								OR (c.oid='deliveries'::regclass AND a.attname='tracking_retirement_lock_guard')))
							OR (has_column_privilege(candidate.oid,c.oid,a.attname,'SELECT') AND NOT (
								c.oid='thinpost_schema_migrations'::regclass
								OR (c.oid='runtime_gate'::regclass AND a.attname IN ('singleton','generation','mode'))
								OR (c.oid='tracking_objects'::regclass AND a.attname IN ('delivery_id','object_key','expires_at'))
								OR (c.oid='deliveries'::regclass AND a.attname IN ('id','state'))))))))))))
		FROM pg_roles r WHERE r.rolname=COALESCE(NULLIF($1,''),current_user)`, role, maintenance).Scan(&required, &identity, &unsafe)
	if err != nil || !required || !identity || unsafe {
		return errors.New("tracking retirement database role has unsafe or missing privileges")
	}
	return nil
}

// The login can retire rows only if the exact reviewed trigger is active and
// its definer cannot be invoked by other roles. Migration checksums alone do
// not prove that a live function or trigger was not changed after migration.
func checkTrackingRetirementAuditBoundary(ctx context.Context, db rowQueryer) error {
	if len(migrations) != runtimeGrantSchemaVersion || migrations[20].file != "migrations/021_retirement_audit_privileges.sql" {
		return errors.New("tracking retirement audit boundary needs schema review")
	}
	body, _, err := migrationBody(migrations[20])
	if err != nil {
		return errors.New("tracking retirement audit boundary unavailable")
	}
	parts := strings.SplitN(string(body), "AS $$", 2)
	if len(parts) != 2 {
		return errors.New("tracking retirement audit boundary unavailable")
	}
	functionBody := strings.SplitN(parts[1], "$$;", 2)
	if len(functionBody) != 2 {
		return errors.New("tracking retirement audit boundary unavailable")
	}
	var valid bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		JOIN pg_language l ON l.oid=p.prolang
		WHERE p.oid=to_regprocedure('prevent_tracking_mapping_revival()') AND n.nspname=current_schema()
		AND p.prosrc=$1 AND p.prosecdef AND p.proowner=n.nspowner AND l.lanname='plpgsql'
		AND p.prorettype='pg_catalog.trigger'::regtype AND p.proconfig=ARRAY['search_path=pg_catalog']
		AND NOT EXISTS (SELECT 1 FROM aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) acl
			WHERE acl.privilege_type='EXECUTE' AND acl.grantee<>p.proowner)
		AND EXISTS (SELECT 1 FROM pg_trigger t WHERE t.tgrelid=to_regclass('tracking_objects')
			AND t.tgname='tracking_object_retirement_monotonic' AND NOT t.tgisinternal
			AND t.tgenabled='O' AND t.tgfoid=p.oid AND t.tgtype=19 AND t.tgqual IS NULL
			AND (SELECT array_agg(a.attname::text ORDER BY a.attname::text) FROM unnest(t.tgattr) attr(attnum)
				JOIN pg_attribute a ON a.attrelid=t.tgrelid AND a.attnum=attr.attnum)=ARRAY['object_key','retired_at']::text[])
	)`, functionBody[0]).Scan(&valid)
	if err != nil || !valid {
		return errors.New("tracking retirement audit boundary is unsafe or missing")
	}
	return nil
}
