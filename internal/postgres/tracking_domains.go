package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/customdomain"
	"github.com/zyno-io/thinpost/internal/oplog"
)

// trackingFreshness is how recent a passing check must be for preparation to
// use a tracking domain (customdomain.Freshness; the claim query is plain SQL).
const trackingFreshness = `interval '3 hours'`

// trackingDomainColumns is what scanTrackingDomain reads.
const trackingDomainColumns = `domain,state,COALESCE(check_until>clock_timestamp(),false),dns_result,https_result,observed_target,certificate_expires_at,checked_at,last_ok_at,verified_at,removed_at,created_at`

func scanTrackingDomain(row interface{ Scan(...any) error }) (admin.TrackingDomain, error) {
	var d admin.TrackingDomain
	var dns, https, observed sql.NullString
	var expires, checked, lastOK, verified, removed sql.NullTime
	if err := row.Scan(&d.Name, &d.State, &d.Checking, &dns, &https, &observed, &expires, &checked, &lastOK, &verified, &removed, &d.CreatedAt); err != nil {
		return d, err
	}
	if dns.Valid {
		d.DNSResult = &dns.String
	}
	if https.Valid {
		d.HTTPSResult = &https.String
	}
	if observed.Valid {
		d.ObservedTarget = &observed.String
	}
	for _, f := range []struct {
		from sql.NullTime
		to   **time.Time
	}{{expires, &d.CertificateExpiresAt}, {checked, &d.CheckedAt}, {lastOK, &d.LastOKAt}, {verified, &d.VerifiedAt}, {removed, &d.RemovedAt}} {
		if f.from.Valid {
			t := f.from.Time
			*f.to = &t
		}
	}
	return d, nil
}

