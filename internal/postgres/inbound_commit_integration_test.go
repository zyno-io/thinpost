package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/inbound"
	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/webhook"
)

type inboundFixture struct {
	*webhookFixture
	app, other string
}

// newInboundFixture migrates a schema and gives app "one" an active receiving
// domain and an enabled thinpost_inbound endpoint (inserted as owner with a
// placeholder secret; the dispatcher tests do not open it).
func newInboundFixture(t *testing.T) *inboundFixture {
	t.Helper()
	f := newWebhookFixture(t, "inbound_commit_")
	f.repo.Objects = objectVerifierFunc(func(context.Context, string) error { return nil })
	i := &inboundFixture{webhookFixture: f, app: f.app("one"), other: f.app("two")}
	i.domain(i.app, "mail.example.com", "active")
	i.endpoint(i.app)
	return i
}

func (f *inboundFixture) domain(app, domain, state string) {
	f.t.Helper()
	f.exec(`INSERT INTO inbound_domains(id,app_id,domain,state,verified_at) VALUES ($1,$2,$3,$4,clock_timestamp())`, f.id(), app, domain, state)
}

func (f *inboundFixture) endpoint(app string) string {
	f.t.Helper()
	id := f.id()
	f.exec(`INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,format,secret_ciphertext) VALUES ($1,$2,'https://hooks.example.com/in','thinpost_inbound','\x73')`, id, app)
	return id
}

