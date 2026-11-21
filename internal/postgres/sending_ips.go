package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"strconv"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/egress"
)

// Sending IPs name the Edge's outbound addresses and assign them to apps. None
// of the code here logs, and no error it returns carries database text, so an
// address (which unique-violation details would quote) never leaves the store.

// maxSendingIPs bounds the pool: the Edge can advertise at most 16 addresses.
const maxSendingIPs = 64

// RecordEdgeSources stores what the worker reported in Ready, for the console
// only: delivery is addressed from the live session's report, never from this
// copy. Only the live lease holder may write, and a stale epoch cannot
// overwrite a newer one. The names are stored in the bounce_domains column,
// since a source's name is its bounce domain. A worker that reports no usable
// sources is stored with NULL lists (unknown, not empty).
func (r *Repository) RecordEdgeSources(ctx context.Context, lease ControllerLease, caps delivery.EdgeCapabilities) error {
	if !leaseID(lease.WorkerID) || !leaseID(lease.ControllerID) || lease.Epoch == 0 {
		return errors.New("invalid controller lease")
	}
	var addresses, domains any
	if len(caps.Sources) > 0 {
		list, names := make([]string, 0, len(caps.Sources)), make([]string, 0, len(caps.Sources))
		for _, source := range caps.Sources {
			list, names = append(list, source.Address.String()), append(names, source.Hostname)
		}
		addresses, domains = list, names
	}
	var ports any
	if caps.SubmissionPorts != nil {
		// The ports come from the edge's report, which is not trusted. Keep only
		// valid, distinct ports and at most the schema's 8, so a bad report can
		// never fail the upsert and drop the rest of the source report.
		list := make([]string, 0, len(caps.SubmissionPorts))
		seen := make(map[int]bool, len(caps.SubmissionPorts))
		for _, port := range caps.SubmissionPorts {
			if port < 1 || port > 65535 || seen[port] || len(list) == 8 {
				continue
			}
			seen[port] = true
			list = append(list, strconv.Itoa(port))
		}
		ports = list
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	live, err := lockLiveControllerLease(ctx, tx, lease)
	if err != nil {
		return err
	}
	if !live {
		return delivery.ErrLeaseInvalid
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO edge_source_reports(worker_id,epoch,addresses,bounce_domains,reported_at,submission_ports)
		VALUES ($1,$2,$3::text[]::inet[],$4::text[],clock_timestamp(),$5::text[]::integer[])
		ON CONFLICT (worker_id) DO UPDATE SET epoch=EXCLUDED.epoch,addresses=EXCLUDED.addresses,bounce_domains=EXCLUDED.bounce_domains,reported_at=EXCLUDED.reported_at,submission_ports=EXCLUDED.submission_ports
		WHERE edge_source_reports.epoch<=EXCLUDED.epoch`, lease.WorkerID, lease.Epoch, addresses, domains, ports); err != nil {
		return err
	}
	return tx.Commit()
}

// ListSendingIPs reads the pool with the Edge's last report, without the
// runtime gate, so the console works during a deploy.
func (r *Repository) ListSendingIPs(ctx context.Context) (admin.SendingIPList, error) {
	out := admin.SendingIPList{IPs: []admin.SendingIP{}, UnnamedEdgeAddresses: []string{}}
	tx, err := r.beginOperationsRead(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT ip.id,ip.name,host(ip.address),ip.helo_name,ip.bounce_domain,ip.created_at,
		(SELECT count(*) FROM apps a WHERE a.sending_ip_id=ip.id)
		FROM sending_ips ip ORDER BY ip.name LIMIT $1`, maxSendingIPs)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var ip admin.SendingIP
		if err = rows.Scan(&ip.ID, &ip.Name, &ip.Address, &ip.HeloName, &ip.BounceDomain, &ip.CreatedAt, &ip.AppCount); err != nil {
			return out, err
		}
		out.IPs = append(out.IPs, ip)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM apps WHERE sending_ip_id IS NULL`).Scan(&out.UnpinnedAppCount); err != nil {
		return out, err
	}
	var reportedAt sql.NullTime
	var legacy sql.NullBool
	var addressJSON, domainJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT reported_at,addresses IS NULL,
		to_json(ARRAY(SELECT host(x) FROM unnest(addresses) WITH ORDINALITY AS t(x,n) ORDER BY n)),to_json(bounce_domains)
		FROM edge_source_reports ORDER BY reported_at DESC LIMIT 1`).Scan(&reportedAt, &legacy, &addressJSON, &domainJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if !reportedAt.Valid {
		return out, tx.Commit()
	}
	out.EdgeReportedAt = &reportedAt.Time
	if legacy.Bool {
		return out, tx.Commit()
	}
	var addresses, domains []string
	if json.Unmarshal(addressJSON, &addresses) != nil || (len(domainJSON) > 0 && json.Unmarshal(domainJSON, &domains) != nil) {
		return out, errors.New("unreadable edge report")
	}
	out.EdgeReportsSources = true
	if len(addresses) > 0 {
		out.EdgePrimaryAddress = &addresses[0]
	}
	named := make(map[string]bool, len(out.IPs))
	for i := range out.IPs {
		ip := &out.IPs[i]
		// Mail is addressed from the edge's report: the bounce domain is the
		// reported name of the address it leaves from, so nothing waits on the
		// stored bounce domain any more.
		onEdge := slices.Contains(addresses, ip.Address)
		bounceOnEdge := onEdge
		ip.OnEdge, ip.BounceOnEdge = &onEdge, &bounceOnEdge
		ip.IsPrimary = len(addresses) > 0 && addresses[0] == ip.Address
		named[ip.Address] = true
	}
	for _, address := range addresses {
		if !named[address] {
			out.UnnamedEdgeAddresses = append(out.UnnamedEdgeAddresses, address)
		}
	}
	return out, tx.Commit()
}

// GetSendingIP reads one row for the DNS check.
func (r *Repository) GetSendingIP(ctx context.Context, id string) (admin.SendingIP, error) {
	var ip admin.SendingIP
	if !opaqueID(id) {
		return ip, admin.ErrNotFound
	}
	err := r.DB.QueryRowContext(ctx, `SELECT id,name,host(address),helo_name,bounce_domain,created_at,(SELECT count(*) FROM apps a WHERE a.sending_ip_id=sending_ips.id) FROM sending_ips WHERE id=$1`, id).
		Scan(&ip.ID, &ip.Name, &ip.Address, &ip.HeloName, &ip.BounceDomain, &ip.CreatedAt, &ip.AppCount)
	if errors.Is(err, sql.ErrNoRows) {
		return ip, admin.ErrNotFound
	}
	return ip, err
}

func validSendingIPInput(in admin.SendingIPInput) bool {
	if !admin.ValidSendingIPInput(in) {
		return false
	}
	address, err := netip.ParseAddr(in.Address)
	return err == nil && egress.PublicSource(address)
}

// auditSystem writes a system-scoped audit row (app_id NULL). Details never
// hold an address.
func auditSystem(ctx context.Context, tx *sql.Tx, actor, action, resource string, details map[string]any) error {
	id, err := newOpaqueID()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details) VALUES ($1,$2,NULL,$3,$4,$5::jsonb)`, id, actor, action, resource, string(encoded))
	return err
}

// CreateSendingIP adds a row. A duplicate name or address is admin.ErrConflict.
func (r *Repository) CreateSendingIP(ctx context.Context, actor string, in admin.SendingIPInput) (admin.SendingIP, error) {
	if !adminText(actor, 256) || !validSendingIPInput(in) {
		return admin.SendingIP{}, admin.ErrInvalidInput
	}
	id, err := newOpaqueID()
	if err != nil {
		return admin.SendingIP{}, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.SendingIP{}, err
	}
	defer tx.Rollback()
	// Serialize pool edits with each other and with assignments.
	if _, err = tx.ExecContext(ctx, `LOCK TABLE sending_ips IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return admin.SendingIP{}, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sending_ips`).Scan(&count); err != nil {
		return admin.SendingIP{}, err
	}
	if count >= maxSendingIPs {
		return admin.SendingIP{}, admin.ErrConflict
	}
	var ip admin.SendingIP
	err = tx.QueryRowContext(ctx, `INSERT INTO sending_ips(id,name,address,helo_name,bounce_domain) VALUES ($1,$2,$3::text::inet,$4,$5)
		RETURNING id,name,host(address),helo_name,bounce_domain,created_at`, id, in.Name, in.Address, in.HeloName, in.BounceDomain).
		Scan(&ip.ID, &ip.Name, &ip.Address, &ip.HeloName, &ip.BounceDomain, &ip.CreatedAt)
	if sqlState(err) == "23505" {
		return admin.SendingIP{}, admin.ErrConflict
	}
	if err != nil {
		return admin.SendingIP{}, errors.New("create sending ip failed")
	}
	if err = auditSystem(ctx, tx, actor, "sending_ip.created", id, map[string]any{"name": in.Name}); err != nil {
		return admin.SendingIP{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.SendingIP{}, err
	}
	return ip, nil
}

// UpdateSendingIP edits the name, HELO name or bounce domain. The address never
// changes. An identical request changes and audits nothing.
func (r *Repository) UpdateSendingIP(ctx context.Context, actor, id string, patch admin.SendingIPPatch) (admin.SendingIP, error) {
	if !adminText(actor, 256) || !opaqueID(id) || patch.Name == nil && patch.HeloName == nil && patch.BounceDomain == nil ||
		patch.Name != nil && !admin.ValidSendingIPName(*patch.Name) ||
		patch.HeloName != nil && !admin.ValidSendingIPDomain(*patch.HeloName, 253) ||
		patch.BounceDomain != nil && !admin.ValidSendingIPDomain(*patch.BounceDomain, 219) {
		return admin.SendingIP{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.SendingIP{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `LOCK TABLE sending_ips IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return admin.SendingIP{}, err
	}
	var ip admin.SendingIP
	err = tx.QueryRowContext(ctx, `SELECT id,name,host(address),helo_name,bounce_domain,created_at,(SELECT count(*) FROM apps a WHERE a.sending_ip_id=sending_ips.id) FROM sending_ips WHERE id=$1 FOR UPDATE`, id).
		Scan(&ip.ID, &ip.Name, &ip.Address, &ip.HeloName, &ip.BounceDomain, &ip.CreatedAt, &ip.AppCount)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.SendingIP{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.SendingIP{}, err
	}
	next := ip
	var changed []string
	if patch.Name != nil && *patch.Name != ip.Name {
		next.Name = *patch.Name
		changed = append(changed, "name")
	}
	if patch.HeloName != nil && *patch.HeloName != ip.HeloName {
		next.HeloName = *patch.HeloName
		changed = append(changed, "helo_name")
	}
	if patch.BounceDomain != nil && *patch.BounceDomain != ip.BounceDomain {
		next.BounceDomain = *patch.BounceDomain
		changed = append(changed, "bounce_domain")
	}
	if len(changed) == 0 {
		return ip, tx.Commit()
	}
	// A row saved before this rule may not satisfy it; only a change to the HELO
	// name or bounce domain has to.
	if (next.HeloName != ip.HeloName || next.BounceDomain != ip.BounceDomain) && !admin.HeloUnderBounceDomain(next.HeloName, next.BounceDomain) {
		return admin.SendingIP{}, admin.ErrInvalidInput
	}
	_, err = tx.ExecContext(ctx, `UPDATE sending_ips SET name=$2,helo_name=$3,bounce_domain=$4 WHERE id=$1`, id, next.Name, next.HeloName, next.BounceDomain)
	if sqlState(err) == "23505" {
		return admin.SendingIP{}, admin.ErrConflict
	}
	if err != nil {
		return admin.SendingIP{}, errors.New("update sending ip failed")
	}
	if err = auditSystem(ctx, tx, actor, "sending_ip.updated", id, map[string]any{"name": next.Name, "fields": changed}); err != nil {
		return admin.SendingIP{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.SendingIP{}, err
	}
	return next, nil
}

