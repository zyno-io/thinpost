package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/netip"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
)

const smtpPolicyColumns = `smtp_from_mode,smtp_from_domain,smtp_from_keep_origin,smtp_from_address,smtp_from_only_unauthorized`

func scanSMTPPolicy(row rowScanner) (admin.SMTPFromPolicy, error) {
	var p admin.SMTPFromPolicy
	var domain, address sql.NullString
	if err := row.Scan(&p.Mode, &domain, &p.KeepOrigin, &address, &p.OnlyUnauthorized); err != nil {
		return admin.SMTPFromPolicy{}, err
	}
	if domain.Valid {
		p.Domain = &domain.String
	}
	if address.Valid {
		p.Address = &address.String
	}
	return p, nil
}

func samePolicy(a, b admin.SMTPFromPolicy) bool {
	same := func(x, y *string) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	return a.Mode == b.Mode && a.KeepOrigin == b.KeepOrigin && a.OnlyUnauthorized == b.OnlyUnauthorized && same(a.Domain, b.Domain) && same(a.Address, b.Address)
}

// GetSMTPSettings reads the app's From policy without the gate.
func (r *Repository) GetSMTPSettings(ctx context.Context, appID string) (admin.SMTPFromPolicy, error) {
	if !opaqueID(appID) {
		return admin.SMTPFromPolicy{}, admin.ErrInvalidInput
	}
	policy, err := scanSMTPPolicy(r.DB.QueryRowContext(ctx, `SELECT `+smtpPolicyColumns+` FROM apps WHERE id=$1`, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return admin.SMTPFromPolicy{}, admin.ErrNotFound
	}
	return policy, err
}

// SetSMTPSettings stores a normalized policy under the open gate and audits a
// change as smtp_settings.updated. An identical policy is a no-op with no audit
// row. A target domain must be an active verified sender domain of the app,
// otherwise the result is ErrConflict.
func (r *Repository) SetSMTPSettings(ctx context.Context, actor, appID string, policy admin.SMTPFromPolicy) (admin.SMTPFromPolicy, error) {
	if !adminText(actor, 256) || !opaqueID(appID) {
		return admin.SMTPFromPolicy{}, admin.ErrInvalidInput
	}
	policy, ok := admin.NormalizeSMTPFromPolicy(policy)
	if !ok {
		return admin.SMTPFromPolicy{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.SMTPFromPolicy{}, err
	}
	defer tx.Rollback()
	previous, err := scanSMTPPolicy(tx.QueryRowContext(ctx, `SELECT `+smtpPolicyColumns+` FROM apps WHERE id=$1 FOR UPDATE`, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return admin.SMTPFromPolicy{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.SMTPFromPolicy{}, err
	}
	if samePolicy(previous, policy) {
		return policy, tx.Commit()
	}
	for _, target := range admin.SMTPPolicyDomains(policy) {
		var verified bool
		err = tx.QueryRowContext(ctx, `SELECT true FROM sender_domains WHERE app_id=$1 AND domain=$2 AND active FOR SHARE`, appID, target).Scan(&verified)
		if errors.Is(err, sql.ErrNoRows) {
			return admin.SMTPFromPolicy{}, admin.ErrConflict
		}
		if err != nil {
			return admin.SMTPFromPolicy{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE apps SET smtp_from_mode=$2,smtp_from_domain=$3,smtp_from_keep_origin=$4,smtp_from_address=$5,smtp_from_only_unauthorized=$6 WHERE id=$1`,
		appID, policy.Mode, policy.Domain, policy.KeepOrigin, policy.Address, policy.OnlyUnauthorized); err != nil {
		return admin.SMTPFromPolicy{}, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "smtp_settings.updated", appID); err != nil {
		return admin.SMTPFromPolicy{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.SMTPFromPolicy{}, err
	}
	return policy, nil
}

// SMTPConnection resolves the SMTP host from operator-set sending-IP rows: the
// app's assigned row, else the row whose address is the configured primary
// source. Nothing the Edge reports chooses the host; its report supplies only
// the observed submission ports. An unset primary resolves no host and does not
// set PrimaryUnnamed.
func (r *Repository) SMTPConnection(ctx context.Context, appID string, primary netip.Addr) (admin.SMTPConnection, error) {
	out := admin.SMTPConnection{Ports: admin.SMTPPorts(), ObservedPorts: []int{}}
	if !opaqueID(appID) {
		return out, admin.ErrInvalidInput
	}
	var assigned bool
	var helo sql.NullString
	err := r.DB.QueryRowContext(ctx, `SELECT a.sending_ip_id IS NOT NULL,s.helo_name FROM apps a LEFT JOIN sending_ips s ON s.id=a.sending_ip_id WHERE a.id=$1`, appID).Scan(&assigned, &helo)
	if errors.Is(err, sql.ErrNoRows) {
		return out, admin.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	switch {
	case assigned && helo.Valid:
		source := admin.SMTPHostAppSendingIP
		out.Host, out.HostSource = &helo.String, &source
	case !assigned && primary.IsValid():
		err = r.DB.QueryRowContext(ctx, `SELECT helo_name FROM sending_ips WHERE address=$1::inet`, primary.Unmap().String()).Scan(&helo)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		if err == nil {
			source := admin.SMTPHostPrimary
			out.Host, out.HostSource = &helo.String, &source
		} else {
			out.PrimaryUnnamed = true
		}
	}
	var reported time.Time
	var ports []byte
	err = r.DB.QueryRowContext(ctx, `SELECT reported_at,to_json(submission_ports) FROM edge_source_reports ORDER BY reported_at DESC LIMIT 1`).Scan(&reported, &ports)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var list []int
	if len(ports) > 0 && json.Unmarshal(ports, &list) != nil {
		return out, errors.New("unreadable edge report")
	}
	if list != nil {
		out.ObservedPorts, out.ObservedAt = list, &reported
	}
	return out, nil
}
