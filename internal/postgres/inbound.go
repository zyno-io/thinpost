package postgres

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zyno-io/thinpost/internal/inbound"
	"github.com/zyno-io/thinpost/internal/objectkey"
)

// Inbound mail intake. Addresses and header values are stored in plaintext in
// the database (as in schemas 28 and 31) and travel only over the private
// control stream; nothing here is logged.

const inboundPendingStates = `('queued','in_flight','recovery_hold')`
const inboundFormats = `('thinpost_inbound','postmark_inbound')`

// maxInboundRoutes bounds one routing snapshot (the Edge refuses more).
const maxInboundRoutes = 100000

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// resolveInbound maps recipient domains to the one app that owns all of them.
// With lock set, the domain and app rows are share-locked until commit, so a
// concurrent disable, delete or pause waits for this transaction. A domain that
// is missing, disabled or owned by another app makes the set unroutable, as do
// a paused app and an app with no enabled inbound endpoint (the commit
// re-checks endpoints under their own lock). A routed set at its pending
// webhook-delivery limit reports Backlog.
func resolveInbound(ctx context.Context, tx *sql.Tx, domains []string, lock bool) (inbound.Resolution, error) {
	var out inbound.Resolution
	if len(domains) == 0 {
		return out, nil
	}
	query := `SELECT d.domain,d.app_id,d.state,a.active,a.inbound_pending_limit,
		EXISTS (SELECT 1 FROM webhook_subscriptions s WHERE s.app_id=d.app_id AND s.active AND s.format IN ` + inboundFormats + `),
		(SELECT count(*) FROM webhook_deliveries w WHERE w.app_id=d.app_id AND w.lane='inbound' AND w.state IN ` + inboundPendingStates + `)
		FROM inbound_domains d JOIN apps a ON a.id=d.app_id
		WHERE d.domain=ANY($1::text[]) AND d.state IN ('active','disabled') ORDER BY d.domain`
	if lock {
		query += ` FOR SHARE OF d, a`
	}
	rows, err := tx.QueryContext(ctx, query, domains)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	found := 0
	routable, backlog, multiple := true, false, false
	for rows.Next() {
		var domain, appID, state string
		var active, endpoint bool
		var limit, pending int
		if err = rows.Scan(&domain, &appID, &state, &active, &limit, &endpoint, &pending); err != nil {
			return out, err
		}
		found++
		if out.AppID == "" {
			out.AppID = appID
		} else if out.AppID != appID {
			routable, multiple = false, true
		}
		if state != "active" || !active || !endpoint {
			routable = false
		}
		if pending >= limit {
			backlog = true
		}
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if found != len(domains) {
		routable = false
	}
	switch {
	case !routable:
		// With several owners the app is not identified.
		if multiple {
			out.AppID = ""
		}
		out.Status = inbound.Unroutable
	case backlog:
		out.Status = inbound.Backlog
	default:
		out.Status = inbound.Routed
	}
	return out, nil
}

// ResolveInbound implements inbound.Repository.
func (r *Repository) ResolveInbound(ctx context.Context, domains []string) (inbound.Resolution, error) {
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return inbound.Resolution{}, err
	}
	defer tx.Rollback()
	out, err := resolveInbound(ctx, tx, domains, false)
	if err != nil {
		return inbound.Resolution{}, err
	}
	return out, tx.Commit()
}

func hexFingerprints(fingerprints [][32]byte) []string {
	out := make([]string, len(fingerprints))
	for i, f := range fingerprints {
		out[i] = hex.EncodeToString(f[:])
	}
	return out
}

// inboundTotals returns today's UTC counts: stored messages and the part of
// them that correlated to the app's outbound mail, and the app's daily limit.
func inboundTotals(ctx context.Context, tx *sql.Tx, appID string) (total, correlated, limit int, err error) {
	err = tx.QueryRowContext(ctx, `SELECT a.inbound_daily_limit,
		COALESCE((SELECT sum(count) FROM inbound_daily WHERE app_id=a.id AND day=(clock_timestamp() AT TIME ZONE 'UTC')::date AND result='accepted'),0)::integer,
		COALESCE((SELECT sum(count) FROM inbound_daily WHERE app_id=a.id AND day=(clock_timestamp() AT TIME ZONE 'UTC')::date AND result='correlated'),0)::integer
		FROM apps a WHERE a.id=$1`, appID).Scan(&limit, &total, &correlated)
	return
}

