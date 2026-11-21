package postgres

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/dkimkey"
)

// A retired key's record stays in DNS for at least this long, and for the
// retry policy's maximum age plus two days if that is longer: mail signed with
// the old key may still be waiting to be retried.
const (
	minKeepRecord   = 7 * 24 * time.Hour
	keepRecordSlack = 2 * 24 * time.Hour
)

var generatedSelector = regexp.MustCompile(`^` + dkimkey.SelectorPrefix + `([0-9]+)$`)

func (r *Repository) ListDomains(ctx context.Context, appID string) ([]admin.Domain, error) {
	if !opaqueID(appID) {
		return nil, admin.ErrInvalidInput
	}
	// Domains and keys are read in one snapshot, so a domain or key created
	// meanwhile cannot appear in one read and not the other.
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, admin.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return loadDomains(ctx, tx, appID, "")
}

// loadDomains reads an app's sender domains (or just one) with their keys.
func loadDomains(ctx context.Context, q queryer, appID, only string) ([]admin.Domain, error) {
	rows, err := q.QueryContext(ctx, `SELECT domain,active,created_at,verified_at FROM sender_domains WHERE app_id=$1 AND ($2='' OR domain=$2) ORDER BY domain`, appID, only)
	if err != nil {
		return nil, err
	}
	var result []admin.Domain
	index := map[string]int{}
	for rows.Next() {
		var d admin.Domain
		if err = rows.Scan(&d.Name, &d.Active, &d.CreatedAt, &d.VerifiedAt); err != nil {
			rows.Close()
			return nil, err
		}
		index[d.Name] = len(result)
		result = append(result, d)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	keys, err := q.QueryContext(ctx, `SELECT domain,selector,state,public_key,published_at,activated_at,keep_record_until FROM dkim_keys
		WHERE app_id=$1 AND ($2='' OR domain=$2) AND (state IN ('active','pending') OR (state='retired' AND keep_record_until>clock_timestamp()))
		ORDER BY domain,retired_at DESC NULLS LAST`, appID, only)
	if err != nil {
		return nil, err
	}
	defer keys.Close()
	for keys.Next() {
		var domain, selector, state string
		var public []byte
		var published, activated, keep *time.Time
		if err = keys.Scan(&domain, &selector, &state, &public, &published, &activated, &keep); err != nil {
			return nil, err
		}
		i, ok := index[domain]
		if !ok {
			continue
		}
		d := &result[i]
		key := &admin.DKIMKey{Selector: selector, DNSName: dkimkey.RecordName(selector, domain), DNSValue: dkimkey.RecordValue(public), PublishedAt: published, ActivatedAt: activated}
		switch state {
		case "active":
			d.ActiveKey = key
		case "pending":
			d.PendingKey = key
		case "retired":
			if d.RetiredKey == nil && keep != nil {
				d.RetiredKey = &admin.DKIMRetiredKey{Selector: selector, KeepRecordUntil: *keep}
			}
		}
	}
	return result, keys.Err()
}

// RequestDomain starts verifying a sender domain: it records the domain and
// generates its first DKIM key. The record to publish is the key's TXT record,
// and finding it in DNS is the verification. Asking again returns the same
// pending record; a domain that already has an active key is a conflict (a
// rotation is a new key, not a new request).
func (r *Repository) RequestDomain(ctx context.Context, actor, appID, domain string) (admin.DomainChallenge, error) {
	return r.newKey(ctx, actor, appID, domain, true)
}

// CreateDKIMKey makes the domain's next pending key. It is how a domain
// verified before DKIM keys existed gets its first key, and how a rotation
// starts.
func (r *Repository) CreateDKIMKey(ctx context.Context, actor, appID, domain string) (admin.DomainChallenge, error) {
	return r.newKey(ctx, actor, appID, domain, false)
}

func (r *Repository) newKey(ctx context.Context, actor, appID, domain string, request bool) (admin.DomainChallenge, error) {
	normalized, ok := admin.NormalizeDomain(domain)
	if !ok || normalized != domain || !opaqueID(appID) || !adminText(actor, 256) {
		return admin.DomainChallenge{}, admin.ErrInvalidInput
	}
	if r.DKIMKeys == nil {
		return admin.DomainChallenge{}, errors.New("DKIM keys need the object encryption keyring")
	}
	// Generated before the transaction: it takes a moment and holds no lock.
	material, err := dkimkey.Generate()
	if err != nil {
		return admin.DomainChallenge{}, err
	}
	// Another app can take the selector this one picked; try again with the
	// next free one.
	for attempt := 0; attempt < 8; attempt++ {
		result, err := r.newKeyTx(ctx, actor, appID, domain, request, material)
		if err == nil || !isSelectorTaken(err) {
			return result, err
		}
	}
	return admin.DomainChallenge{}, errors.New("cannot allocate a DKIM selector")
}

func isSelectorTaken(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "dkim_keys_selector_idx"
}

func (r *Repository) newKeyTx(ctx context.Context, actor, appID, domain string, request bool, material dkimkey.Material) (admin.DomainChallenge, error) {
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.DomainChallenge{}, err
	}
	defer tx.Rollback()
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 AND active FOR SHARE`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.DomainChallenge{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.DomainChallenge{}, err
	}
	if request {
		if _, err = tx.ExecContext(ctx, `INSERT INTO sender_domains(app_id,domain,active) VALUES ($1,$2,false) ON CONFLICT (app_id,domain) DO NOTHING`, appID, domain); err != nil {
			return admin.DomainChallenge{}, err
		}
	}
	// The domain row serializes key changes for this (app, domain).
	err = tx.QueryRowContext(ctx, `SELECT true FROM sender_domains WHERE app_id=$1 AND domain=$2 FOR UPDATE`, appID, domain).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.DomainChallenge{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.DomainChallenge{}, err
	}
	var pending, active bool
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(bool_or(state='pending'),false),COALESCE(bool_or(state='active'),false) FROM dkim_keys WHERE app_id=$1 AND domain=$2`, appID, domain).Scan(&pending, &active); err != nil {
		return admin.DomainChallenge{}, err
	}
	switch {
	case pending && request:
		// Asking again shows the same record; the public key is not a secret.
	case pending || (active && request):
		return admin.DomainChallenge{}, admin.ErrConflict
	default:
		if err = insertPendingKey(ctx, tx, r.DKIMKeys, appID, domain, material); err != nil {
			return admin.DomainChallenge{}, err
		}
		if err = recordAudit(ctx, tx, actor, appID, "domain.dkim_key.created", domain); err != nil {
			return admin.DomainChallenge{}, err
		}
	}
	domains, err := loadDomains(ctx, tx, appID, domain)
	if err != nil || len(domains) != 1 || domains[0].PendingKey == nil {
		return admin.DomainChallenge{}, errors.Join(errors.New("pending DKIM key missing"), err)
	}
	if err = tx.Commit(); err != nil {
		return admin.DomainChallenge{}, err
	}
	key := domains[0].PendingKey
	return admin.DomainChallenge{Domain: domains[0], DNSName: key.DNSName, DNSValue: key.DNSValue}, nil
}

