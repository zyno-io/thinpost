package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
)

// Sending IPs mirror the Edge's report so the console can list the addresses
// and pin apps to them by a stable id. Nothing here lets a caller create, edit
// or delete a row: they are written only from the report. None of the code
// here logs, and no error it returns carries database text, so an address
// never leaves the store.

// maxSendingIPs bounds the list: the Edge can advertise at most 16 addresses
// and pinned rows it no longer reports stay.
const maxSendingIPs = 64

// RecordEdgeSources stores what the worker reported in Ready and mirrors it
// into sending_ips. Delivery is addressed from the live session's report, never
// from this copy. Only the live lease holder may write, and a stale epoch
// writes nothing (not even rows). In one transaction, with report writers
// serialized by a table lock that does not block pins:
//
//   - each reported address is upserted by address, refreshing its HELO name;
//     the address of a row never changes;
//   - rows that are neither reported nor pinned by an app are deleted, inside
//     a savepoint: if a concurrent pin wins the race the foreign key refuses
//     the delete, the savepoint is rolled back, the row is kept and the report
//     still commits (the next report prunes it);
//   - apps are never touched, so a report cannot move an app to another
//     address, and a pinned row is never deleted.
//
// A worker that reports no usable sources is stored with a NULL address list
// (unknown, not empty) and prunes every unpinned row.
func (r *Repository) RecordEdgeSources(ctx context.Context, lease ControllerLease, caps delivery.EdgeCapabilities) error {
	if !leaseID(lease.WorkerID) || !leaseID(lease.ControllerID) || lease.Epoch == 0 {
		return errors.New("invalid controller lease")
	}
	var addresses any
	reported := make([]string, 0, len(caps.Sources))
	names := make([]string, 0, len(caps.Sources))
	for _, source := range caps.Sources {
		reported, names = append(reported, source.Address.String()), append(names, source.Hostname)
	}
	if len(reported) > 0 {
		addresses = reported
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
	var written string
	err = tx.QueryRowContext(ctx, `INSERT INTO edge_source_reports(worker_id,epoch,addresses,reported_at,submission_ports)
		VALUES ($1,$2,$3::text[]::inet[],clock_timestamp(),$4::text[]::integer[])
		ON CONFLICT (worker_id) DO UPDATE SET epoch=EXCLUDED.epoch,addresses=EXCLUDED.addresses,reported_at=EXCLUDED.reported_at,submission_ports=EXCLUDED.submission_ports
		WHERE edge_source_reports.epoch<=EXCLUDED.epoch RETURNING worker_id`, lease.WorkerID, lease.Epoch, addresses, ports).Scan(&written)
	if errors.Is(err, sql.ErrNoRows) {
		// A newer epoch already reported: this write is stale, so it must not
		// touch the rows either.
		return nil
	}
	if err != nil {
		return err
	}
	// SHARE ROW EXCLUSIVE serializes report writers against each other. It does
	// not conflict with the key-share locks that pinning an app takes.
	if _, err = tx.ExecContext(ctx, `LOCK TABLE sending_ips IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	for i, address := range reported {
		id, idErr := newOpaqueID()
		if idErr != nil {
			return idErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO sending_ips(id,address,helo_name) VALUES ($1,$2::text::inet,$3)
			ON CONFLICT (address) DO UPDATE SET helo_name=EXCLUDED.helo_name WHERE sending_ips.helo_name<>EXCLUDED.helo_name`, id, address, names[i]); err != nil {
			return errors.New("record sending ip failed")
		}
	}
	if _, err = tx.ExecContext(ctx, `SAVEPOINT prune_sending_ips`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM sending_ips s WHERE NOT (host(s.address)=ANY($1::text[]))
		AND NOT EXISTS (SELECT 1 FROM apps a WHERE a.sending_ip_id=s.id)`, reported)
	switch {
	case err == nil:
		if _, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT prune_sending_ips`); err != nil {
			return err
		}
	case sqlState(err) == "23503":
		// An app was pinned to a row between the check and the delete. The pin
		// wins; the row is pruned by a later report if it is unpinned again.
		if _, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT prune_sending_ips`); err != nil {
			return err
		}
	default:
		return errors.New("prune sending ips failed")
	}
	return tx.Commit()
}

// ListSendingIPs reads the rows with the Edge's last report, without the
// runtime gate, so the console works during a deploy. The main IP is first.
func (r *Repository) ListSendingIPs(ctx context.Context) (admin.SendingIPList, error) {
	out := admin.SendingIPList{IPs: []admin.SendingIP{}}
	tx, err := r.beginOperationsRead(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT ip.id,host(ip.address),ip.helo_name,ip.created_at,
		(SELECT count(*) FROM apps a WHERE a.sending_ip_id=ip.id)
		FROM sending_ips ip ORDER BY ip.helo_name,ip.address LIMIT $1`, maxSendingIPs)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var ip admin.SendingIP
		if err = rows.Scan(&ip.ID, &ip.Address, &ip.HeloName, &ip.CreatedAt, &ip.AppCount); err != nil {
			return out, err
		}
		out.IPs = append(out.IPs, ip)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM apps WHERE sending_ip_id IS NULL AND deleted_at IS NULL`).Scan(&out.UnpinnedAppCount); err != nil {
		return out, err
	}
	var reportedAt sql.NullTime
	var unknown sql.NullBool
	var addressJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT reported_at,addresses IS NULL,
		to_json(ARRAY(SELECT host(x) FROM unnest(addresses) WITH ORDINALITY AS t(x,n) ORDER BY n))
		FROM edge_source_reports ORDER BY reported_at DESC LIMIT 1`).Scan(&reportedAt, &unknown, &addressJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if !reportedAt.Valid {
		return out, tx.Commit()
	}
	out.EdgeReportedAt = &reportedAt.Time
	if unknown.Bool {
		return out, tx.Commit()
	}
	var addresses []string
	if json.Unmarshal(addressJSON, &addresses) != nil {
		return out, errors.New("unreadable edge report")
	}
	out.EdgeReportsIdentity = true
	if len(addresses) > 0 {
		out.EdgePrimaryAddress = &addresses[0]
	}
	for i := range out.IPs {
		ip := &out.IPs[i]
		onEdge := slices.Contains(addresses, ip.Address)
		ip.OnEdge = &onEdge
		ip.IsPrimary = len(addresses) > 0 && addresses[0] == ip.Address
	}
	sort.SliceStable(out.IPs, func(i, j int) bool { return out.IPs[i].IsPrimary && !out.IPs[j].IsPrimary })
	return out, tx.Commit()
}