// inboundDailyAllowed applies the daily cap. A message that correlated to one of
// the app's own deliveries is exempt from the base cap up to a hard ceiling of
// twice the cap, so catch-all noise cannot crowd out real replies.
func inboundDailyAllowed(total, correlated, limit int, isCorrelated bool) bool {
	if total >= 2*limit {
		return false
	}
	return isCorrelated || total-correlated < limit
}

func countInbound(ctx context.Context, tx *sql.Tx, appID, result string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO inbound_daily(app_id,day,result,count) VALUES ($1,(clock_timestamp() AT TIME ZONE 'UTC')::date,$2,1)
		ON CONFLICT (app_id,day,result) DO UPDATE SET count=LEAST(inbound_daily.count+1,1000000000)`, appID, result)
	return err
}

// CheckInbound implements inbound.Repository.
func (r *Repository) CheckInbound(ctx context.Context, appID string, fingerprints [][32]byte, correlated bool) (inbound.Check, error) {
	if !opaqueID(appID) || len(fingerprints) == 0 || len(fingerprints) > 8 {
		return inbound.CheckNew, errors.New("invalid inbound check")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return inbound.CheckNew, err
	}
	defer tx.Rollback()
	var duplicate bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM inbound_messages WHERE app_id=$1 AND fingerprint=ANY(ARRAY(SELECT decode(x,'hex') FROM unnest($2::text[]) x)))`, appID, hexFingerprints(fingerprints)).Scan(&duplicate)
	if err != nil {
		return inbound.CheckNew, err
	}
	if duplicate {
		return inbound.CheckDuplicate, tx.Commit()
	}
	total, correlatedToday, limit, err := inboundTotals(ctx, tx, appID)
	if err != nil {
		return inbound.CheckNew, err
	}
	if !inboundDailyAllowed(total, correlatedToday, limit, correlated) {
		return inbound.CheckFull, tx.Commit()
	}
	return inbound.CheckNew, tx.Commit()
}

var inboundCounterResults = map[string]bool{"duplicate": true, "tempfail_routing": true, "tempfail_limit": true, "tempfail_internal": true}