// insertPendingKey allocates the lowest unused thinpostN selector for the
// domain (across all apps: they would share one DNS name) and stores the
// sealed key. A concurrent allocation shows up as a unique violation on the
// selector index, which the caller retries.
func insertPendingKey(ctx context.Context, tx *sql.Tx, sealer dkimkey.Sealer, appID, domain string, material dkimkey.Material) error {
	rows, err := tx.QueryContext(ctx, `SELECT selector FROM dkim_keys WHERE domain=$1`, domain)
	if err != nil {
		return err
	}
	used := map[int]bool{}
	for rows.Next() {
		var selector string
		if err = rows.Scan(&selector); err != nil {
			rows.Close()
			return err
		}
		if m := generatedSelector.FindStringSubmatch(selector); m != nil {
			if n, convErr := strconv.Atoi(m[1]); convErr == nil {
				used[n] = true
			}
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	n := 1
	for used[n] {
		n++
	}
	selector := dkimkey.SelectorPrefix + strconv.Itoa(n)
	id, err := newOpaqueID()
	if err != nil {
		return err
	}
	sealed, err := dkimkey.Seal(sealer, id, appID, domain, selector, material.PKCS8)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO dkim_keys(id,app_id,domain,selector,public_key,sealed_private_key,state) VALUES ($1,$2,$3,$4,$5,$6,'pending')`,
		id, appID, domain, selector, material.SPKI, sealed)
	return err
}

// PendingDKIMKey returns the key whose record the caller should look for in
// DNS. An empty selector means the domain's only pending key.
func (r *Repository) PendingDKIMKey(ctx context.Context, appID, domain, selector string) (admin.DKIMKeyRef, error) {
	var ref admin.DKIMKeyRef
	normalized, ok := admin.NormalizeDomain(domain)
	if !ok || normalized != domain || !opaqueID(appID) || (selector != "" && !dkimkey.ValidSelector(selector)) {
		return ref, admin.ErrInvalidInput
	}
	var exists bool
	err := r.DB.QueryRowContext(ctx, `SELECT true FROM sender_domains WHERE app_id=$1 AND domain=$2`, appID, domain).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ref, admin.ErrNotFound
	}
	if err != nil {
		return ref, err
	}
	var state string
	if selector == "" {
		err = r.DB.QueryRowContext(ctx, `SELECT selector,public_key,state FROM dkim_keys WHERE app_id=$1 AND domain=$2 AND state='pending'`, appID, domain).Scan(&ref.Selector, &ref.PublicKey, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return admin.DKIMKeyRef{}, admin.ErrConflict
		}
		return ref, err
	}
	err = r.DB.QueryRowContext(ctx, `SELECT selector,public_key,state FROM dkim_keys WHERE app_id=$1 AND domain=$2 AND selector=$3`, appID, domain, selector).Scan(&ref.Selector, &ref.PublicKey, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.DKIMKeyRef{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.DKIMKeyRef{}, err
	}
	if state != "pending" {
		return admin.DKIMKeyRef{}, admin.ErrConflict
	}
	return ref, nil
}

// ActivateDKIMKey is the step after the DNS check found key's record. It
// guards on the key still being the pending one with that public key.
//
//   - No active key: the key becomes active and the domain is verified.
//   - An active key, switchActive=false: only "the record was seen" is
//     recorded (the key is ready to switch to).
//   - An active key, switchActive=true: in one transaction the old key is
//     retired and its private key shredded, and the pending key becomes active.
func (r *Repository) ActivateDKIMKey(ctx context.Context, actor, appID, domain string, key admin.DKIMKeyRef, switchActive bool) (admin.Domain, error) {
	normalized, ok := admin.NormalizeDomain(domain)
	if !ok || normalized != domain || !opaqueID(appID) || !adminText(actor, 256) || !dkimkey.ValidSelector(key.Selector) || len(key.PublicKey) == 0 {
		return admin.Domain{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.Domain{}, err
	}
	defer tx.Rollback()
	var domainActive bool
	err = tx.QueryRowContext(ctx, `SELECT active FROM sender_domains WHERE app_id=$1 AND domain=$2 FOR UPDATE`, appID, domain).Scan(&domainActive)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.Domain{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.Domain{}, err
	}
	var pendingID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM dkim_keys WHERE app_id=$1 AND domain=$2 AND selector=$3 AND state='pending' AND public_key=$4 FOR UPDATE`,
		appID, domain, key.Selector, key.PublicKey).Scan(&pendingID)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.Domain{}, admin.ErrConflict
	}
	if err != nil {
		return admin.Domain{}, err
	}
	var activeID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM dkim_keys WHERE app_id=$1 AND domain=$2 AND state='active' FOR UPDATE`, appID, domain).Scan(&activeID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return admin.Domain{}, err
	}
	hasActive := err == nil
	switch {
	case hasActive && !switchActive:
		if _, err = tx.ExecContext(ctx, `UPDATE dkim_keys SET published_at=COALESCE(published_at,clock_timestamp()) WHERE id=$1`, pendingID); err != nil {
			return admin.Domain{}, err
		}
	default:
		if hasActive {
			policy, policyErr := deliveryRetryPolicy(ctx, tx)
			if policyErr != nil {
				return admin.Domain{}, policyErr
			}
			keep := max(minKeepRecord, policy.MaxAge+keepRecordSlack)
			// Retire first: the shredding CHECK ties the state to the missing
			// private key, and only one key may be active at a time.
			if _, err = tx.ExecContext(ctx, `UPDATE dkim_keys SET state='retired',sealed_private_key=NULL,retired_at=clock_timestamp(),keep_record_until=clock_timestamp()+$2*interval '1 millisecond' WHERE id=$1`,
				activeID, keep.Milliseconds()); err != nil {
				return admin.Domain{}, err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE dkim_keys SET state='active',activated_at=clock_timestamp(),published_at=COALESCE(published_at,clock_timestamp()) WHERE id=$1`, pendingID); err != nil {
			return admin.Domain{}, err
		}
		if !domainActive {
			if _, err = tx.ExecContext(ctx, `UPDATE sender_domains SET active=true,verified_at=COALESCE(verified_at,clock_timestamp()) WHERE app_id=$1 AND domain=$2`, appID, domain); err != nil {
				return admin.Domain{}, err
			}
			if err = recordAudit(ctx, tx, actor, appID, "domain.verified", domain); err != nil {
				return admin.Domain{}, err
			}
		}
		action := "domain.dkim_key.activated"
		if hasActive {
			action = "domain.dkim_key.switched"
		}
		if err = recordAudit(ctx, tx, actor, appID, action, domain); err != nil {
			return admin.Domain{}, err
		}
	}
	domains, err := loadDomains(ctx, tx, appID, domain)
	if err != nil || len(domains) != 1 {
		return admin.Domain{}, errors.Join(errors.New("domain missing"), err)
	}
	if err = tx.Commit(); err != nil {
		return admin.Domain{}, err
	}
	return domains[0], nil
}

// CancelDKIMKey removes a pending key. Active and retired keys are never
// deleted.
func (r *Repository) CancelDKIMKey(ctx context.Context, actor, appID, domain, selector string) error {
	normalized, ok := admin.NormalizeDomain(domain)
	if !ok || normalized != domain || !opaqueID(appID) || !adminText(actor, 256) || !dkimkey.ValidSelector(selector) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM dkim_keys WHERE app_id=$1 AND domain=$2 AND selector=$3 AND state='pending'`, appID, domain, selector)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return admin.ErrNotFound
	}
	if err = recordAudit(ctx, tx, actor, appID, "domain.dkim_key.cancelled", domain); err != nil {
		return err
	}
	return tx.Commit()
}
