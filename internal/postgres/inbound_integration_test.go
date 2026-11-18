package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook"
)

// TestMigration039 applies migrations through 38 over existing webhook rows,
// then migrates: every old row satisfies the replaced source CHECK and the
// existing deliveries are on the event lane.
func TestMigration039(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "migration39_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if migrations[38].version != 39 {
		t.Fatal("this test targets migration 39")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:38] {
		body, checksum, err := migrationBody(item)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, item.version, checksum); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('app','test');
		INSERT INTO messages(id,app_id,object_key,enhanced_protection) VALUES ('m1','app','k1',true);
		INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ('d1','m1',0,'f1');
		INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch) VALUES ('a1','d1','w',1);
		INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ('e1','app','m1','d1','a1','accepted',250);
		INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ('e2','app','m1','preparation_failed',0);
		INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext) VALUES ('s1','app','https://hooks.example.com/','x');
		INSERT INTO webhook_deliveries(event_id,subscription_id,app_id) VALUES ('e1','s1','app')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var lane string
	var daily, pending int
	if err = db.QueryRowContext(ctx, `SELECT lane FROM webhook_deliveries WHERE event_id='e1'`).Scan(&lane); err != nil || lane != "event" {
		t.Fatal("existing delivery is not on the event lane", lane, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT inbound_daily_limit,inbound_pending_limit FROM apps WHERE id='app'`).Scan(&daily, &pending); err != nil || daily != 2000 || pending != 500 {
		t.Fatal("existing app does not have the default inbound limits", daily, pending, err)
	}
	var events int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_events WHERE inbound_id IS NULL AND message_id IS NOT NULL`).Scan(&events); err != nil || events != 2 {
		t.Fatal("existing events changed", events, err)
	}
}

