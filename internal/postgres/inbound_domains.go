package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zyno-io/thinpost/internal/admin"
)

const inboundDomainColumns = `domain,state,created_at,verified_at,mx_status,mx_checked_at`

type inboundDomainScanner interface{ Scan(...any) error }

func scanInboundDomain(row inboundDomainScanner) (admin.InboundDomain, error) {
	var d admin.InboundDomain
	var verified, checked sql.NullTime
	var mx sql.NullString
	if err := row.Scan(&d.Name, &d.State, &d.CreatedAt, &verified, &mx, &checked); err != nil {
		return admin.InboundDomain{}, err
	}
	if verified.Valid {
		d.VerifiedAt = &verified.Time
	}
	if mx.Valid {
		d.MXStatus = &mx.String
	}
	if checked.Valid {
		d.MXCheckedAt = &checked.Time
	}
	return d, nil
}

// isUniqueViolation reports a unique-index conflict, such as a second app
// verifying a domain another app already owns.
func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func validInboundDomainName(domain string) bool {
	normalized, ok := admin.NormalizeDomain(domain)
	return ok && normalized == domain
}

func (r *Repository) ListInboundDomains(ctx context.Context, appID string) ([]admin.InboundDomain, error) {
	if !opaqueID(appID) {
		return nil, admin.ErrInvalidInput
	}
	var exists bool
	err := r.DB.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, admin.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT `+inboundDomainColumns+` FROM inbound_domains WHERE app_id=$1 ORDER BY domain`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []admin.InboundDomain
	for rows.Next() {
		d, err := scanInboundDomain(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// RequestInboundDomain records a pending claim and returns its TXT challenge.
// Asking again for a pending domain replaces the challenge. Several apps may
// hold a pending claim on one domain; only the first to verify becomes its
// owner.
func (r *Repository) RequestInboundDomain(ctx context.Context, actor, appID, domain string) (admin.InboundDomainChallenge, error) {
	var result admin.InboundDomainChallenge
	if !validInboundDomainName(domain) || !opaqueID(appID) || !adminText(actor, 256) {
		return result, admin.ErrInvalidInput
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return result, err
	}
	value := "thinpost-inbound-verification=" + base64.RawURLEncoding.EncodeToString(random[:])
	digest := sha256.Sum256([]byte(value))
	id, err := newOpaqueID()
	if err != nil {
		return result, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 AND active FOR SHARE`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return result, admin.ErrNotFound
	}
	if err != nil {
		return result, err
	}
	if err = requireNoBounceDomain(ctx, tx, domain); err != nil {
		return admin.InboundDomainChallenge{}, err
	}
	row := tx.QueryRowContext(ctx, `INSERT INTO inbound_domains(id,app_id,domain,verification_hash) VALUES ($1,$2,$3,$4)
		ON CONFLICT (app_id,domain) DO UPDATE SET verification_hash=EXCLUDED.verification_hash,updated_at=clock_timestamp() WHERE inbound_domains.state='pending'
		RETURNING `+inboundDomainColumns, id, appID, domain, digest[:])
	result.Domain, err = scanInboundDomain(row)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.InboundDomainChallenge{}, admin.ErrConflict
	}
	if err != nil {
		return admin.InboundDomainChallenge{}, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "inbound_domain.requested", domain); err != nil {
		return admin.InboundDomainChallenge{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.InboundDomainChallenge{}, err
	}
	result.DNSName = "_thinpost." + domain
	result.DNSValue = value
	return result, nil
}

