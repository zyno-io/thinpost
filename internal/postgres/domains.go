package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
)

func (r *Repository) ListDomains(ctx context.Context, appID string) ([]admin.Domain, error) {
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
	rows, err := r.DB.QueryContext(ctx, `SELECT domain,active,created_at,verified_at FROM sender_domains WHERE app_id=$1 ORDER BY domain`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []admin.Domain
	for rows.Next() {
		var d admin.Domain
		if err = rows.Scan(&d.Name, &d.Active, &d.CreatedAt, &d.VerifiedAt); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
func (r *Repository) RequestDomain(ctx context.Context, actor, appID, domain string) (admin.DomainChallenge, error) {
	var result admin.DomainChallenge
	normalized, ok := admin.NormalizeDomain(domain)
	if !ok || normalized != domain || !opaqueID(appID) || !adminText(actor, 256) {
		return result, admin.ErrInvalidInput
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return result, err
	}
	value := "thinpost-verification=" + base64.RawURLEncoding.EncodeToString(random[:])
	digest := sha256.Sum256([]byte(value))
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
	err = tx.QueryRowContext(ctx, `INSERT INTO sender_domains(app_id,domain,active,verification_hash) VALUES ($1,$2,false,$3)
		ON CONFLICT (app_id,domain) DO UPDATE SET verification_hash=EXCLUDED.verification_hash,verified_at=NULL WHERE NOT sender_domains.active
		RETURNING domain,active,created_at,verified_at`, appID, domain, digest[:]).Scan(&result.Domain.Name, &result.Domain.Active, &result.Domain.CreatedAt, &result.Domain.VerifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.DomainChallenge{}, admin.ErrConflict
	}
	if err != nil {
		return admin.DomainChallenge{}, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "domain.challenge.created", domain); err != nil {
		return admin.DomainChallenge{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.DomainChallenge{}, err
	}
	result.DNSName = "_thinpost." + domain
	result.DNSValue = value
	return result, nil
}
func (r *Repository) DomainChallenge(ctx context.Context, appID, domain string) (admin.Domain, [32]byte, error) {
	var d admin.Domain
	var digest [32]byte
	normalized, ok := admin.NormalizeDomain(domain)
	if !ok || normalized != domain || !opaqueID(appID) {
		return d, digest, admin.ErrInvalidInput
	}
	var raw []byte
	err := r.DB.QueryRowContext(ctx, `SELECT domain,active,created_at,verified_at,verification_hash FROM sender_domains WHERE app_id=$1 AND domain=$2`, appID, domain).Scan(&d.Name, &d.Active, &d.CreatedAt, &d.VerifiedAt, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return d, digest, admin.ErrNotFound
	}
	if err != nil {
		return d, digest, err
	}
	if !d.Active && len(raw) != 32 {
		return d, digest, admin.ErrConflict
	}
	copy(digest[:], raw)
	return d, digest, nil
}
func (r *Repository) VerifyDomain(ctx context.Context, actor, appID, domain string, digest [32]byte) (admin.Domain, error) {
	var result admin.Domain
	normalized, ok := admin.NormalizeDomain(domain)
	if !ok || normalized != domain || !opaqueID(appID) || !adminText(actor, 256) {
		return result, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `UPDATE sender_domains SET active=true,verified_at=clock_timestamp() WHERE app_id=$1 AND domain=$2 AND NOT active AND verification_hash=$3 RETURNING domain,active,created_at,verified_at`, appID, domain, digest[:]).Scan(&result.Name, &result.Active, &result.CreatedAt, &result.VerifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return result, admin.ErrConflict
	}
	if err != nil {
		return result, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "domain.verified", domain); err != nil {
		return admin.Domain{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.Domain{}, err
	}
	return result, nil
}
