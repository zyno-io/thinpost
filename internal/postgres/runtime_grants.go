package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// runtimeGrantSchemaVersion forces a privilege review when a migration is added.
const runtimeGrantSchemaVersion = 41

// GrantRuntimeRole grants only the reviewed runtime operations. It is an
// explicit maintenance operation, not an application startup migration. The
// database and role must already exist; PUBLIC must not have database TEMP.
// Existing unsafe grants or memberships are rejected, never silently revoked.
func GrantRuntimeRole(ctx context.Context, db *sql.DB, role string) error {
	if role == "" || len(role) > 63 || role == "PUBLIC" {
		return errors.New("invalid runtime role")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = grantRuntimeRoleTx(ctx, tx, role); err != nil {
		return err
	}
	return tx.Commit()
}

// grantRuntimeRoleTx applies the reviewed grants and the privilege check inside
// the caller's transaction, so a deploy can commit them with its migrations.
func grantRuntimeRoleTx(ctx context.Context, tx *sql.Tx, role string) error {
	if role == "" || len(role) > 63 || role == "PUBLIC" {
		return errors.New("invalid runtime role")
	}
	var err error
	if len(migrations) != runtimeGrantSchemaVersion {
		return errors.New("runtime privileges need review for the current schema")
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return errors.New("cannot lock schema for runtime grants")
	}
	count, err := appliedCount(ctx, tx)
	if err != nil || count != runtimeGrantSchemaVersion {
		return errors.New("database schema is not at the reviewed version")
	}
	var schema string
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT current_schema(), EXISTS (
		SELECT 1 FROM pg_roles WHERE rolname=$1 AND rolcanlogin AND NOT rolsuper AND rolname<>current_user
	)`, role).Scan(&schema, &valid)
	if err != nil || schema == "" || !valid {
		return errors.New("runtime role or schema is invalid")
	}
	roleSQL := pgx.Identifier{role}.Sanitize()
	schemaSQL := pgx.Identifier{schema}.Sanitize()
	if _, err = tx.ExecContext(ctx, "GRANT USAGE ON SCHEMA "+schemaSQL+" TO "+roleSQL); err != nil {
		return fmt.Errorf("grant schema usage: %w", err)
	}
	grants := []struct{ table, privileges string }{
		{"thinpost_schema_migrations", "SELECT"},
		{"apps", "SELECT, INSERT, UPDATE"},
		{"api_keys", "SELECT, INSERT, UPDATE"},
		{"audit_events", "INSERT"},
		{"sender_domains", "SELECT, INSERT, UPDATE"},
		{"messages", "SELECT, INSERT, UPDATE"},
		{"message_recipients", "SELECT, INSERT"},
		{"deliveries", "SELECT, INSERT, UPDATE"},
		{"delivery_attempts", "SELECT, INSERT, UPDATE"},
		{"webhook_subscriptions", "SELECT, INSERT, UPDATE"},
		{"webhook_events", "SELECT, INSERT"},
		{"webhook_deliveries", "SELECT, INSERT, UPDATE"},
		{"webhook_attempts", "SELECT, INSERT, UPDATE"},
		{"tracking_objects", "SELECT, INSERT, UPDATE (lock_guard)"},
		{"tracking_links", "SELECT, INSERT, UPDATE (event_count)"},
		{"tracking_events", "SELECT, INSERT"},
		{"idempotency_keys", "SELECT, INSERT, UPDATE, DELETE"},
		{"controller_leases", "SELECT, INSERT, UPDATE"},
		{"bounce_receipts", "SELECT, INSERT"},
		{"suppressions", "SELECT, INSERT, DELETE, UPDATE (reason, origin, source_message_id)"},
		{"recipient_feedback", "SELECT, INSERT"},
		{"feedback_mailbox", "SELECT, INSERT, UPDATE"},
		{"feedback_reporters", "SELECT, INSERT, DELETE"},
		{"inbound_feedback_daily", "SELECT, INSERT, UPDATE"},
		{"inbound_domains", "SELECT, INSERT, UPDATE, DELETE"},
		{"inbound_messages", "SELECT, INSERT"},
		{"inbound_recipients", "SELECT, INSERT"},
		{"inbound_daily", "SELECT, INSERT, UPDATE"},
		{"delivery_retry_policy", "SELECT, UPDATE"},
		{"sending_ips", "SELECT, INSERT, UPDATE, DELETE"},
		{"edge_source_reports", "SELECT, INSERT, UPDATE"},
		{"runtime_gate", "SELECT, UPDATE (lock_guard)"},
	}
	for _, grant := range grants {
		table := pgx.Identifier{schema, grant.table}.Sanitize()
		if _, err = tx.ExecContext(ctx, "GRANT "+grant.privileges+" ON TABLE "+table+" TO "+roleSQL); err != nil {
			return fmt.Errorf("grant runtime privileges for %s: %w", grant.table, err)
		}
	}
	return checkRuntimeRolePrivileges(ctx, tx, role, true)
}