// DeleteSendingIP removes an unused row. It is admin.ErrConflict while any app
// is assigned the IP; the foreign key backs that check against a concurrent
// assignment. Attempt history keeps the raw address, so nothing is lost.
func (r *Repository) DeleteSendingIP(ctx context.Context, actor, id string) error {
	if !adminText(actor, 256) || !opaqueID(id) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `LOCK TABLE sending_ips IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	var name string
	var apps int
	err = tx.QueryRowContext(ctx, `SELECT name,(SELECT count(*) FROM apps a WHERE a.sending_ip_id=sending_ips.id) FROM sending_ips WHERE id=$1 FOR UPDATE`, id).Scan(&name, &apps)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if apps > 0 {
		return admin.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM sending_ips WHERE id=$1`, id); err != nil {
		if sqlState(err) == "23503" {
			return admin.ErrConflict
		}
		return errors.New("delete sending ip failed")
	}
	if err = auditSystem(ctx, tx, actor, "sending_ip.deleted", id, map[string]any{"name": name}); err != nil {
		return err
	}
	return tx.Commit()
}

// GetAppSendingIP returns the app's assignment; a nil ID is the primary.
func (r *Repository) GetAppSendingIP(ctx context.Context, appID string) (admin.AppSendingIP, error) {
	var out admin.AppSendingIP
	if !opaqueID(appID) {
		return out, admin.ErrNotFound
	}
	var id, name sql.NullString
	err := r.DB.QueryRowContext(ctx, `SELECT a.sending_ip_id,ip.name FROM apps a LEFT JOIN sending_ips ip ON ip.id=a.sending_ip_id WHERE a.id=$1`, appID).Scan(&id, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return out, admin.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	return appSendingIP(id, name), nil
}

func appSendingIP(id, name sql.NullString) admin.AppSendingIP {
	var out admin.AppSendingIP
	if id.Valid {
		out.SendingIPID = &id.String
		out.Name = &name.String
	}
	return out
}

// SetAppSendingIP assigns the app's sending IP (nil: back to the primary).
// Queued and retried deliveries use the new IP at their next claim; an attempt
// already with the worker finishes on the old one. The IP is a routing choice,
// not an authorization, so there is no permit-time recheck. An identical
// assignment changes and audits nothing.
func (r *Repository) SetAppSendingIP(ctx context.Context, actor, appID string, sendingIPID *string) (admin.AppSendingIP, error) {
	if !adminText(actor, 256) || !opaqueID(appID) || sendingIPID != nil && !opaqueID(*sendingIPID) {
		return admin.AppSendingIP{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.AppSendingIP{}, err
	}
	defer tx.Rollback()
	var target any
	var name sql.NullString
	if sendingIPID != nil {
		// A key-share lock blocks a concurrent delete of the target row.
		err = tx.QueryRowContext(ctx, `SELECT name FROM sending_ips WHERE id=$1 FOR KEY SHARE`, *sendingIPID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return admin.AppSendingIP{}, admin.ErrNotFound
		}
		if err != nil {
			return admin.AppSendingIP{}, err
		}
		target = *sendingIPID
	}
	var current sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT sending_ip_id FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.AppSendingIP{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.AppSendingIP{}, err
	}
	result := appSendingIP(sql.NullString{String: stringValue(sendingIPID), Valid: sendingIPID != nil}, name)
	if current.Valid == (sendingIPID != nil) && (!current.Valid || current.String == *sendingIPID) {
		return result, tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE apps SET sending_ip_id=$2 WHERE id=$1`, appID, target); err != nil {
		if sqlState(err) == "23503" {
			return admin.AppSendingIP{}, admin.ErrNotFound
		}
		return admin.AppSendingIP{}, err
	}
	auditID, err := newOpaqueID()
	if err != nil {
		return admin.AppSendingIP{}, err
	}
	var from any
	if current.Valid {
		from = current.String
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
		VALUES ($1,$2,$3,'app.sending_ip.updated',$3,jsonb_build_object('from',$4::text,'to',$5::text))`, auditID, actor, appID, from, target); err != nil {
		return admin.AppSendingIP{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.AppSendingIP{}, err
	}
	return result, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
