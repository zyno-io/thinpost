package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/dkimkey"
)

// ActiveSigningKey returns the key that signs mail from an app's domain. It
// rechecks current authorization each time (not just the original acceptance):
// the app and the sender domain must be active and verified. Anything else,
// including a verified domain that has no key yet, is dkimkey.ErrNoKey. The
// sealed private key is not read here; SealedSigningKey fetches it on a cache
// miss.
func (r *Repository) ActiveSigningKey(ctx context.Context, appID, domain string) (dkimkey.Stored, error) {
	normalized, ok := admin.NormalizeDomain(domain)
	if !opaqueID(appID) || !ok || normalized != domain {
		return dkimkey.Stored{}, dkimkey.ErrNoKey
	}
	k := dkimkey.Stored{AppID: appID, Domain: domain}
	err := r.DB.QueryRowContext(ctx, `SELECT k.id,k.selector,k.public_key FROM dkim_keys k
		JOIN sender_domains d ON d.app_id=k.app_id AND d.domain=k.domain JOIN apps a ON a.id=k.app_id
		WHERE k.app_id=$1 AND k.domain=$2 AND k.state='active' AND d.active AND d.verified_at IS NOT NULL AND a.active`, appID, domain).Scan(&k.ID, &k.Selector, &k.PublicKey)
	if errors.Is(err, sql.ErrNoRows) {
		return dkimkey.Stored{}, dkimkey.ErrNoKey
	}
	return k, err
}

// SealedSigningKey returns an active key with its sealed private key. A key
// that has been retired has none, so it can no longer sign.
func (r *Repository) SealedSigningKey(ctx context.Context, id string) (dkimkey.Stored, error) {
	var k dkimkey.Stored
	if !opaqueID(id) {
		return k, dkimkey.ErrNoKey
	}
	err := r.DB.QueryRowContext(ctx, `SELECT id,app_id,domain,selector,public_key,sealed_private_key FROM dkim_keys WHERE id=$1 AND state='active'`, id).
		Scan(&k.ID, &k.AppID, &k.Domain, &k.Selector, &k.PublicKey, &k.Sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return dkimkey.Stored{}, dkimkey.ErrNoKey
	}
	return k, err
}