// CountInbound implements inbound.Repository: one bounded upsert, for a result
// decided before the commit transaction.
func (r *Repository) CountInbound(ctx context.Context, appID, result string) error {
	if !opaqueID(appID) || !inboundCounterResults[result] {
		return errors.New("invalid inbound counter")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = countInbound(ctx, tx, appID, result); err != nil {
		return err
	}
	return tx.Commit()
}

// permanentDataError reports a database refusal that retrying cannot fix: a
// data exception (class 22) or a check violation, such as a value the schema
// can never store.
func permanentDataError(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && (strings.HasPrefix(pg.Code, "22") || pg.Code == "23514")
}

// ReplyCandidates implements inbound.Repository. One statement, so the shared
// flag and the delivery rows come from one snapshot. A delivery ID matches only
// a legacy message (per-delivery Message-ID); a message ID matches only a
// message prepared with the shared Message-ID. Only the app's own messages are
// searched.
func (r *Repository) ReplyCandidates(ctx context.Context, appID string, ids []string) ([]inbound.Candidate, error) {
	if !opaqueID(appID) || len(ids) == 0 || len(ids) > 20 {
		return nil, nil
	}
	for _, id := range ids {
		if !opaqueID(id) {
			return nil, nil
		}
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT k.id,m.id,d.id,COALESCE(m.sender_domain,''),false,r.address_lower,m.enhanced_protection
		FROM unnest($1::text[]) AS k(id) JOIN deliveries d ON d.id=k.id
		JOIN messages m ON m.id=d.message_id AND m.app_id=$2 AND NOT m.shared_message_id
		JOIN message_recipients r ON r.message_id=d.message_id AND r.recipient_index=d.recipient_index
		UNION ALL
		SELECT k.id,m.id,d.id,COALESCE(m.sender_domain,''),true,r.address_lower,m.enhanced_protection
		FROM unnest($1::text[]) AS k(id) JOIN messages m ON m.id=k.id AND m.app_id=$2 AND m.shared_message_id
		JOIN deliveries d ON d.message_id=m.id
		JOIN message_recipients r ON r.message_id=d.message_id AND r.recipient_index=d.recipient_index
		LIMIT 2100`, ids, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []inbound.Candidate
	for rows.Next() {
		var c inbound.Candidate
		if err = rows.Scan(&c.Key, &c.MessageID, &c.DeliveryID, &c.SenderDomain, &c.Shared, &c.Recipient, &c.EnhancedProtection); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// enqueueInboundDeliveries queues the event on the inbound lane for every
// active endpoint of an inbound format, share-locking them as the outbound
// fan-out does so a concurrent disable waits for this transaction. It returns
// how many it queued.
func enqueueInboundDeliveries(ctx context.Context, tx *sql.Tx, eventID, appID string) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT s.id FROM webhook_subscriptions s WHERE s.app_id=$1 AND s.active AND s.format IN `+inboundFormats+` ORDER BY s.id LIMIT $2 FOR SHARE OF s`, appID, maxActiveWebhooks+1)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(ids) > maxActiveWebhooks {
		return 0, errWebhookFanoutExceeded
	}
	if len(ids) == 0 {
		return 0, nil
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO webhook_deliveries(event_id,subscription_id,app_id,lane) SELECT $1,unnest($3::text[]),$2,'inbound'`, eventID, appID, ids)
	if err != nil {
		return 0, err
	}
	if n, err := result.RowsAffected(); err != nil || int(n) != len(ids) {
		return 0, errors.New("webhook fanout incomplete")
	}
	return len(ids), nil
}

func appLockKey(appID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("thinpost-inbound:" + appID))
	return int64(h.Sum64())
}

// CommitInbound implements inbound.Repository. One transaction, serialized per
// app: it re-resolves the recipients' domains, answers an exact duplicate
// without writing, enforces the pending and daily limits, verifies the object,
// and inserts the message, its recipients, one event per recipient and the
// inbound-lane delivery of each event to every enabled endpoint. Anything that
// cannot be committed leaves nothing but a counter.
func (r *Repository) CommitInbound(ctx context.Context, rec inbound.Record) (inbound.CommitStatus, error) {
	object, err := objectkey.Parse(rec.ObjectKey)
	if err != nil || object.Kind != "inbound" || object.ID != rec.ID || !opaqueID(rec.ID) || !opaqueID(rec.AppID) || len(rec.Fingerprints) == 0 || len(rec.Fingerprints) > 8 ||
		len(rec.Recipients) == 0 || len(rec.Recipients) > inbound.MaxRecipients || len(rec.Domains) == 0 || rec.Size < 1 || rec.Size > inbound.MaxBytes {
		return inbound.CommitTempfailRouting, errors.New("invalid inbound record")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return inbound.CommitTempfailRouting, err
	}
	defer tx.Rollback()
	// Verify the object before taking any app or domain lock: the S3 presence
	// check can be slow and must not hold the share lock on the app row, which
	// would serialize outbound acceptance behind it.
	if err = r.lockAndVerifyObjects(ctx, tx, []string{rec.ObjectKey}); err != nil {
		return inbound.CommitTempfailRouting, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, appLockKey(rec.AppID)); err != nil {
		return inbound.CommitTempfailRouting, err
	}
	// finish records a refusal's counter and commits it.
	finish := func(status inbound.CommitStatus, counter string) (inbound.CommitStatus, error) {
		if err := countInbound(ctx, tx, rec.AppID, counter); err != nil {
			return status, err
		}
		return status, tx.Commit()
	}
	resolution, err := resolveInbound(ctx, tx, rec.Domains, true)
	if err != nil {
		return inbound.CommitTempfailRouting, err
	}
	if resolution.AppID != rec.AppID || resolution.Status == inbound.Unroutable {
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM apps WHERE id=$1)`, rec.AppID).Scan(&exists); err != nil || !exists {
			return inbound.CommitTempfailRouting, err
		}
		return finish(inbound.CommitTempfailRouting, "tempfail_routing")
	}
	var duplicate bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM inbound_messages WHERE app_id=$1 AND fingerprint=ANY(ARRAY(SELECT decode(x,'hex') FROM unnest($2::text[]) x)))`, rec.AppID, hexFingerprints(rec.Fingerprints)).Scan(&duplicate); err != nil {
		return inbound.CommitTempfailRouting, err
	}
	if duplicate {
		return finish(inbound.CommitDuplicate, "duplicate")
	}
	if resolution.Status == inbound.Backlog {
		return finish(inbound.CommitTempfailLimit, "tempfail_limit")
	}

	// Guard the correlation in code: deliveries carry no app ID, so the schema
	// cannot tie correlated_delivery_id to the same app and message. A
	// correlation that does not hold is dropped, never trusted.
	correlation := rec.Correlation
	var enhanced bool
	if correlation != nil {
		err = tx.QueryRowContext(ctx, `SELECT m.enhanced_protection FROM messages m WHERE m.id=$1 AND m.app_id=$2`, correlation.MessageID, rec.AppID).Scan(&enhanced)
		if errors.Is(err, sql.ErrNoRows) {
			correlation = nil
		} else if err != nil {
			return inbound.CommitTempfailRouting, err
		}
	}
	deliveryID := ""
	if correlation != nil && correlation.DeliveryID != "" {
		var ok bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE d.id=$1 AND m.id=$2 AND m.app_id=$3)`, correlation.DeliveryID, correlation.MessageID, rec.AppID).Scan(&ok); err != nil {
			return inbound.CommitTempfailRouting, err
		}
		if ok {
			deliveryID = correlation.DeliveryID
		}
	}
	total, correlated, limit, err := inboundTotals(ctx, tx, rec.AppID)
	if err != nil {
		return inbound.CommitTempfailRouting, err
	}
	if !inboundDailyAllowed(total, correlated, limit, correlation != nil) {
		return finish(inbound.CommitTempfailLimit, "tempfail_limit")
	}

	var correlatedMessage, correlatedDelivery, header, fromMatches any
	if correlation != nil {
		correlatedMessage, header, fromMatches = correlation.MessageID, correlation.Header, correlation.FromMatches
		correlatedDelivery = nullString(deliveryID)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbound_messages(id,app_id,domain,object_key,size_bytes,fingerprint,content_expires_at,mail_from,header_from,subject,message_id_header,
			spf_result,dkim_result,dmarc_result,correlated_message_id,correlated_delivery_id,correlation_header,from_matches_recipient,enhanced_protection,client_ip,helo,tls_version,tls_cipher,receiver,authentication)
		VALUES ($1,$2,$3,$4,$5,$6,clock_timestamp()+interval '30 days',$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24::jsonb)`,
		rec.ID, rec.AppID, rec.Domain, rec.ObjectKey, rec.Size, rec.Fingerprints[0][:], nullString(rec.MailFrom), nullString(rec.HeaderFrom), nullString(rec.Subject), nullString(rec.MessageID),
		rec.Auth.SPF.Result, rec.Auth.DKIMSummary(), rec.Auth.DMARC.Result, correlatedMessage, correlatedDelivery, header, fromMatches, enhanced,
		clientIPValue(rec), nullString(inbound.SanitizeHelo(rec.Helo)), nullString(rec.TLSVersion), nullString(rec.TLSCipher), nullString(rec.Receiver), string(rec.Auth.JSON())); err != nil {
		if permanentDataError(err) {
			return inbound.CommitRejected, nil
		}
		return inbound.CommitTempfailRouting, err
	}
	for i, address := range rec.Recipients {
		if _, err = tx.ExecContext(ctx, `INSERT INTO inbound_recipients(inbound_id,recipient_index,app_id,address) VALUES ($1,$2,$3,$4)`, rec.ID, i, rec.AppID, address); err != nil {
			return inbound.CommitTempfailRouting, err
		}
		eventID, idErr := newOpaqueID()
		if idErr != nil {
			return inbound.CommitTempfailRouting, idErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_events(id,app_id,outcome,smtp_code,inbound_id,inbound_recipient_index) VALUES ($1,$2,'inbound_received',0,$3,$4)`, eventID, rec.AppID, rec.ID, i); err != nil {
			return inbound.CommitTempfailRouting, err
		}
		queued, enqueueErr := enqueueInboundDeliveries(ctx, tx, eventID, rec.AppID)
		if enqueueErr != nil {
			return inbound.CommitTempfailRouting, enqueueErr
		}
		if queued == 0 {
			// The last endpoint was removed or disabled since the check: nobody
			// would receive this message, so do not store it.
			_ = tx.Rollback()
			return r.countOutsideCommit(ctx, rec.AppID, "tempfail_routing")
		}
	}
	counter := "accepted"
	if err = countInbound(ctx, tx, rec.AppID, counter); err != nil {
		return inbound.CommitTempfailRouting, err
	}
	if correlation != nil {
		if err = countInbound(ctx, tx, rec.AppID, "correlated"); err != nil {
			return inbound.CommitTempfailRouting, err
		}
	}
	if err = tx.Commit(); err != nil {
		return inbound.CommitTempfailRouting, errors.New("inbound commit not confirmed")
	}
	return inbound.CommitAccepted, nil
}