// The schema's constraints hold under the owner role.
func TestInboundSchemaConstraints(t *testing.T) {
	f := newWebhookFixture(t, "inb_schema_")
	app, other := f.app("one"), f.app("two")
	bad := func(what, query string, args ...any) {
		t.Helper()
		if _, err := f.owner.ExecContext(f.ctx, query, args...); err == nil {
			t.Fatal("accepted:", what)
		}
	}
	if f.count(`SELECT count(*) FROM apps WHERE id=$1 AND inbound_daily_limit=2000 AND inbound_pending_limit=500`, app) != 1 {
		t.Fatal("default limits are not 2000 and 500")
	}
	for _, query := range []string{
		`UPDATE apps SET inbound_daily_limit=0`, `UPDATE apps SET inbound_daily_limit=1000001`,
		`UPDATE apps SET inbound_pending_limit=0`, `UPDATE apps SET inbound_pending_limit=100001`,
	} {
		bad(query, query)
	}
	f.exec(`UPDATE apps SET inbound_daily_limit=1000000,inbound_pending_limit=100000 WHERE id=$1`, app)
	f.exec(`UPDATE apps SET inbound_daily_limit=2000,inbound_pending_limit=500 WHERE id=$1`, app)

	hash := `decode(repeat('00',32),'hex')`
	domain := func(app, name string) string {
		id := f.id()
		f.exec(`INSERT INTO inbound_domains(id,app_id,domain,verification_hash) VALUES ($1,$2,$3,`+hash+`)`, id, app, name)
		return id
	}
	// Pending claims coexist; the owner index admits one active or disabled owner.
	first, second := domain(app, "mail.example.com"), domain(other, "mail.example.com")
	bad("a duplicate claim by one app", `INSERT INTO inbound_domains(id,app_id,domain,verification_hash) VALUES ($1,$2,'mail.example.com',`+hash+`)`, f.id(), app)
	bad("an uppercase domain", `INSERT INTO inbound_domains(id,app_id,domain,verification_hash) VALUES ($1,$2,'Mail.Example.com',`+hash+`)`, f.id(), app)
	bad("a pending domain without a challenge", `INSERT INTO inbound_domains(id,app_id,domain) VALUES ($1,$2,'nochallenge.example.com')`, f.id(), app)
	bad("an active domain without a verification time", `UPDATE inbound_domains SET state='active' WHERE id=$1`, first)
	bad("a verification time on a pending domain", `UPDATE inbound_domains SET verified_at=now() WHERE id=$1`, first)
	bad("an MX status without a check time", `UPDATE inbound_domains SET mx_status='thinpost' WHERE id=$1`, first)
	bad("an unknown MX status", `UPDATE inbound_domains SET mx_status='bogus',mx_checked_at=now() WHERE id=$1`, first)
	f.exec(`UPDATE inbound_domains SET state='active',verified_at=now(),mx_status='mixed',mx_checked_at=now() WHERE id=$1`, first)
	bad("a second owner", `UPDATE inbound_domains SET state='active',verified_at=now() WHERE id=$1`, second)
	bad("a second disabled owner", `UPDATE inbound_domains SET state='disabled',verified_at=now() WHERE id=$1`, second)
	f.exec(`UPDATE inbound_domains SET state='disabled' WHERE id=$1`, first)
	bad("a second owner behind a disabled one", `UPDATE inbound_domains SET state='active',verified_at=now() WHERE id=$1`, second)

	// An inbound message, its recipient and its webhook event.
	message := f.id()
	f.exec(`INSERT INTO inbound_messages(id,app_id,domain,object_key,size_bytes,fingerprint,content_expires_at) VALUES ($1,$2,'mail.example.com','inbound/v1/0000000000/'||$1,10,`+hash+`,now()+interval '30 days')`, message, app)
	bad("a duplicate fingerprint for one app", `INSERT INTO inbound_messages(id,app_id,domain,object_key,size_bytes,fingerprint,content_expires_at) VALUES ($1,$2,'mail.example.com','other',10,`+hash+`,now())`, f.id(), app)
	f.exec(`INSERT INTO inbound_messages(id,app_id,domain,object_key,size_bytes,fingerprint,content_expires_at) VALUES ($1,$2,'mail.example.com','other',10,`+hash+`,now())`, f.id(), other)
	bad("an unknown authentication result", `UPDATE inbound_messages SET dmarc_result='bogus' WHERE id=$1`, message)
	bad("a correlation header without a message", `UPDATE inbound_messages SET correlation_header='in_reply_to' WHERE id=$1`, message)
	bad("a recipient under the wrong app", `INSERT INTO inbound_recipients(inbound_id,recipient_index,app_id,address) VALUES ($1,0,$2,'a@example.com')`, message, other)
	f.exec(`INSERT INTO inbound_recipients(inbound_id,recipient_index,app_id,address) VALUES ($1,0,$2,'a@mail.example.com')`, message, app)
	event := func(id, app, outcome string, index any, smtp int) error {
		_, err := f.owner.ExecContext(f.ctx, `INSERT INTO webhook_events(id,app_id,inbound_id,inbound_recipient_index,outcome,smtp_code) VALUES ($1,$2,$3,$4,$5,$6)`, id, app, message, index, outcome, smtp)
		return err
	}
	if err := event(f.id(), app, "inbound_received", 0, 0); err != nil {
		t.Fatal("inbound event refused", err)
	}
	for what, err := range map[string]error{
		"a second event for one recipient":   event(f.id(), app, "inbound_received", 0, 0),
		"an event under another app":         event(f.id(), other, "inbound_received", 0, 0),
		"an event for a missing recipient":   event(f.id(), app, "inbound_received", 1, 0),
		"an inbound event without an index":  event(f.id(), app, "inbound_received", nil, 0),
		"an inbound event with an SMTP code": event(f.id(), app, "inbound_received", 0, 250),
		"an inbound event with another name": event(f.id(), app, "accepted", 0, 0),
	} {
		if err == nil {
			t.Fatal("accepted:", what)
		}
	}
	// The outbound sources still need their message.
	outbound := f.event(app)
	bad("an outbound event without a message", `UPDATE webhook_events SET message_id=NULL WHERE id=$1`, outbound)
	bad("a message on an inbound event", `INSERT INTO webhook_events(id,app_id,message_id,inbound_id,inbound_recipient_index,outcome,smtp_code) VALUES ($1,$2,(SELECT message_id FROM webhook_events WHERE id=$3),$4,0,'inbound_received',0)`, f.id(), app, outbound, message)
	bad("a preparation_failed event with the inbound outcome", `UPDATE webhook_events SET outcome='inbound_received' WHERE id=$1`, outbound)

	// Lane.
	hook := f.create(app, "https://hooks.example.com/")
	f.exec(`INSERT INTO webhook_deliveries(event_id,subscription_id,app_id) VALUES ($1,$2,$3)`, outbound, hook.ID, app)
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE event_id=$1 AND lane='event'`, outbound) != 1 {
		t.Fatal("a delivery inserted without a lane is not on the event lane")
	}
	bad("an unknown lane", `UPDATE webhook_deliveries SET lane='bogus' WHERE event_id=$1`, outbound)

	// Daily counters.
	f.exec(`INSERT INTO inbound_daily(app_id,day,result,count) VALUES ($1,current_date,'accepted',1)`, app)
	bad("an unknown counter result", `INSERT INTO inbound_daily(app_id,day,result,count) VALUES ($1,current_date,'bogus',1)`, app)
	bad("a duplicate counter", `INSERT INTO inbound_daily(app_id,day,result,count) VALUES ($1,current_date,'accepted',1)`, app)
}

// The runtime role has exactly the reviewed inbound privileges.
func TestInboundRuntimeGrants(t *testing.T) {
	f := newWebhookFixture(t, "inb_grants_")
	app := f.app("grants")
	db := f.repo.DB
	run := func(query string, args ...any) error {
		_, err := db.ExecContext(f.ctx, query, args...)
		return err
	}
	hash := `decode(repeat('00',32),'hex')`
	id := f.id()
	for _, query := range []string{
		`INSERT INTO inbound_domains(id,app_id,domain,verification_hash) VALUES ('` + id + `','` + app + `','a.example.com',` + hash + `)`,
		`UPDATE inbound_domains SET mx_status='none',mx_checked_at=now()`,
		`SELECT count(*) FROM inbound_domains`,
		`INSERT INTO inbound_messages(id,app_id,domain,object_key,size_bytes,fingerprint,content_expires_at) VALUES ('` + id + `','` + app + `','a.example.com','inbound/v1/0000000000/` + id + `',1,` + hash + `,now())`,
		`SELECT object_key FROM inbound_messages`,
		`INSERT INTO inbound_recipients(inbound_id,recipient_index,app_id,address) VALUES ('` + id + `',0,'` + app + `','x@a.example.com')`,
		`SELECT address FROM inbound_recipients`,
		`INSERT INTO webhook_events(id,app_id,inbound_id,inbound_recipient_index,outcome,smtp_code) VALUES ('` + f.id() + `','` + app + `','` + id + `',0,'inbound_received',0)`,
		`INSERT INTO inbound_daily(app_id,day,result,count) VALUES ('` + app + `',current_date,'accepted',1)`,
		`UPDATE inbound_daily SET count=count+1`,
		`SELECT count FROM inbound_daily`,
		`UPDATE apps SET inbound_daily_limit=10 WHERE id='` + app + `'`,
		`DELETE FROM inbound_domains`,
	} {
		if err := run(query); err != nil {
			t.Fatal("runtime role lacks a reviewed privilege:", query, err)
		}
	}
	for _, query := range []string{
		`UPDATE inbound_messages SET size_bytes=2`,
		`DELETE FROM inbound_messages`,
		`UPDATE inbound_recipients SET address='y@a.example.com'`,
		`DELETE FROM inbound_recipients`,
		`DELETE FROM inbound_daily`,
		`TRUNCATE inbound_domains`,
	} {
		if err := run(query); err == nil {
			t.Fatal("runtime role has an unreviewed privilege:", query)
		}
	}
}

// Outbound webhook claims see only the event lane, and fan-out puts outbound
// deliveries on it.
func TestOutboundWebhookClaimIgnoresInboundLane(t *testing.T) {
	f := newWebhookFixture(t, "inb_lane_")
	app := f.app("lane")
	f.create(app, "https://lane.example.com/")
	event := f.acceptedEvent(app)
	f.fanout(event, app)
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE event_id=$1 AND lane='event'`, event) != 1 {
		t.Fatal("outbound fan-out is not on the event lane")
	}
	f.exec(`UPDATE webhook_deliveries SET lane='inbound' WHERE event_id=$1`, event)
	if _, err := f.repo.ClaimWebhook(f.ctx, f.id(), time.Minute); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal("the event-lane claim took an inbound-lane delivery", err)
	}
	f.exec(`UPDATE webhook_deliveries SET lane='event' WHERE event_id=$1`, event)
	claim, err := f.repo.ClaimWebhook(f.ctx, f.id(), time.Minute)
	if err != nil || claim.Event.ID != event {
		t.Fatal("outbound claim changed", claim.Event.ID, err)
	}
}

