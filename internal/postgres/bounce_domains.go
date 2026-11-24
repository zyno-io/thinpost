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

// BackgroundCheckActor is the audit actor of a check run by the checker loop.
const BackgroundCheckActor = "thinpost:check"

// bounceFreshness is how recent a passing check must be for the claim to use a
// bounce domain (customdomain.Freshness; the claim query is plain SQL).
const bounceFreshness = `interval '3 hours'`

// Lease lengths. A background lease outlives a slow lookup; a manual check's
// only has to keep the background checker off the row while the console waits.
const (
	backgroundLease = `interval '2 minutes'`
	manualLease     = `interval '30 seconds'`
)

const bounceDomainColumns = `domain,state,COALESCE(check_until>clock_timestamp(),false),last_result,observed_target,checked_at,last_ok_at,verified_at,created_at`

type scanFunc func(...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }

func scanBounceDomain(row interface{ Scan(...any) error }) (admin.BounceDomain, error) {
	var d admin.BounceDomain
	var result, observed sql.NullString
	var checked, lastOK, verified sql.NullTime
	if err := row.Scan(&d.Name, &d.State, &d.Checking, &result, &observed, &checked, &lastOK, &verified, &d.CreatedAt); err != nil {
		return d, err
	}
	if result.Valid {
		d.Result = &result.String
	}
	if observed.Valid {
		d.ObservedTarget = &observed.String
	}
	if checked.Valid {
		d.CheckedAt = &checked.Time
	}
	if lastOK.Valid {
		d.LastOKAt = &lastOK.Time
	}
	if verified.Valid {
		d.VerifiedAt = &verified.Time
	}
	return d, nil
}

// lockDomainName serializes everything that decides who may use a name as a
// bounce domain or a receiving domain. The two kinds must never share a name,
// and each side checks the other under this lock. Take it after beginRuntime.
func lockDomainName(ctx context.Context, tx *sql.Tx, domain string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('thinpost-domain:'||$1::text,0))`, domain)
	return err
}

func (r *Repository) ListBounceDomains(ctx context.Context, appID string) (map[string]admin.BounceDomain, error) {
	if !opaqueID(appID) {
		return nil, admin.ErrInvalidInput
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT sender_domain,`+bounceDomainColumns+` FROM bounce_domains WHERE app_id=$1`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]admin.BounceDomain{}
	for rows.Next() {
		var sender string
		d, err := scanBounceDomain(scanFunc(func(dest ...any) error { return rows.Scan(append([]any{&sender}, dest...)...) }))
		if err != nil {
			return nil, err
		}
		out[sender] = d
	}
	return out, rows.Err()
}

func (r *Repository) GetBounceDomain(ctx context.Context, appID, senderDomain string) (admin.BounceDomain, error) {
	if !opaqueID(appID) || !validInboundDomainName(senderDomain) {
		return admin.BounceDomain{}, admin.ErrInvalidInput
	}
	d, err := scanBounceDomain(r.DB.QueryRowContext(ctx, `SELECT `+bounceDomainColumns+` FROM bounce_domains WHERE app_id=$1 AND sender_domain=$2`, appID, senderDomain))
	if errors.Is(err, sql.ErrNoRows) {
		return d, admin.ErrNotFound
	}
	return d, err
}

