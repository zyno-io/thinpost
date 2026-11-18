package postgres

import (
	"context"
	"github.com/zyno-io/thinpost/internal/admin"
)

// SigningDomainActive rechecks current app/domain authorization when preparing
// each message, rather than relying only on the original acceptance decision.
func (r *Repository) SigningDomainActive(ctx context.Context, appID, domain string) (bool, error) {
	normalized, ok := admin.NormalizeDomain(domain)
	if !opaqueID(appID) || !ok || normalized != domain {
		return false, nil
	}
	var active bool
	err := r.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sender_domains d JOIN apps a ON a.id=d.app_id WHERE d.app_id=$1 AND d.domain=$2 AND d.active AND d.verified_at IS NOT NULL AND a.active)`, appID, domain).Scan(&active)
	return active, err
}