// requireNoBounceDomain refuses a receiving domain that any app uses as its
// bounce domain. It takes the domain-name lock, which adding a bounce domain
// takes too, so the two checks cannot both pass for one name.
func requireNoBounceDomain(ctx context.Context, tx *sql.Tx, domain string) error {
	if err := lockDomainName(ctx, tx, domain); err != nil {
		return err
	}
	var taken bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM bounce_domains b JOIN apps a ON a.id=b.app_id WHERE b.domain=$1 AND a.deleted_at IS NULL)`, domain).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return admin.ErrBounceDomainClash
	}
	return nil
}

// InboundDomainChallenge returns the domain and, for a pending claim, the
// digest of its TXT value.
func (r *Repository) InboundDomainChallenge(ctx context.Context, appID, domain string) (admin.InboundDomain, [32]byte, error) {
	var digest [32]byte
	if !validInboundDomainName(domain) || !opaqueID(appID) {
		return admin.InboundDomain{}, digest, admin.ErrInvalidInput
	}
	var raw []byte
	row := r.DB.QueryRowContext(ctx, `SELECT `+inboundDomainColumns+`,verification_hash FROM inbound_domains WHERE app_id=$1 AND domain=$2`, appID, domain)
	var d admin.InboundDomain
	var verified, checked sql.NullTime
	var mx sql.NullString
	err := row.Scan(&d.Name, &d.State, &d.CreatedAt, &verified, &mx, &checked, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.InboundDomain{}, digest, admin.ErrNotFound
	}
	if err != nil {
		return admin.InboundDomain{}, digest, err
	}
	if verified.Valid {
		d.VerifiedAt = &verified.Time
	}
	if mx.Valid {
		d.MXStatus = &mx.String
	}
	if checked.Valid {
		d.MXCheckedAt = &checked.Time
	}
	if d.State == admin.InboundDomainPending {
		if len(raw) != 32 {
			return admin.InboundDomain{}, digest, admin.ErrConflict
		}
		copy(digest[:], raw)
	}
	return d, digest, nil
}

// VerifyInboundDomain activates a pending claim whose challenge matches. When
// another app already owns the domain the unique index rejects the update and
// the call returns ErrConflict: the first verification wins.
func (r *Repository) VerifyInboundDomain(ctx context.Context, actor, appID, domain string, digest [32]byte) (admin.InboundDomain, error) {
	if !validInboundDomainName(domain) || !opaqueID(appID) || !adminText(actor, 256) {
		return admin.InboundDomain{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.InboundDomain{}, err
	}
	defer tx.Rollback()
	if err = requireNoBounceDomain(ctx, tx, domain); err != nil {
		return admin.InboundDomain{}, err
	}
	row := tx.QueryRowContext(ctx, `UPDATE inbound_domains SET state='active',verified_at=clock_timestamp(),verification_hash=NULL,updated_at=clock_timestamp()
		WHERE app_id=$1 AND domain=$2 AND state='pending' AND verification_hash=$3 RETURNING `+inboundDomainColumns, appID, domain, digest[:])
	result, err := scanInboundDomain(row)
	if errors.Is(err, sql.ErrNoRows) || isUniqueViolation(err) {
		return admin.InboundDomain{}, admin.ErrConflict
	}
	if err != nil {
		return admin.InboundDomain{}, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "inbound_domain.verified", domain); err != nil {
		return admin.InboundDomain{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.InboundDomain{}, err
	}
	return result, nil
}

// RecordInboundMX stores the result of an MX check with the database clock.
func (r *Repository) RecordInboundMX(ctx context.Context, actor, appID, domain, status string) (admin.InboundDomain, error) {
	switch status {
	case admin.MXThinpost, admin.MXMixed, admin.MXOther, admin.MXNone, admin.MXNull, admin.MXError:
	default:
		return admin.InboundDomain{}, admin.ErrInvalidInput
	}
	if !validInboundDomainName(domain) || !opaqueID(appID) || !adminText(actor, 256) {
		return admin.InboundDomain{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.InboundDomain{}, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `UPDATE inbound_domains SET mx_status=$3,mx_checked_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE app_id=$1 AND domain=$2 RETURNING `+inboundDomainColumns, appID, domain, status)
	result, err := scanInboundDomain(row)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.InboundDomain{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.InboundDomain{}, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "inbound_domain.mx_checked", domain); err != nil {
		return admin.InboundDomain{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.InboundDomain{}, err
	}
	return result, nil
}

// SetInboundDomainEnabled moves a verified domain between active and disabled.
// It is idempotent: a domain already in the target state is returned unchanged
// and nothing is audited. A pending claim cannot be enabled or disabled.
func (r *Repository) SetInboundDomainEnabled(ctx context.Context, actor, appID, domain string, enabled bool) (admin.InboundDomain, error) {
	if !validInboundDomainName(domain) || !opaqueID(appID) || !adminText(actor, 256) {
		return admin.InboundDomain{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.InboundDomain{}, err
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM inbound_domains WHERE app_id=$1 AND domain=$2 FOR UPDATE`, appID, domain).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.InboundDomain{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.InboundDomain{}, err
	}
	if state == admin.InboundDomainPending {
		return admin.InboundDomain{}, admin.ErrConflict
	}
	target, action := admin.InboundDomainDisabled, "inbound_domain.disabled"
	if enabled {
		target, action = admin.InboundDomainActive, "inbound_domain.enabled"
	}
	if state == target {
		result, err := scanInboundDomain(tx.QueryRowContext(ctx, `SELECT `+inboundDomainColumns+` FROM inbound_domains WHERE app_id=$1 AND domain=$2`, appID, domain))
		if err != nil {
			return admin.InboundDomain{}, err
		}
		return result, tx.Commit()
	}
	result, err := scanInboundDomain(tx.QueryRowContext(ctx, `UPDATE inbound_domains SET state=$3,updated_at=clock_timestamp() WHERE app_id=$1 AND domain=$2 RETURNING `+inboundDomainColumns, appID, domain, target))
	if err != nil {
		return admin.InboundDomain{}, err
	}
	if err = recordAudit(ctx, tx, actor, appID, action, domain); err != nil {
		return admin.InboundDomain{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.InboundDomain{}, err
	}
	return result, nil
}