// SetBounceDomain gives a verified sender domain its bounce domain, or
// replaces the one it has, in one transaction. The new domain starts pending
// and is used for mail only after a check passes; until then mail uses the
// HELO name. The name must be a strict subdomain of the sender domain and can
// never be a receiving domain that takes mail (checked under the domain-name
// lock, which the inbound side takes too).
func (r *Repository) SetBounceDomain(ctx context.Context, actor, appID, senderDomain, name string) (admin.BounceDomain, error) {
	if !adminText(actor, 256) || !opaqueID(appID) || !validInboundDomainName(senderDomain) || !validInboundDomainName(name) || len(name) > 219 || !customdomain.StrictSubdomain(name, senderDomain) {
		return admin.BounceDomain{}, admin.ErrInvalidInput
	}
	id, err := newOpaqueID()
	if err != nil {
		return admin.BounceDomain{}, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.BounceDomain{}, err
	}
	defer tx.Rollback()
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT active FROM apps WHERE id=$1 FOR SHARE`, appID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.BounceDomain{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.BounceDomain{}, err
	}
	if !active {
		return admin.BounceDomain{}, admin.ErrAppPaused
	}
	var verified bool
	err = tx.QueryRowContext(ctx, `SELECT active FROM sender_domains WHERE app_id=$1 AND domain=$2 AND removed_at IS NULL FOR SHARE`, appID, senderDomain).Scan(&verified)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.BounceDomain{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.BounceDomain{}, err
	}
	if !verified {
		return admin.BounceDomain{}, admin.ErrSenderDomainUnverified
	}
	var oldID, oldName string
	err = tx.QueryRowContext(ctx, `SELECT id,domain FROM bounce_domains WHERE app_id=$1 AND sender_domain=$2 FOR UPDATE`, appID, senderDomain).Scan(&oldID, &oldName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return admin.BounceDomain{}, err
	}
	if err == nil && oldName == name {
		d, err := scanBounceDomain(tx.QueryRowContext(ctx, `SELECT `+bounceDomainColumns+` FROM bounce_domains WHERE id=$1`, oldID))
		if err != nil {
			return admin.BounceDomain{}, err
		}
		return d, tx.Commit()
	}
	if err = lockDomainName(ctx, tx, name); err != nil {
		return admin.BounceDomain{}, err
	}
	var taken bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM inbound_domains WHERE domain=$1 AND state IN ('active','disabled'))`, name).Scan(&taken); err != nil {
		return admin.BounceDomain{}, err
	}
	if taken {
		return admin.BounceDomain{}, admin.ErrInboundDomainClash
	}
	if oldID != "" {
		if _, err = tx.ExecContext(ctx, `DELETE FROM bounce_domains WHERE id=$1`, oldID); err != nil {
			return admin.BounceDomain{}, err
		}
		if err = recordAudit(ctx, tx, actor, appID, "bounce_domain.removed", oldName); err != nil {
			return admin.BounceDomain{}, err
		}
	}
	d, err := scanBounceDomain(tx.QueryRowContext(ctx, `INSERT INTO bounce_domains(id,app_id,sender_domain,domain) VALUES ($1,$2,$3,$4) RETURNING `+bounceDomainColumns, id, appID, senderDomain, name))
	if isUniqueViolation(err) {
		// Another of the app's sender domains already uses this name.
		return admin.BounceDomain{}, admin.ErrConflict
	}
	if err != nil {
		return admin.BounceDomain{}, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "bounce_domain.added", name); err != nil {
		return admin.BounceDomain{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.BounceDomain{}, err
	}
	return d, nil
}

// DeleteBounceDomain removes the sender domain's bounce domain. Mail goes back
// to the HELO name at once; bounces already on their way to the old domain are
// still accepted by the edge, which takes bounce tokens at any domain.
func (r *Repository) DeleteBounceDomain(ctx context.Context, actor, appID, senderDomain string) error {
	if !adminText(actor, 256) || !opaqueID(appID) || !validInboundDomainName(senderDomain) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name string
	err = tx.QueryRowContext(ctx, `DELETE FROM bounce_domains WHERE app_id=$1 AND sender_domain=$2 RETURNING domain`, appID, senderDomain).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err = recordAudit(ctx, tx, actor, appID, "bounce_domain.removed", name); err != nil {
		return err
	}
	return tx.Commit()
}

