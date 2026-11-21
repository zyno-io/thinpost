package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
)

const smtpPolicyColumns = `smtp_from_mode,smtp_from_domain,smtp_from_address,smtp_from_only_unauthorized`

func scanSMTPPolicy(row rowScanner) (admin.SMTPFromPolicy, error) {
	var p admin.SMTPFromPolicy
	var domain, address sql.NullString
	if err := row.Scan(&p.Mode, &domain, &address, &p.OnlyUnauthorized); err != nil {
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
	return a.Mode == b.Mode && a.OnlyUnauthorized == b.OnlyUnauthorized && same(a.Domain, b.Domain) && same(a.Address, b.Address)
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
	target := ""
	switch {
	case policy.Domain != nil:
		target = *policy.Domain
	case policy.Address != nil:
		target = admin.SMTPAddressDomain(*policy.Address)
	}
	if target != "" {
		var verified bool
		err = tx.QueryRowContext(ctx, `SELECT true FROM sender_domains WHERE app_id=$1 AND domain=$2 AND active FOR SHARE`, appID, target).Scan(&verified)
		if errors.Is(err, sql.ErrNoRows) {
			return admin.SMTPFromPolicy{}, admin.ErrConflict
		}
		if err != nil {
			return admin.SMTPFromPolicy{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE apps SET smtp_from_mode=$2,smtp_from_domain=$3,smtp_from_address=$4,smtp_from_only_unauthorized=$5 WHERE id=$1`,
		appID, policy.Mode, policy.Domain, policy.Address, policy.OnlyUnauthorized); err != nil {
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

// SMTPConnection resolves the SMTP host: the HELO name of the app's chosen
// sending IP, else of the main IP (the first address) of the Edge's last
// report. The names are the ones the Edge reported, mirrored into sending_ips.
// HostStatus is "not_reported" while no report names the IP, and
// "ip_unavailable" when the app's IP is not among the addresses the Edge now
// reports.
func (r *Repository) SMTPConnection(ctx context.Context, appID string) (admin.SMTPConnection, error) {
	out := admin.SMTPConnection{Ports: admin.SMTPPorts(), ObservedPorts: []int{}, HostStatus: admin.SMTPHostNotReported}
	if !opaqueID(appID) {
		return out, admin.ErrInvalidInput
	}
	var assigned, haveReport, onEdge bool
	var helo, mainHelo sql.NullString
	err := r.DB.QueryRowContext(ctx, `SELECT a.sending_ip_id IS NOT NULL,s.helo_name,r.addresses IS NOT NULL,COALESCE(s.address=ANY(r.addresses),false),
		(SELECT m.helo_name FROM sending_ips m WHERE m.address=r.addresses[1])
		FROM apps a LEFT JOIN sending_ips s ON s.id=a.sending_ip_id
		LEFT JOIN LATERAL (SELECT addresses FROM edge_source_reports ORDER BY reported_at DESC LIMIT 1) r ON true
		WHERE a.id=$1`, appID).Scan(&assigned, &helo, &haveReport, &onEdge, &mainHelo)
	if errors.Is(err, sql.ErrNoRows) {
		return out, admin.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	switch {
	case assigned && helo.Valid && haveReport && onEdge:
		source := admin.SMTPHostAppSendingIP
		out.Host, out.HostSource, out.HostStatus = &helo.String, &source, admin.SMTPHostOK
	case assigned && haveReport:
		out.HostStatus = admin.SMTPHostIPUnavailable
	case !assigned && mainHelo.Valid:
		source := admin.SMTPHostPrimary
		out.Host, out.HostSource, out.HostStatus = &mainHelo.String, &source, admin.SMTPHostOK
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