func clientIPValue(rec inbound.Record) any {
	if !rec.ClientIP.IsValid() {
		return nil
	}
	return rec.ClientIP.String()
}

// countOutsideCommit records a refusal's counter in its own transaction after
// the commit transaction was rolled back, and reports routing tempfail.
func (r *Repository) countOutsideCommit(ctx context.Context, appID, result string) (inbound.CommitStatus, error) {
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return inbound.CommitTempfailRouting, nil
	}
	defer tx.Rollback()
	if err = countInbound(ctx, tx, appID, result); err == nil {
		_ = tx.Commit()
	}
	return inbound.CommitTempfailRouting, nil
}

// InboundRoutes implements inbound.Repository with a read-only snapshot that
// also works while the deploy gate is closed (Open then reports false). A
// domain accepts only when it is active and its app is active, has an enabled
// inbound endpoint and is below its pending limit; every other owned domain is
// reported as tempfail so senders queue instead of being refused.
func (r *Repository) InboundRoutes(ctx context.Context) (inbound.RouteSet, error) {
	var out inbound.RouteSet
	tx, err := r.beginOperationsRead(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var generation sql.NullString
	var mode string
	if err = tx.QueryRowContext(ctx, `SELECT generation,mode FROM runtime_gate WHERE singleton=true`).Scan(&generation, &mode); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	out.Open = err == nil && generation.Valid && generation.String == r.Generation && mode == "open"
	rows, err := tx.QueryContext(ctx, `WITH pending AS (
			SELECT app_id,count(*) AS n FROM webhook_deliveries WHERE lane='inbound' AND state IN `+inboundPendingStates+` GROUP BY app_id),
		endpoints AS (SELECT DISTINCT app_id FROM webhook_subscriptions WHERE active AND format IN `+inboundFormats+`)
		SELECT d.domain,d.app_id,(d.state='active' AND a.active AND e.app_id IS NOT NULL AND COALESCE(p.n,0) < a.inbound_pending_limit)
		FROM inbound_domains d JOIN apps a ON a.id=d.app_id
		LEFT JOIN endpoints e ON e.app_id=d.app_id LEFT JOIN pending p ON p.app_id=d.app_id
		WHERE d.state IN ('active','disabled') ORDER BY d.domain LIMIT $1`, maxInboundRoutes+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var route inbound.Route
		if err = rows.Scan(&route.Domain, &route.AppID, &route.Accept); err != nil {
			return out, err
		}
		route.Domain = strings.ToLower(route.Domain)
		out.Routes = append(out.Routes, route)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Routes) > maxInboundRoutes {
		return inbound.RouteSet{}, errors.New("too many inbound routes")
	}
	return out, tx.Commit()
}

var _ inbound.Repository = (*Repository)(nil)
