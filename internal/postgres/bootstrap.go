package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type DatabaseBootstrap struct {
	Database, Schema, RuntimeRole, PoolerAuthRole string
}

func validIdentifier(value string) bool {
	return value != "" && len(value) <= 63 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// BootstrapDatabase prepares one explicitly named, dedicated database. It
// changes only its PUBLIC connection and temporary-table privileges, grants
// connection to the specified roles, and creates a protected application
// schema. It does not migrate tables or open the recovery gate.
func BootstrapDatabase(ctx context.Context, db *sql.DB, input DatabaseBootstrap) error {
	if !validIdentifier(input.Database) || !validIdentifier(input.Schema) || !validIdentifier(input.RuntimeRole) ||
		input.Schema == "public" || strings.HasPrefix(input.Schema, "pg_") ||
		(input.PoolerAuthRole != "" && !validIdentifier(input.PoolerAuthRole)) ||
		strings.EqualFold(input.RuntimeRole, "public") || strings.EqualFold(input.PoolerAuthRole, "public") {
		return errors.New("invalid database bootstrap parameters")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return errors.New("cannot lock database bootstrap")
	}
	var name, current, owner string
	var authenticated bool
	err = tx.QueryRowContext(ctx, `SELECT current_database(), current_user, pg_get_userbyid(datdba), current_user=session_user
		FROM pg_database WHERE datname=current_database()`).Scan(&name, &current, &owner, &authenticated)
	if err != nil || name != input.Database || current != owner || !authenticated || current == input.RuntimeRole {
		return errors.New("bootstrap requires the authenticated owner of the expected database")
	}
	var runtimeValid, poolerValid bool
	err = tx.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM pg_roles WHERE rolname=$1 AND rolcanlogin AND NOT rolsuper AND NOT pg_has_role(oid,(SELECT oid FROM pg_roles WHERE rolname=current_user),'SET')),
		$2='' OR EXISTS (SELECT 1 FROM pg_roles WHERE rolname=$2 AND NOT rolsuper)`, input.RuntimeRole, input.PoolerAuthRole).Scan(&runtimeValid, &poolerValid)
	if err != nil || !runtimeValid || !poolerValid {
		return errors.New("bootstrap roles are invalid or missing")
	}
	var schemaOwner sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname=$1`, input.Schema).Scan(&schemaOwner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if schemaOwner.Valid && schemaOwner.String != current {
		return errors.New("existing application schema has a different owner")
	}
	var publicCreate bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace n,
		LATERAL aclexplode(COALESCE(n.nspacl,acldefault('n',n.nspowner))) a
		WHERE n.nspname=$1 AND a.grantee=0 AND a.privilege_type='CREATE')`, input.Schema).Scan(&publicCreate)
	if err != nil || publicCreate {
		return errors.New("application schema has unsafe PUBLIC access")
	}
	databaseSQL := pgx.Identifier{name}.Sanitize()
	schemaSQL := pgx.Identifier{input.Schema}.Sanitize()
	if _, err = tx.ExecContext(ctx, "REVOKE CONNECT, TEMPORARY ON DATABASE "+databaseSQL+" FROM PUBLIC"); err != nil {
		return err
	}
	roles := []string{input.RuntimeRole}
	if input.PoolerAuthRole != "" && input.PoolerAuthRole != input.RuntimeRole {
		roles = append(roles, input.PoolerAuthRole)
	}
	for _, role := range roles {
		if _, err = tx.ExecContext(ctx, "GRANT CONNECT ON DATABASE "+databaseSQL+" TO "+pgx.Identifier{role}.Sanitize()); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+schemaSQL+" AUTHORIZATION "+pgx.Identifier{current}.Sanitize()); err != nil {
		return err
	}
	return tx.Commit()
}