// record builds a committable record for the app: a fresh object, one
// recipient at mail.example.com and a fresh fingerprint.
func (f *inboundFixture) record(app string, domains ...string) inbound.Record {
	f.t.Helper()
	if len(domains) == 0 {
		domains = []string{"mail.example.com"}
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		f.t.Fatal(err)
	}
	id := hex.EncodeToString(raw[:])
	key, err := objectkey.New("inbound", id, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	var fp [32]byte
	if _, err = rand.Read(fp[:]); err != nil {
		f.t.Fatal(err)
	}
	rec := inbound.Record{ID: id, ObjectKey: key, AppID: app, Domains: domains, Domain: domains[0], Fingerprints: [][32]byte{fp}, Size: 100,
		MailFrom: "alice@example.org", HeaderFrom: "alice@example.org", Subject: "Re: hello", MessageID: "<m@example.org>",
		Auth:     inbound.Authentication{SPF: inbound.SPFResult{Result: "pass", Domain: "example.org", Identity: "mailfrom"}, DKIM: []inbound.DKIMResult{{Result: "fail", Domain: "example.org"}}, DMARC: inbound.DMARCResult{Result: "fail", Domain: "example.org", Policy: "reject"}},
		ClientIP: netip.MustParseAddr("192.0.2.7"), Helo: "mx.example.org", TLSVersion: "TLS 1.3", TLSCipher: "TLS_AES_128_GCM_SHA256", Receiver: "smtp.mx.example"}
	for _, d := range domains {
		rec.Recipients = append(rec.Recipients, "reply@"+d)
	}
	return rec
}

func (f *inboundFixture) commit(rec inbound.Record) inbound.CommitStatus {
	f.t.Helper()
	status, err := f.repo.CommitInbound(f.ctx, rec)
	if err != nil {
		f.t.Fatal(err)
	}
	return status
}

func (f *inboundFixture) counter(app, result string) int {
	return f.count(`SELECT COALESCE(sum(count),0)::integer FROM inbound_daily WHERE app_id=$1 AND result=$2`, app, result)
}

func (f *inboundFixture) messages(app string) int {
	return f.count(`SELECT count(*) FROM inbound_messages WHERE app_id=$1`, app)
}

func TestCommitInboundWritesMessageEventsAndInboundLaneDeliveries(t *testing.T) {
	f := newInboundFixture(t)
	second := f.endpoint(f.app)
	message, deliveries := f.feedbackSharedMessage(f.app, "alice@example.org", "bob@example.org")
	f.exec(`UPDATE messages SET enhanced_protection=true WHERE id=$1`, message)
	rec := f.record(f.app)
	rec.Recipients = []string{"Reply+Ticket@mail.example.com", "other@mail.example.com"}
	rec.Helo = "mx.example.org"
	rec.Correlation = &inbound.Correlation{MessageID: message, DeliveryID: deliveries[0], Header: "in_reply_to", FromMatches: true}
	if got := f.commit(rec); got != inbound.CommitAccepted {
		t.Fatalf("status %v", got)
	}
	var domain, objectKey, mailFrom, fromHeader, subject, spf, dkim, dmarc, header, helo, receiver, tlsVersion, correlatedMessage, correlatedDelivery, ip string
	var size int
	var matches, ep bool
	var ttlSeconds int64
	err := f.owner.QueryRowContext(f.ctx, `SELECT domain,object_key,size_bytes,mail_from,header_from,subject,spf_result,dkim_result,dmarc_result,correlation_header,from_matches_recipient,enhanced_protection,helo,receiver,tls_version,correlated_message_id,correlated_delivery_id,host(client_ip),extract(epoch from content_expires_at-received_at)::bigint
		FROM inbound_messages WHERE id=$1`, rec.ID).Scan(&domain, &objectKey, &size, &mailFrom, &fromHeader, &subject, &spf, &dkim, &dmarc, &header, &matches, &ep, &helo, &receiver, &tlsVersion, &correlatedMessage, &correlatedDelivery, &ip, &ttlSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if domain != "mail.example.com" || objectKey != rec.ObjectKey || size != 100 || mailFrom != "alice@example.org" || fromHeader != "alice@example.org" || subject != "Re: hello" || spf != "pass" || dkim != "fail" || dmarc != "fail" ||
		header != "in_reply_to" || !matches || !ep || helo != "mx.example.org" || receiver != "smtp.mx.example" || tlsVersion != "TLS 1.3" || correlatedMessage != message || correlatedDelivery != deliveries[0] || ip != "192.0.2.7" {
		t.Fatal("stored columns", domain, objectKey, size, mailFrom, subject, spf, dkim, dmarc, header, matches, ep, helo, correlatedMessage, correlatedDelivery, ip)
	}
	if ttl := time.Duration(ttlSeconds) * time.Second; ttl < 29*24*time.Hour || ttl > 31*24*time.Hour {
		t.Fatalf("content retention %v", ttl)
	}
	if f.count(`SELECT count(*) FROM inbound_messages WHERE id=$1 AND authentication->'spf'->>'result'='pass' AND jsonb_array_length(authentication->'dkim')=1 AND authentication->'arc'->>'evaluated'='false'`, rec.ID) != 1 {
		t.Fatal("authentication document")
	}
	if f.count(`SELECT count(*) FROM inbound_recipients WHERE inbound_id=$1`, rec.ID) != 2 {
		t.Fatal("recipients")
	}
	// One event per recipient, each queued on the inbound lane for both endpoints.
	if f.count(`SELECT count(*) FROM webhook_events WHERE inbound_id=$1 AND outcome='inbound_received' AND message_id IS NULL AND app_id=$2`, rec.ID, f.app) != 2 {
		t.Fatal("events")
	}
	if f.count(`SELECT count(*) FROM webhook_deliveries d JOIN webhook_events e ON e.id=d.event_id WHERE e.inbound_id=$1 AND d.lane='inbound' AND d.state='queued'`, rec.ID) != 4 {
		t.Fatal("deliveries")
	}
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE lane='event'`) != 0 || second == "" {
		t.Fatal("an inbound event reached the outbound lane")
	}
	if f.counter(f.app, "accepted") != 1 || f.counter(f.app, "correlated") != 1 {
		t.Fatal("counters")
	}
	// The outbound claim never sees inbound rows.
	if _, err := f.repo.ClaimWebhook(f.ctx, f.id(), time.Minute); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatalf("the outbound claim took an inbound delivery: %v", err)
	}
}

func TestCommitInboundDeduplicates(t *testing.T) {
	f := newInboundFixture(t)
	rec := f.record(f.app)
	if f.commit(rec) != inbound.CommitAccepted {
		t.Fatal("first commit")
	}
	check, err := f.repo.CheckInbound(f.ctx, f.app, rec.Fingerprints, false)
	if err != nil || check != inbound.CheckDuplicate {
		t.Fatalf("check %v %v", check, err)
	}
	// The same transaction retried: a new object and ID, the same fingerprint.
	retry := f.record(f.app)
	retry.Fingerprints = rec.Fingerprints
	if got := f.commit(retry); got != inbound.CommitDuplicate {
		t.Fatalf("retry status %v", got)
	}
	// An old key version still finds it.
	other := [32]byte{9}
	old := f.record(f.app)
	old.Fingerprints = [][32]byte{other, rec.Fingerprints[0]}
	if got := f.commit(old); got != inbound.CommitDuplicate {
		t.Fatalf("rotated-key retry status %v", got)
	}
	if f.messages(f.app) != 1 || f.count(`SELECT count(*) FROM webhook_events WHERE outcome='inbound_received'`) != 1 || f.counter(f.app, "duplicate") != 2 {
		t.Fatal("a duplicate created a second message or event")
	}
	// The same bytes for another app are not a duplicate.
	f.domain(f.other, "other.example.com", "active")
	f.endpoint(f.other)
	foreign := f.record(f.other, "other.example.com")
	foreign.Fingerprints = rec.Fingerprints
	if got := f.commit(foreign); got != inbound.CommitAccepted {
		t.Fatalf("another app's identical message: %v", got)
	}
}

func TestCommitInboundConcurrentDuplicatesCommitOnce(t *testing.T) {
	f := newInboundFixture(t)
	base := f.record(f.app)
	var wg sync.WaitGroup
	statuses := make(chan inbound.CommitStatus, 8)
	for i := 0; i < 8; i++ {
		rec := f.record(f.app)
		rec.Fingerprints = base.Fingerprints
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, err := f.repo.CommitInbound(f.ctx, rec)
			if err != nil {
				t.Error(err)
				return
			}
			statuses <- status
		}()
	}
	wg.Wait()
	close(statuses)
	accepted, duplicates := 0, 0
	for s := range statuses {
		switch s {
		case inbound.CommitAccepted:
			accepted++
		case inbound.CommitDuplicate:
			duplicates++
		}
	}
	if accepted != 1 || duplicates != 7 || f.messages(f.app) != 1 || f.count(`SELECT count(*) FROM webhook_events WHERE outcome='inbound_received'`) != 1 {
		t.Fatalf("accepted %d duplicates %d messages %d", accepted, duplicates, f.messages(f.app))
	}
}

func TestCommitInboundReResolvesRouting(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(f *inboundFixture)
		rec    func(f *inboundFixture) inbound.Record
	}{
		"domain disabled": {func(f *inboundFixture) {
			f.exec(`UPDATE inbound_domains SET state='disabled' WHERE domain='mail.example.com'`)
		}, func(f *inboundFixture) inbound.Record { return f.record(f.app) }},
		"domain deleted": {func(f *inboundFixture) { f.exec(`DELETE FROM inbound_domains WHERE domain='mail.example.com'`) }, func(f *inboundFixture) inbound.Record { return f.record(f.app) }},
		"app paused":     {func(f *inboundFixture) { f.exec(`UPDATE apps SET active=false WHERE id=$1`, f.app) }, func(f *inboundFixture) inbound.Record { return f.record(f.app) }},
		"endpoint disabled": {func(f *inboundFixture) {
			f.exec(`UPDATE webhook_subscriptions SET active=false WHERE app_id=$1`, f.app)
		}, func(f *inboundFixture) inbound.Record { return f.record(f.app) }},
		"domains span apps": {func(*inboundFixture) {}, func(f *inboundFixture) inbound.Record {
			return f.record(f.app, "mail.example.com", "other.example.com")
		}},
		"unknown domain": {func(*inboundFixture) {}, func(f *inboundFixture) inbound.Record {
			return f.record(f.app, "mail.example.com", "unknown.example.com")
		}},
		"domain moved to another app": {func(*inboundFixture) {}, func(f *inboundFixture) inbound.Record {
			return f.record(f.app, "other.example.com")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newInboundFixture(t)
			f.domain(f.other, "other.example.com", "active")
			f.endpoint(f.other)
			tc.change(f)
			if got := f.commit(tc.rec(f)); got != inbound.CommitTempfailRouting {
				t.Fatalf("status %v", got)
			}
			if f.messages(f.app) != 0 || f.count(`SELECT count(*) FROM webhook_events WHERE outcome='inbound_received'`) != 0 || f.count(`SELECT count(*) FROM webhook_deliveries`) != 0 {
				t.Fatal("a routing change still stored the message")
			}
			if got := f.counter(f.app, "tempfail_routing"); got != 1 {
				t.Fatalf("routing counter %d", got)
			}
		})
	}
}

func TestResolveInbound(t *testing.T) {
	f := newInboundFixture(t)
	f.domain(f.app, "second.example.com", "active")
	f.domain(f.other, "other.example.com", "active")
	f.domain(f.other, "off.example.com", "disabled")
	resolve := func(domains ...string) inbound.Resolution {
		t.Helper()
		r, err := f.repo.ResolveInbound(f.ctx, domains)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := resolve("mail.example.com", "second.example.com"); r.Status != inbound.Routed || r.AppID != f.app {
		t.Fatalf("%+v", r)
	}
	if r := resolve("mail.example.com", "other.example.com"); r.Status != inbound.Unroutable || r.AppID != "" {
		t.Fatalf("two apps: %+v", r)
	}
	if r := resolve("mail.example.com", "missing.example.com"); r.Status != inbound.Unroutable {
		t.Fatalf("unknown: %+v", r)
	}
	if r := resolve("off.example.com"); r.Status != inbound.Unroutable || r.AppID != f.other {
		t.Fatalf("disabled: %+v", r)
	}
	// Other has no endpoint: unroutable even though the domain is active.
	if r := resolve("other.example.com"); r.Status != inbound.Unroutable {
		t.Fatalf("no endpoint: %+v", r)
	}
	// A pending claim never routes.
	f.exec(`INSERT INTO inbound_domains(id,app_id,domain,verification_hash) VALUES ($1,$2,'claim.example.com',decode(repeat('ab',32),'hex'))`, f.id(), f.other)
	if r := resolve("claim.example.com"); r.Status != inbound.Unroutable {
		t.Fatalf("pending: %+v", r)
	}
}

func TestCommitInboundLimits(t *testing.T) {
	f := newInboundFixture(t)
	f.exec(`UPDATE apps SET inbound_daily_limit=2 WHERE id=$1`, f.app)
	message, deliveries := f.feedbackSharedMessage(f.app, "alice@example.org", "bob@example.org")
	correlated := func() inbound.Record {
		r := f.record(f.app)
		r.Correlation = &inbound.Correlation{MessageID: message, DeliveryID: deliveries[0], Header: "references", FromMatches: true}
		return r
	}
	// Two uncorrelated messages fill the base cap; the third is a tempfail.
	for i := 0; i < 2; i++ {
		if got := f.commit(f.record(f.app)); got != inbound.CommitAccepted {
			t.Fatalf("message %d: %v", i, got)
		}
	}
	if got := f.commit(f.record(f.app)); got != inbound.CommitTempfailLimit {
		t.Fatalf("over the daily cap: %v", got)
	}
	// Replies to the app's own mail still get in, up to twice the cap in total.
	for i := 0; i < 2; i++ {
		if got := f.commit(correlated()); got != inbound.CommitAccepted {
			t.Fatalf("correlated %d: %v", i, got)
		}
	}
	if got := f.commit(correlated()); got != inbound.CommitTempfailLimit {
		t.Fatalf("over the hard ceiling: %v", got)
	}
	if got := f.commit(f.record(f.app)); got != inbound.CommitTempfailLimit {
		t.Fatalf("uncorrelated after the ceiling: %v", got)
	}
	if f.messages(f.app) != 4 || f.counter(f.app, "accepted") != 4 || f.counter(f.app, "correlated") != 2 || f.counter(f.app, "tempfail_limit") != 3 {
		t.Fatalf("messages %d accepted %d correlated %d limited %d", f.messages(f.app), f.counter(f.app, "accepted"), f.counter(f.app, "correlated"), f.counter(f.app, "tempfail_limit"))
	}
	// The pre-check reports the ceiling too, so nothing is uploaded for it.
	if check, err := f.repo.CheckInbound(f.ctx, f.app, f.record(f.app).Fingerprints, false); err != nil || check != inbound.CheckFull {
		t.Fatalf("pre-check: %v %v", check, err)
	}
}

func TestCommitInboundPendingLimit(t *testing.T) {
	f := newInboundFixture(t)
	f.exec(`UPDATE apps SET inbound_pending_limit=1 WHERE id=$1`, f.app)
	if got := f.commit(f.record(f.app)); got != inbound.CommitAccepted {
		t.Fatal(got)
	}
	// One undelivered inbound webhook is the limit: further mail is a tempfail
	// at the pre-check (Backlog), at the commit and in the routing snapshot.
	r, err := f.repo.ResolveInbound(f.ctx, []string{"mail.example.com"})
	if err != nil || r.Status != inbound.Backlog {
		t.Fatalf("%+v %v", r, err)
	}
	if got := f.commit(f.record(f.app)); got != inbound.CommitTempfailLimit {
		t.Fatalf("commit at the pending limit: %v", got)
	}
	set, err := f.repo.InboundRoutes(f.ctx)
	if err != nil || len(set.Routes) != 1 || set.Routes[0].Accept {
		t.Fatalf("snapshot at the pending limit: %+v %v", set, err)
	}
	// Delivered or failed deliveries are no longer pending.
	f.exec(`UPDATE webhook_deliveries SET state='delivered',delivered_at=now() WHERE lane='inbound'`)
	if got := f.commit(f.record(f.app)); got != inbound.CommitAccepted {
		t.Fatalf("after the backlog drained: %v", got)
	}
}

// Deliveries carry no app ID, so the commit checks in code that a correlation
// belongs to the receiving app and message.
func TestCommitInboundGuardsCorrelation(t *testing.T) {
	f := newInboundFixture(t)
	f.domain(f.other, "other.example.com", "active")
	mine, myDeliveries := f.feedbackSharedMessage(f.app, "alice@example.org")
	_, foreignDeliveries := f.feedbackSharedMessage(f.other, "alice@example.org")
	stored := func(rec inbound.Record) (message, delivery *string, header *string) {
		t.Helper()
		var m, d, h *string
		if err := f.owner.QueryRowContext(f.ctx, `SELECT correlated_message_id,correlated_delivery_id,correlation_header FROM inbound_messages WHERE id=$1`, rec.ID).Scan(&m, &d, &h); err != nil {
			t.Fatal(err)
		}
		return m, d, h
	}
	// Another app's message: the whole correlation is dropped.
	var otherMessage string
	if err := f.owner.QueryRowContext(f.ctx, `SELECT message_id FROM deliveries WHERE id=$1`, foreignDeliveries[0]).Scan(&otherMessage); err != nil {
		t.Fatal(err)
	}
	rec := f.record(f.app)
	rec.Correlation = &inbound.Correlation{MessageID: otherMessage, DeliveryID: foreignDeliveries[0], Header: "in_reply_to", FromMatches: true}
	if f.commit(rec) != inbound.CommitAccepted {
		t.Fatal("commit")
	}
	if m, d, h := stored(rec); m != nil || d != nil || h != nil {
		t.Fatal("a correlation to another app was stored")
	}
	// My message with another app's delivery: message-level only.
	rec = f.record(f.app)
	rec.Correlation = &inbound.Correlation{MessageID: mine, DeliveryID: foreignDeliveries[0], Header: "in_reply_to", FromMatches: true}
	if f.commit(rec) != inbound.CommitAccepted {
		t.Fatal("commit")
	}
	if m, d, h := stored(rec); m == nil || *m != mine || d != nil || h == nil {
		t.Fatal("a foreign delivery was attached to my message")
	}
	// A delivery of another message of the same app does not belong to this message.
	other, _ := f.feedbackSharedMessage(f.app, "bob@example.org")
	rec = f.record(f.app)
	rec.Correlation = &inbound.Correlation{MessageID: other, DeliveryID: myDeliveries[0], Header: "references"}
	if f.commit(rec) != inbound.CommitAccepted {
		t.Fatal("commit")
	}
	if m, d, _ := stored(rec); m == nil || *m != other || d != nil {
		t.Fatal("a delivery of a different message was attached")
	}
	// A correct one is kept.
	rec = f.record(f.app)
	rec.Correlation = &inbound.Correlation{MessageID: mine, DeliveryID: myDeliveries[0], Header: "in_reply_to", FromMatches: true}
	if f.commit(rec) != inbound.CommitAccepted {
		t.Fatal("commit")
	}
	if m, d, _ := stored(rec); m == nil || d == nil || *d != myDeliveries[0] {
		t.Fatal("a valid correlation was dropped")
	}
}

func TestReplyCandidatesBothFormsAndAppScope(t *testing.T) {
	f := newInboundFixture(t)
	sharedMessage, sharedDeliveries := f.feedbackSharedMessage(f.app, "Alice@Example.org", "bob@example.org")
	legacyMessage, legacyDelivery := f.feedbackDelivery(f.app, "carol@example.org", "accepted")
	_, foreign := f.feedbackSharedMessage(f.other, "dave@example.org")
	var foreignMessage string
	if err := f.owner.QueryRowContext(f.ctx, `SELECT message_id FROM deliveries WHERE id=$1`, foreign[0]).Scan(&foreignMessage); err != nil {
		t.Fatal(err)
	}
	got, err := f.repo.ReplyCandidates(f.ctx, f.app, []string{sharedMessage, legacyDelivery, foreignMessage})
	if err != nil {
		t.Fatal(err)
	}
	byDelivery := map[string]inbound.Candidate{}
	for _, c := range got {
		byDelivery[c.DeliveryID] = c
	}
	if len(got) != 3 || byDelivery[sharedDeliveries[0]].Recipient != "alice@example.org" || !byDelivery[sharedDeliveries[0]].Shared || byDelivery[sharedDeliveries[1]].MessageID != sharedMessage ||
		byDelivery[legacyDelivery].Shared || byDelivery[legacyDelivery].MessageID != legacyMessage || byDelivery[legacyDelivery].SenderDomain != "sender.example" {
		t.Fatalf("%+v", got)
	}
	// A message ID does not match the legacy form and a delivery ID does not
	// match the shared form.
	if got, _ = f.repo.ReplyCandidates(f.ctx, f.app, []string{legacyMessage, sharedDeliveries[0]}); len(got) != 0 {
		t.Fatalf("wrong-form identifiers matched: %+v", got)
	}
	// Nothing leaks across apps.
	if got, _ = f.repo.ReplyCandidates(f.ctx, f.other, []string{sharedMessage, legacyDelivery}); len(got) != 0 {
		t.Fatalf("another app's candidates: %+v", got)
	}
	if got, _ = f.repo.ReplyCandidates(f.ctx, f.app, []string{"not-an-id"}); len(got) != 0 {
		t.Fatal("malformed identifier matched")
	}
}

func TestInboundRoutesSnapshot(t *testing.T) {
	f := newInboundFixture(t)
	f.domain(f.app, "off.example.com", "disabled")
	f.domain(f.other, "noendpoint.example.com", "active")
	f.exec(`INSERT INTO inbound_domains(id,app_id,domain,verification_hash) VALUES ($1,$2,'claim.example.com',decode(repeat('ab',32),'hex'))`, f.id(), f.other)
	set, err := f.repo.InboundRoutes(f.ctx)
	if err != nil || !set.Open {
		t.Fatalf("%+v %v", set, err)
	}
	accept := map[string]bool{}
	for _, r := range set.Routes {
		accept[r.Domain] = r.Accept
	}
	if len(accept) != 3 || !accept["mail.example.com"] || accept["off.example.com"] || accept["noendpoint.example.com"] {
		t.Fatalf("%v", accept)
	}
	// A paused app's domains remain listed so senders queue (451), not bounce.
	f.exec(`UPDATE apps SET active=false WHERE id=$1`, f.app)
	set, _ = f.repo.InboundRoutes(f.ctx)
	for _, r := range set.Routes {
		if r.Accept {
			t.Fatalf("%s accepts while its app is paused", r.Domain)
		}
	}
	// With the gate closed the snapshot still builds and reports it.
	f.exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
	set, err = f.repo.InboundRoutes(f.ctx)
	if err != nil || set.Open || len(set.Routes) != 3 {
		t.Fatalf("closed gate: %+v %v", set, err)
	}
	// And every mutating path refuses while closed.
	if _, err = f.repo.CommitInbound(f.ctx, f.record(f.app)); err == nil {
		t.Fatal("committed with the gate closed")
	}
	if _, err = f.repo.ResolveInbound(f.ctx, []string{"mail.example.com"}); err == nil {
		t.Fatal("resolved with the gate closed")
	}
}

func TestCommitInboundRejectsMalformedRecords(t *testing.T) {
	f := newInboundFixture(t)
	for name, mutate := range map[string]func(*inbound.Record){
		"wrong kind":    func(r *inbound.Record) { r.ObjectKey = strings.Replace(r.ObjectKey, "inbound/", "bounces/", 1) },
		"id mismatch":   func(r *inbound.Record) { r.ID = strings.Repeat("a", 32) },
		"no recipients": func(r *inbound.Record) { r.Recipients = nil },
		"no domains":    func(r *inbound.Record) { r.Domains = nil },
		"huge":          func(r *inbound.Record) { r.Size = inbound.MaxBytes + 1 },
	} {
		rec := f.record(f.app)
		mutate(&rec)
		if _, err := f.repo.CommitInbound(f.ctx, rec); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A missing object aborts the commit.
	f.repo.Objects = objectVerifierFunc(func(context.Context, string) error { return errors.New("missing") })
	if _, err := f.repo.CommitInbound(f.ctx, f.record(f.app)); err == nil || f.messages(f.app) != 0 {
		t.Fatal("committed without the object")
	}
}

// A value the schema can never store is a deterministic refusal, not an error
// that would make the sender retry forever.
func TestCommitInboundDeterministicFailureIsRejected(t *testing.T) {
	f := newInboundFixture(t)
	rec := f.record(f.app)
	rec.Auth.DKIM = []inbound.DKIMResult{{Result: "pass", Domain: "a\x00b.example"}}
	status, err := f.repo.CommitInbound(f.ctx, rec)
	if err != nil || status != inbound.CommitRejected {
		t.Fatalf("%v %v", status, err)
	}
	if f.messages(f.app) != 0 {
		t.Fatal("a rejected record left a message")
	}
	// A transient failure (the gate closing) stays an error.
	f.exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
	if _, err = f.repo.CommitInbound(f.ctx, f.record(f.app)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCheckInboundAppliesTheRealCapAndCountersAreBounded(t *testing.T) {
	f := newInboundFixture(t)
	f.exec(`UPDATE apps SET inbound_daily_limit=1 WHERE id=$1`, f.app)
	if f.commit(f.record(f.app)) != inbound.CommitAccepted {
		t.Fatal("first")
	}
	fp := f.record(f.app).Fingerprints
	if got, err := f.repo.CheckInbound(f.ctx, f.app, fp, false); err != nil || got != inbound.CheckFull {
		t.Fatalf("an uncorrelated message at the base cap: %v %v", got, err)
	}
	if got, err := f.repo.CheckInbound(f.ctx, f.app, fp, true); err != nil || got != inbound.CheckNew {
		t.Fatalf("a reply is exempt up to twice the cap: %v %v", got, err)
	}
	for _, result := range []string{"duplicate", "tempfail_routing", "tempfail_limit", "tempfail_internal"} {
		if err := f.repo.CountInbound(f.ctx, f.app, result); err != nil {
			t.Fatal(result, err)
		}
		if f.counter(f.app, result) != 1 {
			t.Fatal("not counted", result)
		}
	}
	if f.repo.CountInbound(f.ctx, f.app, "accepted") == nil || f.repo.CountInbound(f.ctx, "bad", "duplicate") == nil {
		t.Fatal("the counter accepts values it must not")
	}
}

// The object presence check can be slow; it must not hold the share lock on the
// app row, which would queue outbound writers behind S3.
func TestCommitInboundVerifiesObjectBeforeLockingTheApp(t *testing.T) {
	f := newInboundFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.repo.Objects = objectVerifierFunc(func(context.Context, string) error {
		close(entered)
		<-release
		return nil
	})
	result := make(chan error, 1)
	go func() {
		_, err := f.repo.CommitInbound(f.ctx, f.record(f.app))
		result <- err
	}()
	<-entered
	// An exclusive write to the app row completes while the check is pending.
	done := make(chan error, 1)
	go func() {
		_, err := f.owner.ExecContext(f.ctx, `UPDATE apps SET name='renamed' WHERE id=$1`, f.app)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the app row is locked while the object is being verified")
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestWorstCaseAuthenticationPassesTheDatabaseCheck(t *testing.T) {
	f := newInboundFixture(t)
	long := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	rec := f.record(f.app)
	rec.Auth = inbound.Authentication{SPF: inbound.SPFResult{Result: "softfail", Domain: long, Identity: "mailfrom"}, DMARC: inbound.DMARCResult{Result: "temperror", Domain: long, Policy: "quarantine"}, ARC: inbound.ARCResult{Present: true}}
	for i := 0; i < 8; i++ {
		rec.Auth.DKIM = append(rec.Auth.DKIM, inbound.DKIMResult{Result: "temperror", Domain: long})
	}
	if got := f.commit(rec); got != inbound.CommitAccepted {
		t.Fatalf("worst-case authentication: %v", got)
	}
}