// GetSendingIP reads one row for the DNS check.
func (r *Repository) GetSendingIP(ctx context.Context, id string) (admin.SendingIP, error) {
	var ip admin.SendingIP
	if !opaqueID(id) {
		return ip, admin.ErrNotFound
	}
	err := r.DB.QueryRowContext(ctx, `SELECT id,host(address),helo_name,created_at,(SELECT count(*) FROM apps a WHERE a.sending_ip_id=sending_ips.id) FROM sending_ips WHERE id=$1`, id).
		Scan(&ip.ID, &ip.Address, &ip.HeloName, &ip.CreatedAt, &ip.AppCount)
	if errors.Is(err, sql.ErrNoRows) {
		return ip, admin.ErrNotFound
	}
	return ip, err
}

// lockSendingIPForPin key-share locks a row, which blocks a concurrent prune of
// it until the caller's transaction ends, and reports whether the Edge's last
// report lists its address. A missing row is admin.ErrNotFound.
func lockSendingIPForPin(ctx context.Context, tx *sql.Tx, id string) (address, helo string, onEdge bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT host(ip.address),ip.helo_name,
		COALESCE(ip.address=ANY(r.addresses),false)
		FROM sending_ips ip
		LEFT JOIN LATERAL (SELECT addresses FROM edge_source_reports ORDER BY reported_at DESC LIMIT 1) r ON true
		WHERE ip.id=$1 FOR KEY SHARE OF ip`, id).Scan(&address, &helo, &onEdge)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, admin.ErrNotFound
	}
	return address, helo, onEdge, err
}

// GetAppSendingIP returns the app's choice; a nil ID is the main IP.
func (r *Repository) GetAppSendingIP(ctx context.Context, appID string) (admin.AppSendingIP, error) {
	var out admin.AppSendingIP
	if !opaqueID(appID) {
		return out, admin.ErrNotFound
	}
	var id, address, helo sql.NullString
	err := r.DB.QueryRowContext(ctx, `SELECT a.sending_ip_id,host(ip.address),ip.helo_name FROM apps a LEFT JOIN sending_ips ip ON ip.id=a.sending_ip_id WHERE a.id=$1`, appID).Scan(&id, &address, &helo)
	if errors.Is(err, sql.ErrNoRows) {
		return out, admin.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	return appSendingIP(id, address, helo), nil
}

func appSendingIP(id, address, helo sql.NullString) admin.AppSendingIP {
	var out admin.AppSendingIP
	if id.Valid {
		out.SendingIPID, out.Address, out.HeloName = &id.String, &address.String, &helo.String
	}
	return out
}

// SetAppSendingIP sets the app's sending IP (nil: back to the main IP).
// Queued and retried deliveries use the new IP at their next claim; an attempt
// already with the worker finishes on the old one. The IP is a routing choice,
// not an authorization, so there is no permit-time recheck. Choosing an IP the
// Edge is not reporting right now is admin.ErrConflict (an app pinned to one
// would have its mail wait); choosing none is always allowed. An identical
// choice changes and audits nothing, even for an IP no longer reported.
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
	var address, helo string
	var onEdge bool
	if sendingIPID != nil {
		// The key-share lock blocks a concurrent prune of the target row.
		if address, helo, onEdge, err = lockSendingIPForPin(ctx, tx, *sendingIPID); err != nil {
			return admin.AppSendingIP{}, err
		}
		target = *sendingIPID
	}
	var current sql.NullString
	// NO KEY UPDATE, not UPDATE: audit inserts by bounce-domain checks (which
	// hold a bounce row) take a KEY SHARE lock on the app, and must not wait
	// here while this waits for their row.
	err = tx.QueryRowContext(ctx, `SELECT sending_ip_id FROM apps WHERE id=$1 AND deleted_at IS NULL FOR NO KEY UPDATE`, appID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.AppSendingIP{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.AppSendingIP{}, err
	}
	result := appSendingIP(sql.NullString{String: stringValue(sendingIPID), Valid: sendingIPID != nil}, sql.NullString{String: address, Valid: sendingIPID != nil}, sql.NullString{String: helo, Valid: sendingIPID != nil})
	if current.Valid == (sendingIPID != nil) && (!current.Valid || current.String == *sendingIPID) {
		return result, tx.Commit()
	}
	if sendingIPID != nil && !onEdge {
		return admin.AppSendingIP{}, admin.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE apps SET sending_ip_id=$2 WHERE id=$1`, appID, target); err != nil {
		if sqlState(err) == "23503" {
			return admin.AppSendingIP{}, admin.ErrNotFound
		}
		return admin.AppSendingIP{}, err
	}
	// The app's bounce domains must now point at the new IP's HELO name. Check
	// them at the next tick, and drop any check already in flight: it was run
	// against the old name. Claims stop using a domain whose verified target is
	// not the live name immediately, without waiting for this.
	if _, err = tx.ExecContext(ctx, `UPDATE bounce_domains SET next_check_at=clock_timestamp(),check_token=NULL,check_until=NULL WHERE app_id=$1`, appID); err != nil {
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
