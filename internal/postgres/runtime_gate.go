package postgres

import (
	"context"
	"database/sql"
	"errors"
)

var ErrRuntimeClosed = errors.New("runtime admission is closed")

// CheckAdmission verifies that this replica can transact under the configured
// deployment generation. It does not require an active SMTP worker lease.
func (r *Repository) CheckAdmission(ctx context.Context) error {
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CheckRuntimeGatePrivileges rejects credentials that can alter the gate or
// recovery evidence. PostgreSQL requires UPDATE on a column to take FOR
// SHARE; only the constrained, inert lock_guard column may be writable.
// Deployments must grant that column and keep the maintenance role separate.
func CheckRuntimeGatePrivileges(ctx context.Context, db *sql.DB) error {
	return checkRuntimeRolePrivileges(ctx, db, "", false)
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// checkRuntimeRolePrivileges can also validate a role before the maintenance
// transaction commits its grants. It evaluates inherited and SET privileges.
func checkRuntimeRolePrivileges(ctx context.Context, db rowQueryer, role string, maintenance bool) error {
	var canRead, canLock, loginRole, unsafe bool
	err := db.QueryRowContext(ctx, `SELECT
		has_table_privilege(runtime.oid,'runtime_gate','SELECT'),
		has_column_privilege(runtime.oid,'runtime_gate','lock_guard','UPDATE'),
		($2 OR current_user=session_user),
		EXISTS (SELECT 1 FROM pg_roles candidate WHERE pg_has_role(runtime.oid,candidate.oid,'SET') AND (
			has_database_privilege(candidate.oid,current_database(),'TEMPORARY,CREATE')
			OR EXISTS (SELECT 1 FROM pg_namespace WHERE has_schema_privilege(candidate.oid,oid,'CREATE'))
			OR has_table_privilege(candidate.oid,'runtime_gate','INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
			OR has_any_column_privilege(candidate.oid,'runtime_gate','INSERT')
			OR EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid='runtime_gate'::regclass AND attnum>0 AND NOT attisdropped AND attname<>'lock_guard' AND has_column_privilege(candidate.oid,'runtime_gate',attname,'UPDATE'))
			OR EXISTS (SELECT 1 FROM pg_class WHERE oid IN ('runtime_gate'::regclass,'recovery_holds'::regclass,'recovery_events'::regclass) AND pg_has_role(candidate.oid,relowner,'MEMBER'))
			OR has_table_privilege(candidate.oid,'recovery_holds','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
			OR has_any_column_privilege(candidate.oid,'recovery_holds','SELECT,INSERT,UPDATE')
			OR has_table_privilege(candidate.oid,'recovery_events','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
			OR has_any_column_privilege(candidate.oid,'recovery_events','SELECT,INSERT,UPDATE')
			OR EXISTS (SELECT 1 FROM pg_class c WHERE c.oid=to_regclass('delivery_retry_policy_events') AND (
				pg_has_role(candidate.oid,c.relowner,'MEMBER') OR has_table_privilege(candidate.oid,c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
				OR has_any_column_privilege(candidate.oid,c.oid,'SELECT,INSERT,UPDATE')))
			OR EXISTS (SELECT 1 FROM pg_class c WHERE c.oid=to_regclass('delivery_attempts') AND (
				pg_has_role(candidate.oid,c.relowner,'MEMBER') OR has_table_privilege(candidate.oid,c.oid,'TRUNCATE,REFERENCES,TRIGGER')))
			OR has_parameter_privilege(candidate.oid,'session_replication_role','SET')
			OR EXISTS (SELECT 1 FROM pg_class c WHERE c.oid IN (to_regclass('tracking_objects'),to_regclass('tracking_links'),to_regclass('tracking_events')) AND (
				pg_has_role(candidate.oid,c.relowner,'MEMBER') OR has_table_privilege(candidate.oid,c.oid,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
				OR EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
					AND has_column_privilege(candidate.oid,c.oid,a.attname,'UPDATE')
					AND NOT ((c.oid=to_regclass('tracking_objects') AND a.attname='lock_guard') OR (c.oid=to_regclass('tracking_links') AND a.attname='event_count')))))))
		FROM pg_roles runtime WHERE runtime.rolname=COALESCE(NULLIF($1,''),current_user)`, role, maintenance).Scan(&canRead, &canLock, &loginRole, &unsafe)
	if err != nil {
		return err
	}
	if !canRead || !canLock || !loginRole || unsafe {
		return errors.New("runtime database role has unsafe recovery privileges")
	}
	return nil
}

// beginRuntime holds the admission lock until commit or rollback. Gate
// closure takes an exclusive lock, so it waits for earlier mutations and
// prevents later ones from committing. Always acquire it before other locks.
func (r *Repository) beginRuntime(ctx context.Context) (*sql.Tx, error) {
	if r.DB == nil || !opaqueID(r.Generation) {
		return nil, ErrRuntimeClosed
	}
	// Delivery finalization takes a fresh statement snapshot after locking the
	// parent message. Do not inherit a server-level repeatable-read default.
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	var generation sql.NullString
	var mode string
	err = tx.QueryRowContext(ctx, `SELECT generation,mode FROM runtime_gate WHERE singleton=true FOR SHARE`).Scan(&generation, &mode)
	if err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRuntimeClosed
		}
		return nil, err
	}
	if !generation.Valid || generation.String != r.Generation || mode != "open" {
		_ = tx.Rollback()
		return nil, ErrRuntimeClosed
	}
	return tx, nil
}