func inboundDigest(value string) [32]byte { return sha256.Sum256([]byte(value)) }

func TestInboundDomainLifecyclePostgres(t *testing.T) {
	f := newWebhookFixture(t, "inb_domains_")
	app, other := f.app("one"), f.app("two")
	repo := f.repo
	audits := func(action string) int {
		return f.count(`SELECT count(*) FROM audit_events WHERE action=$1`, action)
	}
	if _, err := repo.RequestInboundDomain(f.ctx, "operator", app, "Bad.Example.com"); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("accepted a non-normalized domain", err)
	}
	if _, err := repo.RequestInboundDomain(f.ctx, "operator", f.id(), "mail.example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("request for a missing app", err)
	}
	first, err := repo.RequestInboundDomain(f.ctx, "operator", app, "mail.example.com")
	if err != nil || first.DNSName != "_thinpost.mail.example.com" || !strings.HasPrefix(first.DNSValue, "thinpost-inbound-verification=") || first.Domain.State != "pending" || first.Domain.VerifiedAt != nil || first.Domain.MXStatus != nil {
		t.Fatal("request", first, err)
	}
	second, err := repo.RequestInboundDomain(f.ctx, "operator", app, "mail.example.com")
	if err != nil || second.DNSValue == first.DNSValue {
		t.Fatal("asking again did not replace the challenge", err)
	}
	domain, digest, err := repo.InboundDomainChallenge(f.ctx, app, "mail.example.com")
	if err != nil || domain.State != "pending" || digest != inboundDigest(second.DNSValue) {
		t.Fatal("challenge", domain, err)
	}
	if _, err = repo.VerifyInboundDomain(f.ctx, "operator", app, "mail.example.com", inboundDigest(first.DNSValue)); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("a replaced challenge verified", err)
	}
	if _, _, err = repo.InboundDomainChallenge(f.ctx, other, "mail.example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("another app saw the claim", err)
	}
	if audits("inbound_domain.requested") != 2 {
		t.Fatal("requests are not audited")
	}

	// Two apps racing to verify one domain: exactly one wins.
	otherClaim, err := repo.RequestInboundDomain(f.ctx, "operator", other, "mail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		app string
		err error
	}
	results := make(chan outcome, 2)
	var start sync.WaitGroup
	start.Add(1)
	for _, claim := range []struct{ app, value string }{{app, second.DNSValue}, {other, otherClaim.DNSValue}} {
		go func() {
			start.Wait()
			_, verifyErr := repo.VerifyInboundDomain(f.ctx, "operator", claim.app, "mail.example.com", inboundDigest(claim.value))
			results <- outcome{claim.app, verifyErr}
		}()
	}
	start.Done()
	a, b := <-results, <-results
	var winner, loser outcome
	switch {
	case a.err == nil && errors.Is(b.err, admin.ErrConflict):
		winner, loser = a, b
	case b.err == nil && errors.Is(a.err, admin.ErrConflict):
		winner, loser = b, a
	default:
		t.Fatal("expected one winner and one conflict", a.err, b.err)
	}
	if f.count(`SELECT count(*) FROM inbound_domains WHERE domain='mail.example.com' AND state='active'`) != 1 ||
		f.count(`SELECT count(*) FROM inbound_domains WHERE domain='mail.example.com' AND state='pending' AND app_id=$1`, loser.app) != 1 {
		t.Fatal("the loser's claim changed")
	}
	if audits("inbound_domain.verified") != 1 {
		t.Fatal("verification is not audited exactly once")
	}
	loserValue := otherClaim.DNSValue
	if loser.app == app {
		loserValue = second.DNSValue
	}
	if _, err = repo.VerifyInboundDomain(f.ctx, "operator", loser.app, "mail.example.com", inboundDigest(loserValue)); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("the loser verified later", err)
	}
	winnerDomain, digest, err := repo.InboundDomainChallenge(f.ctx, winner.app, "mail.example.com")
	if err != nil || winnerDomain.State != "active" || winnerDomain.VerifiedAt == nil || digest != ([32]byte{}) {
		t.Fatal("verified domain", winnerDomain, err)
	}
	if _, err = repo.RequestInboundDomain(f.ctx, "operator", winner.app, "mail.example.com"); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("an owned domain was re-requested", err)
	}

	// A disabled domain keeps its owner; deleting it frees the name.
	if got, err := repo.SetInboundDomainEnabled(f.ctx, "operator", winner.app, "mail.example.com", true); err != nil || got.State != "active" || audits("inbound_domain.enabled") != 0 {
		t.Fatal("enabling an active domain is not a no-op", got, err)
	}
	if got, err := repo.SetInboundDomainEnabled(f.ctx, "operator", winner.app, "mail.example.com", false); err != nil || got.State != "disabled" || got.VerifiedAt == nil {
		t.Fatal("disable", got, err)
	}
	if _, err = repo.SetInboundDomainEnabled(f.ctx, "operator", winner.app, "mail.example.com", false); err != nil || audits("inbound_domain.disabled") != 1 {
		t.Fatal("disabling twice audited twice", err)
	}
	if _, err = repo.VerifyInboundDomain(f.ctx, "operator", loser.app, "mail.example.com", inboundDigest(loserValue)); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("a disabled domain's owner was displaced", err)
	}
	if _, err = repo.SetInboundDomainEnabled(f.ctx, "operator", loser.app, "mail.example.com", true); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("a pending claim was enabled", err)
	}
	if _, err = repo.SetInboundDomainEnabled(f.ctx, "operator", winner.app, "nothere.example.com", true); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("enable of a missing domain", err)
	}
	if got, err := repo.SetInboundDomainEnabled(f.ctx, "operator", winner.app, "mail.example.com", true); err != nil || got.State != "active" || audits("inbound_domain.enabled") != 1 {
		t.Fatal("enable", got, err)
	}

	// MX status.
	for _, status := range []string{admin.MXThinpost, admin.MXMixed, admin.MXOther, admin.MXNone, admin.MXNull, admin.MXError} {
		got, err := repo.RecordInboundMX(f.ctx, "operator", winner.app, "mail.example.com", status)
		if err != nil || got.MXStatus == nil || *got.MXStatus != status || got.MXCheckedAt == nil || time.Since(*got.MXCheckedAt) > time.Minute {
			t.Fatal("record MX", status, got, err)
		}
	}
	if audits("inbound_domain.mx_checked") != 6 {
		t.Fatal("MX checks are not audited")
	}
	if _, err = repo.RecordInboundMX(f.ctx, "operator", winner.app, "mail.example.com", "bogus"); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("unknown MX status", err)
	}
	if _, err = repo.RecordInboundMX(f.ctx, "operator", loser.app, "other.example.com", admin.MXNone); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("MX status for a missing domain", err)
	}

	// Listing is per app.
	list, err := repo.ListInboundDomains(f.ctx, winner.app)
	if err != nil || len(list) != 1 || list[0].State != "active" || list[0].MXStatus == nil {
		t.Fatal("list", list, err)
	}
	if list, err = repo.ListInboundDomains(f.ctx, loser.app); err != nil || len(list) != 1 || list[0].State != "pending" {
		t.Fatal("list", list, err)
	}
	if _, err = repo.ListInboundDomains(f.ctx, f.id()); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("list for a missing app", err)
	}

	// Deleting the owner frees the domain for the other claim.
	if err = repo.DeleteInboundDomain(f.ctx, "operator", loser.app, "mail.example.com"); err != nil {
		t.Fatal("delete a pending claim", err)
	}
	if err = repo.DeleteInboundDomain(f.ctx, "operator", loser.app, "mail.example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("delete twice", err)
	}
	if err = repo.DeleteInboundDomain(f.ctx, "operator", winner.app, "mail.example.com"); err != nil {
		t.Fatal("delete the owner", err)
	}
	if audits("inbound_domain.deleted") != 2 {
		t.Fatal("deletes are not audited")
	}
	again, err := repo.RequestInboundDomain(f.ctx, "operator", loser.app, "mail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := repo.VerifyInboundDomain(f.ctx, "operator", loser.app, "mail.example.com", inboundDigest(again.DNSValue)); err != nil || got.State != "active" {
		t.Fatal("the freed domain did not verify", got, err)
	}
	// A paused app cannot start a claim.
	paused := f.app("paused")
	f.exec(`UPDATE apps SET active=false WHERE id=$1`, paused)
	if _, err = repo.RequestInboundDomain(f.ctx, "operator", paused, "paused.example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("a paused app requested a domain", err)
	}
}