// BounceTarget is the HELO name an app's bounce domains must be a CNAME to: its
// pinned sending IP's name, else the main IP's name from the latest edge
// report (the same source as EdgeIdentity). Name is empty while that is
// unknown. Reported lists every HELO name the edge reports.
func (r *Repository) BounceTarget(ctx context.Context, appID string) (customdomain.Target, error) {
	var out customdomain.Target
	if !opaqueID(appID) {
		return out, admin.ErrInvalidInput
	}
	var pinned bool
	var pinnedName, pinnedAddress, mainName, mainAddress sql.NullString
	err := r.DB.QueryRowContext(ctx, `SELECT a.sending_ip_id IS NOT NULL,s.helo_name,host(s.address),
		(SELECT m.helo_name FROM sending_ips m WHERE m.address=r.addresses[1]),host(r.addresses[1])
		FROM apps a LEFT JOIN sending_ips s ON s.id=a.sending_ip_id
		LEFT JOIN LATERAL (SELECT addresses FROM edge_source_reports ORDER BY reported_at DESC LIMIT 1) r ON true
		WHERE a.id=$1`, appID).Scan(&pinned, &pinnedName, &pinnedAddress, &mainName, &mainAddress)
	if errors.Is(err, sql.ErrNoRows) {
		return out, admin.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	switch {
	case pinned && pinnedName.Valid:
		out.Name, out.Address = pinnedName.String, pinnedAddress.String
	case !pinned && mainName.Valid:
		out.Name, out.Address = mainName.String, mainAddress.String
	}
	identity, err := r.EdgeIdentity(ctx)
	if err != nil {
		return out, err
	}
	out.Reported = identity.Hostnames
	return out, nil
}

// BeginBounceCheck leases the row to a manual check under a fresh token. The
// later claimant wins: a background check already running for the row writes
// nothing when it finishes.
func (r *Repository) BeginBounceCheck(ctx context.Context, appID, senderDomain string) (customdomain.Check, error) {
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
	err = tx.QueryRowContext(ctx, `UPDATE bounce_domains SET check_token=$3,check_until=clock_timestamp()+`+manualLease+`
		WHERE app_id=$1 AND sender_domain=$2 RETURNING id,app_id,domain`, appID, senderDomain, token).Scan(&check.ID, &check.AppID, &check.Domain)
	if errors.Is(err, sql.ErrNoRows) {
		return check, admin.ErrNotFound
	}
	if err != nil {
		return check, err
	}
	if err = tx.Commit(); err != nil {
		return check, err
	}
	// The target is read after the lease is held, so an IP change that
	// invalidates the lease cannot leave this check with a stale target.
	if check.Target, err = r.BounceTarget(ctx, appID); err != nil {
		return customdomain.Check{}, err
	}
	check.Token = token
	return check, nil
}

// ClaimDueBounceDomains leases up to limit rows that are due and not under a
// live lease, under one fresh token, with FOR UPDATE SKIP LOCKED so replicas
// never take the same row. Each returned check carries the app's current
// target. An expired lease (a replica that died mid-check) is claimable again.
func (r *Repository) ClaimDueBounceDomains(ctx context.Context, limit int) ([]customdomain.Check, error) {
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
	rows, err := tx.QueryContext(ctx, `UPDATE bounce_domains SET check_token=$1,check_until=clock_timestamp()+`+backgroundLease+`
		WHERE id IN (SELECT id FROM bounce_domains
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
	targets := map[string]customdomain.Target{}
	for i := range checks {
		target, ok := targets[checks[i].AppID]
		if !ok {
			if target, err = r.BounceTarget(ctx, checks[i].AppID); err != nil {
				// The lease expires by itself; the row is tried again then.
				return nil, err
			}
			targets[checks[i].AppID] = target
		}
		checks[i].Target = target
	}
	return checks, nil
}

// FinishBounceCheck records a check's result if its lease token is still the
// row's, and otherwise writes nothing (written=false): a newer check, an IP
// change or a removal took the row while this one ran. The new state, failure
// count and next check follow customdomain.Next and Interval. A change of state
// is audited (bounce_domain.activated or failing) and logged with fixed fields.
func (r *Repository) FinishBounceCheck(ctx context.Context, actor string, check customdomain.Check, result customdomain.Result, observed string) (admin.BounceDomain, bool, error) {
	if !adminText(actor, 256) || !opaqueID(check.ID) || !opaqueID(check.Token) || !result.Valid() {
		return admin.BounceDomain{}, false, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.BounceDomain{}, false, err
	}
	defer tx.Rollback()
	var appID, domain, state string
	var failures int
	var createdAge, okAge sql.NullFloat64
	err = tx.QueryRowContext(ctx, `SELECT app_id,domain,state,failures,extract(epoch FROM clock_timestamp()-created_at),extract(epoch FROM clock_timestamp()-last_ok_at)
		FROM bounce_domains WHERE id=$1 AND check_token=$2 FOR UPDATE`, check.ID, check.Token).Scan(&appID, &domain, &state, &failures, &createdAge, &okAge)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.BounceDomain{}, false, nil
	}
	if err != nil {
		return admin.BounceDomain{}, false, err
	}
	next := customdomain.Next(customdomain.Status{State: state, Failures: failures}, result)
	// Pending waits by its age since creation; failing by the time since it
	// last passed (since creation if it never did).
	age := createdAge.Float64
	if next.State == customdomain.StateFailing && okAge.Valid {
		age = okAge.Float64
	}
	wait := customdomain.Interval(next.State, time.Duration(age*float64(time.Second)))
	var d admin.BounceDomain
	if result == customdomain.ResultOK {
		d, err = scanBounceDomain(tx.QueryRowContext(ctx, `UPDATE bounce_domains SET state='active',target=$3,last_result='ok',observed_target=NULLIF($4,''),
			checked_at=clock_timestamp(),last_ok_at=clock_timestamp(),verified_at=COALESCE(verified_at,clock_timestamp()),failures=0,
			next_check_at=clock_timestamp()+$5*interval '1 millisecond',check_token=NULL,check_until=NULL
			WHERE id=$1 AND check_token=$2 RETURNING `+bounceDomainColumns, check.ID, check.Token, check.Target.Name, observed, wait.Milliseconds()))
	} else {
		d, err = scanBounceDomain(tx.QueryRowContext(ctx, `UPDATE bounce_domains SET state=$3,last_result=$4,observed_target=NULLIF($5,''),
			checked_at=clock_timestamp(),failures=$6,next_check_at=clock_timestamp()+$7*interval '1 millisecond',check_token=NULL,check_until=NULL
			WHERE id=$1 AND check_token=$2 RETURNING `+bounceDomainColumns, check.ID, check.Token, next.State, string(result), observed, next.Failures, wait.Milliseconds()))
	}
	if err != nil {
		return admin.BounceDomain{}, false, err
	}
	changed := next.State != state
	if changed {
		if err = recordAudit(ctx, tx, actor, appID, "bounce_domain."+auditVerb(next.State), domain); err != nil {
			return admin.BounceDomain{}, false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return admin.BounceDomain{}, false, err
	}
	if changed {
		oplog.DomainCheck(appID, "bounce", next.State, string(result))
	}
	return d, true, nil
}

func auditVerb(state string) string {
	if state == customdomain.StateActive {
		return "activated"
	}
	return state // failing
}