// ListTrackingDomains returns the app's live (not removed) tracking domains by
// sender domain.
func (r *Repository) ListTrackingDomains(ctx context.Context, appID string) (map[string]admin.TrackingDomain, error) {
	if !opaqueID(appID) {
		return nil, admin.ErrInvalidInput
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT sender_domain,`+trackingDomainColumns+` FROM tracking_domains WHERE app_id=$1 AND state<>'removed'`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]admin.TrackingDomain{}
	for rows.Next() {
		var sender string
		d, err := scanTrackingDomain(scanFunc(func(dest ...any) error { return rows.Scan(append([]any{&sender}, dest...)...) }))
		if err != nil {
			return nil, err
		}
		out[sender] = d
	}
	return out, rows.Err()
}

// ListAllTrackingDomains lists every tracking domain of every app, removed
// tombstones included, for the operator who runs the routes and certificates.
func (r *Repository) ListAllTrackingDomains(ctx context.Context) ([]admin.TrackingDomainListing, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT app_id,sender_domain,`+trackingDomainColumns+` FROM tracking_domains ORDER BY domain,app_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []admin.TrackingDomainListing
	for rows.Next() {
		var item admin.TrackingDomainListing
		d, err := scanTrackingDomain(scanFunc(func(dest ...any) error { return rows.Scan(append([]any{&item.AppID, &item.SenderDomain}, dest...)...) }))
		if err != nil {
			return nil, err
		}
		item.TrackingDomain = d
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *Repository) GetTrackingDomain(ctx context.Context, appID, senderDomain string) (admin.TrackingDomain, error) {
	if !opaqueID(appID) || !validInboundDomainName(senderDomain) {
		return admin.TrackingDomain{}, admin.ErrInvalidInput
	}
	d, err := scanTrackingDomain(r.DB.QueryRowContext(ctx, `SELECT `+trackingDomainColumns+` FROM tracking_domains WHERE app_id=$1 AND sender_domain=$2 AND state<>'removed'`, appID, senderDomain))
	if errors.Is(err, sql.ErrNoRows) {
		return d, admin.ErrNotFound
	}
	return d, err
}

// SetTrackingDomain gives a verified sender domain its tracking domain, or
// replaces the one it has, in one transaction. The new domain starts pending
// and is used for links only after a check passes; until then links use the
// deployment's tracking address. The replaced domain becomes a tombstone, still
// checked daily, because links already sent through it keep working only while
// its route does. A name that was removed earlier is revived as pending.
func (r *Repository) SetTrackingDomain(ctx context.Context, actor, appID, senderDomain, name string) (admin.TrackingDomain, error) {
	if !adminText(actor, 256) || !opaqueID(appID) || !validInboundDomainName(senderDomain) || !validInboundDomainName(name) || len(name) > 253 || !customdomain.StrictSubdomain(name, senderDomain) {
		return admin.TrackingDomain{}, admin.ErrInvalidInput
	}
	id, err := newOpaqueID()
	if err != nil {
		return admin.TrackingDomain{}, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.TrackingDomain{}, err
	}
	defer tx.Rollback()
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT active FROM apps WHERE id=$1 FOR SHARE`, appID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.TrackingDomain{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.TrackingDomain{}, err
	}
	if !active {
		return admin.TrackingDomain{}, admin.ErrAppPaused
	}
	var verified bool
	err = tx.QueryRowContext(ctx, `SELECT active FROM sender_domains WHERE app_id=$1 AND domain=$2 FOR SHARE`, appID, senderDomain).Scan(&verified)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.TrackingDomain{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.TrackingDomain{}, err
	}
	if !verified {
		return admin.TrackingDomain{}, admin.ErrSenderDomainUnverified
	}
	var oldID, oldName string
	err = tx.QueryRowContext(ctx, `SELECT id,domain FROM tracking_domains WHERE app_id=$1 AND sender_domain=$2 AND state<>'removed' FOR UPDATE`, appID, senderDomain).Scan(&oldID, &oldName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return admin.TrackingDomain{}, err
	}
	if err == nil && oldName == name {
		d, err := scanTrackingDomain(tx.QueryRowContext(ctx, `SELECT `+trackingDomainColumns+` FROM tracking_domains WHERE id=$1`, oldID))
		if err != nil {
			return admin.TrackingDomain{}, err
		}
		return d, tx.Commit()
	}
	// The same name may already be a row of this app: a removed one is revived,
	// a live one under another sender domain is a conflict.
	var sameID, sameState string
	err = tx.QueryRowContext(ctx, `SELECT id,state FROM tracking_domains WHERE app_id=$1 AND domain=$2 FOR UPDATE`, appID, name).Scan(&sameID, &sameState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return admin.TrackingDomain{}, err
	}
	if err == nil && sameState != customdomain.StateRemoved {
		return admin.TrackingDomain{}, admin.ErrConflict
	}
	if oldID != "" {
		if err = tombstoneTrackingDomain(ctx, tx, oldID); err != nil {
			return admin.TrackingDomain{}, err
		}
		if err = recordAudit(ctx, tx, actor, appID, "tracking_domain.removed", oldName); err != nil {
			return admin.TrackingDomain{}, err
		}
	}
	var d admin.TrackingDomain
	if sameID != "" {
		d, err = scanTrackingDomain(tx.QueryRowContext(ctx, `UPDATE tracking_domains SET sender_domain=$2,state='pending',dns_result=NULL,https_result=NULL,observed_target=NULL,
			certificate_expires_at=NULL,checked_at=NULL,last_ok_at=NULL,verified_at=NULL,removed_at=NULL,failures=0,next_check_at=clock_timestamp(),
			check_token=NULL,check_until=NULL,created_at=clock_timestamp() WHERE id=$1 RETURNING `+trackingDomainColumns, sameID, senderDomain))
	} else {
		d, err = scanTrackingDomain(tx.QueryRowContext(ctx, `INSERT INTO tracking_domains(id,app_id,sender_domain,domain) VALUES ($1,$2,$3,$4) RETURNING `+trackingDomainColumns, id, appID, senderDomain, name))
	}
	if isUniqueViolation(err) {
		return admin.TrackingDomain{}, admin.ErrConflict
	}
	if err != nil {
		return admin.TrackingDomain{}, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "tracking_domain.added", name); err != nil {
		return admin.TrackingDomain{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.TrackingDomain{}, err
	}
	return d, nil
}

// tombstoneTrackingDomain marks a row removed. Dropping the lease token means a
// check in flight writes nothing; the row is next checked in a day.
func tombstoneTrackingDomain(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `UPDATE tracking_domains SET state='removed',removed_at=clock_timestamp(),failures=0,
		next_check_at=clock_timestamp()+interval '24 hours',check_token=NULL,check_until=NULL WHERE id=$1`, id)
	return err
}

// DeleteTrackingDomain removes the sender domain's tracking domain: new mail
// uses the deployment's tracking address at once. The row stays as a tombstone
// that is checked daily and listed for operators.
func (r *Repository) DeleteTrackingDomain(ctx context.Context, actor, appID, senderDomain string) error {
	if !adminText(actor, 256) || !opaqueID(appID) || !validInboundDomainName(senderDomain) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id, name string
	err = tx.QueryRowContext(ctx, `SELECT id,domain FROM tracking_domains WHERE app_id=$1 AND sender_domain=$2 AND state<>'removed' FOR UPDATE`, appID, senderDomain).Scan(&id, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err = tombstoneTrackingDomain(ctx, tx, id); err != nil {
		return err
	}
	if err = recordAudit(ctx, tx, actor, appID, "tracking_domain.removed", name); err != nil {
		return err
	}
	return tx.Commit()
}

// BeginTrackingCheck leases the live row to a manual check under a fresh token.
// The later claimant wins: a background check already running for the row
// writes nothing when it finishes.
func (r *Repository) BeginTrackingCheck(ctx context.Context, appID, senderDomain string) (customdomain.Check, error) {
	var check customdomain.Check
	if !opaqueID(appID) || !validInboundDomainName(senderDomain) {
		return check, admin.ErrInvalidInput
	}
	token, err := newOpaqueID()
	if err != nil {
		return check, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return check, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `UPDATE tracking_domains SET check_token=$3,check_until=clock_timestamp()+`+manualLease+`
		WHERE app_id=$1 AND sender_domain=$2 AND state<>'removed' RETURNING id,app_id,domain`, appID, senderDomain, token).Scan(&check.ID, &check.AppID, &check.Domain)
	if errors.Is(err, sql.ErrNoRows) {
		return check, admin.ErrNotFound
	}
	if err != nil {
		return check, err
	}
	if err = tx.Commit(); err != nil {
		return check, err
	}
	check.Token = token
	return check, nil
}

// ClaimDueTrackingDomains leases up to limit rows (removed ones included) that
// are due and not under a live lease, under one fresh token, with FOR UPDATE
// SKIP LOCKED so replicas never take the same row. An expired lease (a replica
// that died mid-check) is claimable again.
func (r *Repository) ClaimDueTrackingDomains(ctx context.Context, limit int) ([]customdomain.Check, error) {
	if limit <= 0 {
		return nil, nil
	}
	token, err := newOpaqueID()
	if err != nil {
		return nil, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `UPDATE tracking_domains SET check_token=$1,check_until=clock_timestamp()+`+backgroundLease+`
		WHERE id IN (SELECT id FROM tracking_domains
			WHERE next_check_at<=clock_timestamp() AND (check_until IS NULL OR check_until<clock_timestamp())
			ORDER BY next_check_at FOR UPDATE SKIP LOCKED LIMIT $2)
		RETURNING id,app_id,domain`, token, limit)
	if err != nil {
		return nil, err
	}
	var checks []customdomain.Check
	for rows.Next() {
		check := customdomain.Check{Token: token}
		if err = rows.Scan(&check.ID, &check.AppID, &check.Domain); err != nil {
			rows.Close()
			return nil, err
		}
		checks = append(checks, check)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return checks, nil
}

// FinishTrackingCheck records a check's result if its lease token is still the
// row's, and otherwise writes nothing (written=false): a newer check or a
// removal took the row while this one ran. Only the HTTPS result moves the
// state (customdomain.NextTracking); the DNS result is recorded for people to
// read. A removed row stays removed and is checked again in a day. A change of
// state is audited (tracking_domain.activated or failing) and logged with
// fixed fields.
func (r *Repository) FinishTrackingCheck(ctx context.Context, actor string, check customdomain.Check, outcome customdomain.TrackingOutcome) (admin.TrackingDomain, bool, error) {
	if !adminText(actor, 256) || !opaqueID(check.ID) || !opaqueID(check.Token) || !outcome.DNS.Valid() || !outcome.HTTPS.Valid() {
		return admin.TrackingDomain{}, false, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.TrackingDomain{}, false, err
	}
	defer tx.Rollback()
	var appID, domain, state string
	var failures int
	var createdAge, okAge sql.NullFloat64
	err = tx.QueryRowContext(ctx, `SELECT app_id,domain,state,failures,extract(epoch FROM clock_timestamp()-created_at),extract(epoch FROM clock_timestamp()-last_ok_at)
		FROM tracking_domains WHERE id=$1 AND check_token=$2 FOR UPDATE`, check.ID, check.Token).Scan(&appID, &domain, &state, &failures, &createdAge, &okAge)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.TrackingDomain{}, false, nil
	}
	if err != nil {
		return admin.TrackingDomain{}, false, err
	}
	next := customdomain.NextTracking(customdomain.Status{State: state, Failures: failures}, outcome.HTTPS)
	age := createdAge.Float64
	if next.State == customdomain.StateFailing && okAge.Valid {
		age = okAge.Float64
	}
	wait := customdomain.TrackingInterval(next.State, time.Duration(age*float64(time.Second)))
	var expires sql.NullTime
	if !outcome.CertificateExpires.IsZero() {
		expires = sql.NullTime{Time: outcome.CertificateExpires, Valid: true}
	}
	// A passing HTTPS check stamps last_ok_at (and the first one verified_at);
	// a domain that has never passed keeps both empty.
	ok := outcome.HTTPS == customdomain.HTTPSOK
	d, err := scanTrackingDomain(tx.QueryRowContext(ctx, `UPDATE tracking_domains SET state=$3,dns_result=$4,https_result=$5,observed_target=NULLIF($6,''),
		certificate_expires_at=CASE WHEN $7::timestamptz IS NULL THEN certificate_expires_at ELSE $7 END,
		checked_at=clock_timestamp(),
		last_ok_at=CASE WHEN $8 THEN clock_timestamp() ELSE last_ok_at END,
		verified_at=CASE WHEN $8 THEN COALESCE(verified_at,clock_timestamp()) ELSE verified_at END,
		failures=$9,next_check_at=clock_timestamp()+$10*interval '1 millisecond',check_token=NULL,check_until=NULL
		WHERE id=$1 AND check_token=$2 RETURNING `+trackingDomainColumns, check.ID, check.Token, next.State, string(outcome.DNS), string(outcome.HTTPS), outcome.Observed, expires, ok, next.Failures, wait.Milliseconds()))
	if err != nil {
		return admin.TrackingDomain{}, false, err
	}
	changed := next.State != state
	if changed {
		if err = recordAudit(ctx, tx, actor, appID, "tracking_domain."+auditVerb(next.State), domain); err != nil {
			return admin.TrackingDomain{}, false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return admin.TrackingDomain{}, false, err
	}
	if changed {
		oplog.DomainCheck(appID, "tracking", next.State, string(outcome.HTTPS))
	}
	return d, true, nil
}