// DeleteInboundDomain removes a claim or an owned domain. Stored inbound
// messages keep their own copy of the domain name and are not touched.
func (r *Repository) DeleteInboundDomain(ctx context.Context, actor, appID, domain string) error {
	if !validInboundDomainName(domain) || !opaqueID(appID) || !adminText(actor, 256) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `DELETE FROM inbound_domains WHERE app_id=$1 AND domain=$2 RETURNING state`, appID, domain).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err = recordAudit(ctx, tx, actor, appID, "inbound_domain.deleted", domain); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) GetInboundLimits(ctx context.Context, appID string) (admin.InboundLimits, error) {
	var limits admin.InboundLimits
	if !opaqueID(appID) {
		return limits, admin.ErrInvalidInput
	}
	err := r.DB.QueryRowContext(ctx, `SELECT inbound_daily_limit,inbound_pending_limit FROM apps WHERE id=$1`, appID).Scan(&limits.DailyLimit, &limits.PendingLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return limits, admin.ErrNotFound
	}
	return limits, err
}

func (r *Repository) SetInboundLimits(ctx context.Context, actor, appID string, limits admin.InboundLimits) (admin.InboundLimits, error) {
	if !adminText(actor, 256) || !opaqueID(appID) ||
		limits.DailyLimit < admin.MinInboundLimit || limits.DailyLimit > admin.MaxInboundDailyLimit ||
		limits.PendingLimit < admin.MinInboundLimit || limits.PendingLimit > admin.MaxInboundPendingLimit {
		return admin.InboundLimits{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.InboundLimits{}, err
	}
	defer tx.Rollback()
	var old admin.InboundLimits
	err = tx.QueryRowContext(ctx, `SELECT inbound_daily_limit,inbound_pending_limit FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&old.DailyLimit, &old.PendingLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.InboundLimits{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.InboundLimits{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE apps SET inbound_daily_limit=$2,inbound_pending_limit=$3 WHERE id=$1`, appID, limits.DailyLimit, limits.PendingLimit); err != nil {
		return admin.InboundLimits{}, err
	}
	auditID, err := newOpaqueID()
	if err != nil {
		return admin.InboundLimits{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
		VALUES ($1,$2,$3,'app.inbound_limits.updated',$3,jsonb_build_object('previous_daily_limit',$4::integer,'previous_pending_limit',$5::integer,'daily_limit',$6::integer,'pending_limit',$7::integer))`,
		auditID, actor, appID, old.DailyLimit, old.PendingLimit, limits.DailyLimit, limits.PendingLimit); err != nil {
		return admin.InboundLimits{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.InboundLimits{}, err
	}
	return limits, nil
}

var _ admin.InboundStore = (*Repository)(nil)