func TestInboundLimitsPostgres(t *testing.T) {
	f := newWebhookFixture(t, "inb_limits_")
	app, other := f.app("one"), f.app("two")
	got, err := f.repo.GetInboundLimits(f.ctx, app)
	if err != nil || got != (admin.InboundLimits{DailyLimit: 2000, PendingLimit: 500}) {
		t.Fatal("defaults", got, err)
	}
	saved, err := f.repo.SetInboundLimits(f.ctx, "operator", app, admin.InboundLimits{DailyLimit: 10, PendingLimit: 5})
	if err != nil || saved != (admin.InboundLimits{DailyLimit: 10, PendingLimit: 5}) {
		t.Fatal("set", saved, err)
	}
	if got, err = f.repo.GetInboundLimits(f.ctx, other); err != nil || got.DailyLimit != 2000 {
		t.Fatal("another app changed", got, err)
	}
	var details string
	if err = f.owner.QueryRowContext(f.ctx, `SELECT details::text FROM audit_events WHERE action='app.inbound_limits.updated' AND app_id=$1`, app).Scan(&details); err != nil {
		t.Fatal("limits update is not audited", err)
	}
	for _, want := range []string{`"daily_limit": 10`, `"pending_limit": 5`, `"previous_daily_limit": 2000`, `"previous_pending_limit": 500`} {
		if !strings.Contains(details, want) {
			t.Fatal("audit details lack", want, details)
		}
	}
	for _, bad := range []admin.InboundLimits{{DailyLimit: 0, PendingLimit: 5}, {DailyLimit: 5, PendingLimit: 0}, {DailyLimit: 1000001, PendingLimit: 5}, {DailyLimit: 5, PendingLimit: 100001}, {DailyLimit: -1, PendingLimit: 5}} {
		if _, err = f.repo.SetInboundLimits(f.ctx, "operator", app, bad); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatal("accepted", bad, err)
		}
	}
	if saved, err = f.repo.SetInboundLimits(f.ctx, "operator", app, admin.InboundLimits{DailyLimit: 1000000, PendingLimit: 100000}); err != nil || saved.DailyLimit != 1000000 {
		t.Fatal("upper bounds", saved, err)
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE action='app.inbound_limits.updated'`) != 2 {
		t.Fatal("rejected updates were audited")
	}
	if _, err = f.repo.SetInboundLimits(f.ctx, "operator", f.id(), admin.InboundLimits{DailyLimit: 1, PendingLimit: 1}); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("limits for a missing app", err)
	}
	if _, err = f.repo.GetInboundLimits(f.ctx, f.id()); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("limits for a missing app", err)
	}
}
