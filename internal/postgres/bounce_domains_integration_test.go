package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/customdomain"
	"github.com/zyno-io/thinpost/internal/delivery"
)

// bounceFixture is a migrated, isolated schema with an open gate, one worker
// lease, a main and a second sending address, and a few apps. It runs as the
// schema owner: the runtime role's privileges are covered by
// TestBounceDomainsRuntimeGrants, which needs PostgreSQL 17 or newer.
type bounceFixture struct {
	t     *testing.T
	ctx   context.Context
	db    *sql.DB
	repo  *Repository
	lease ControllerLease
	n     int
}

const (
	bounceGeneration = "00000000000000000000000000000bbb"
	mainAddr         = "45.0.1.22"
	mainName         = "mx.example.net"
	secondAddr       = "45.0.0.10"
	secondName       = "smtp.second.example.org"
)

func newBounceFixture(t *testing.T) *bounceFixture {
	t.Helper()
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "bouncedomains_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(16)
	t.Cleanup(func() { db.Close() })
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := &bounceFixture{t: t, ctx: ctx, db: db, repo: &Repository{DB: db, Generation: bounceGeneration}}
	f.exec(`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, bounceGeneration)
	if f.lease, err = f.repo.AcquireControllerLease(ctx, "worker", "controller", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.RecordEdgeSources(ctx, f.lease, f.caps(true)); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *bounceFixture) id() string {
	f.n++
	return fmt.Sprintf("%032x", f.n+9000)
}

func (f *bounceFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.ExecContext(f.ctx, query, args...); err != nil {
		f.t.Fatal(err)
	}
}

// caps is what the worker reports: the main address, then the second one.
func (f *bounceFixture) caps(anyDomain bool) delivery.EdgeCapabilities {
	return delivery.EdgeCapabilities{AnyBounceDomain: anyDomain, Sources: []delivery.Source{
		{Address: netip.MustParseAddr(mainAddr), Hostname: mainName},
		{Address: netip.MustParseAddr(secondAddr), Hostname: secondName},
	}}
}

func (f *bounceFixture) app(name string) string {
	a := f.id()
	f.exec(`INSERT INTO apps(id,name) VALUES ($1,$2)`, a, name)
	return a
}

func (f *bounceFixture) sender(appID, domain string, active bool) {
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,$2,$3,CASE WHEN $3 THEN now() END)`, appID, domain, active)
}

// queue adds one queued delivery of a message from senderDomain.
func (f *bounceFixture) queue(appID, senderDomain string) string {
	m, d := f.id(), f.id()
	if senderDomain == "" {
		// A message from before schema 28 records no sender domain.
		f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,$3,false,'prepared')`, m, appID, "source-"+m)
	} else {
		f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state,subject,recipient_count,sender_domain,sender_domain_rev) VALUES ($1,$2,$3,false,'prepared','s',1,$4,reverse($4::text)||'.')`, m, appID, "source-"+m, senderDomain)
	}
	f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,$3)`, d, m, "signed-"+d)
	return d
}

// check runs a manual check with the given outcome and returns the row.
func (f *bounceFixture) check(appID, sender string, result customdomain.Result, observed string) admin.BounceDomain {
	f.t.Helper()
	c, err := f.repo.BeginBounceCheck(f.ctx, appID, sender)
	if err != nil {
		f.t.Fatal(err)
	}
	d, written, err := f.repo.FinishBounceCheck(f.ctx, "operator", c, result, observed)
	if err != nil || !written {
		f.t.Fatal("check not recorded", written, err)
	}
	return d
}

// claim claims the one queued delivery and returns the claim.
func (f *bounceFixture) claim(caps delivery.EdgeCapabilities) delivery.Claim {
	f.t.Helper()
	claim, err := f.repo.ClaimDelivery(f.ctx, f.lease, f.id(), caps)
	if err != nil {
		f.t.Fatal("claim", err)
	}
	return claim
}

func (f *bounceFixture) resetDeliveries() {
	f.exec(`DELETE FROM webhook_events`)
	f.exec(`DELETE FROM delivery_attempts`)
	f.exec(`DELETE FROM deliveries`)
	f.exec(`DELETE FROM messages`)
}

func (f *bounceFixture) returnPath(deliveryID string) sql.NullString {
	var rp sql.NullString
	if err := f.db.QueryRowContext(f.ctx, `SELECT return_path_domain FROM delivery_attempts WHERE delivery_id=$1`, deliveryID).Scan(&rp); err != nil {
		f.t.Fatal(err)
	}
	return rp
}

func TestBounceDomainRules(t *testing.T) {
	f := newBounceFixture(t)
	ctx := f.ctx
	a, b := f.app("A"), f.app("B")
	f.sender(a, "example.com", true)
	f.sender(a, "pending.example", false)
	f.sender(b, "example.com", true)

	t.Run("a bounce domain is a strict subdomain of a verified sender domain", func(t *testing.T) {
		for name, tc := range map[string]struct {
			app, sender, domain string
			want                error
		}{
			"the sender domain itself": {a, "example.com", "example.com", admin.ErrInvalidInput},
			"an unrelated domain":      {a, "example.com", "bounces.example.org", admin.ErrInvalidInput},
			"a lookalike suffix":       {a, "example.com", "bounces.not" + "example.com", admin.ErrInvalidInput},
			"an unverified sender":     {a, "pending.example", "b.pending.example", admin.ErrSenderDomainUnverified},
			"an unknown sender":        {a, "nope.example", "b.nope.example", admin.ErrNotFound},
			"an unknown app":           {f.id(), "example.com", "b.example.com", admin.ErrNotFound},
		} {
			if _, err := f.repo.SetBounceDomain(ctx, "operator", tc.app, tc.sender, tc.domain); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v", name, err)
			}
		}
		f.exec(`UPDATE apps SET active=false WHERE id=$1`, b)
		if _, err := f.repo.SetBounceDomain(ctx, "operator", b, "example.com", "bounces.example.com"); !errors.Is(err, admin.ErrAppPaused) {
			t.Error("a paused app changed its bounce domain", err)
		}
		f.exec(`UPDATE apps SET active=true WHERE id=$1`, b)
	})

	t.Run("setting, replacing and removing", func(t *testing.T) {
		d, err := f.repo.SetBounceDomain(ctx, "operator", a, "example.com", "bounces.example.com")
		if err != nil || d.Name != "bounces.example.com" || d.State != "pending" || d.Result != nil || d.Checking {
			t.Fatal("create", d, err)
		}
		if again, err := f.repo.SetBounceDomain(ctx, "operator", a, "example.com", "bounces.example.com"); err != nil || !again.CreatedAt.Equal(d.CreatedAt) {
			t.Fatal("the same name must change nothing", again, err)
		}
		// Several apps may share a custom domain.
		if _, err = f.repo.SetBounceDomain(ctx, "operator", b, "example.com", "bounces.example.com"); err != nil {
			t.Fatal("a second app on the same domain", err)
		}
		replaced, err := f.repo.SetBounceDomain(ctx, "operator", a, "example.com", "mail-bounce.example.com")
		if err != nil || replaced.Name != "mail-bounce.example.com" || replaced.State != "pending" {
			t.Fatal("replace", replaced, err)
		}
		list, err := f.repo.ListBounceDomains(ctx, a)
		if err != nil || len(list) != 1 || list["example.com"].Name != "mail-bounce.example.com" {
			t.Fatal("list", list, err)
		}
		if err = f.repo.DeleteBounceDomain(ctx, "operator", a, "example.com"); err != nil {
			t.Fatal(err)
		}
		if _, err = f.repo.GetBounceDomain(ctx, a, "example.com"); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("deleted domain still found", err)
		}
		if err = f.repo.DeleteBounceDomain(ctx, "operator", a, "example.com"); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("deleting twice", err)
		}
		var added, removed int
		if err = f.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE action='bounce_domain.added'),count(*) FILTER (WHERE action='bounce_domain.removed') FROM audit_events WHERE app_id=$1`, a).Scan(&added, &removed); err != nil || added != 2 || removed != 2 {
			t.Fatal("audit", added, removed, err)
		}
		if err = f.repo.DeleteBounceDomain(ctx, "operator", b, "example.com"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a name another sender domain of the app already uses is a conflict", func(t *testing.T) {
		f.sender(a, "mail.example.com", true)
		if _, err := f.repo.SetBounceDomain(ctx, "operator", a, "example.com", "bounces.mail.example.com"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.repo.SetBounceDomain(ctx, "operator", a, "mail.example.com", "bounces.mail.example.com"); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("the same name on two sender domains of one app", err)
		}
		if err := f.repo.DeleteBounceDomain(ctx, "operator", a, "example.com"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBounceDomainTransitions(t *testing.T) {
	f := newBounceFixture(t)
	a := f.app("A")
	f.sender(a, "example.com", true)
	if _, err := f.repo.SetBounceDomain(f.ctx, "operator", a, "example.com", "bounces.example.com"); err != nil {
		t.Fatal(err)
	}
	state := func() (state, result string, failures int, next time.Time) {
		var r sql.NullString
		if err := f.db.QueryRowContext(f.ctx, `SELECT state,last_result,failures,next_check_at FROM bounce_domains WHERE app_id=$1`, a).Scan(&state, &r, &failures, &next); err != nil {
			t.Fatal(err)
		}
		return state, r.String, failures, next
	}
	d := f.check(a, "example.com", customdomain.ResultMissing, "")
	if s, r, n, _ := state(); s != "pending" || r != "missing" || n != 1 || d.State != "pending" {
		t.Fatal("pending stays pending on a miss", s, r, n)
	}
	d = f.check(a, "example.com", customdomain.ResultOK, mainName)
	if s, _, n, next := state(); s != "active" || n != 0 || d.VerifiedAt == nil || d.LastOKAt == nil || time.Until(next) < 29*time.Minute || time.Until(next) > 31*time.Minute {
		t.Fatal("active after ok, rechecked in 30 minutes", s, n, next)
	}
	f.check(a, "example.com", customdomain.ResultLookupFailed, "")
	if s, r, n, _ := state(); s != "active" || r != "lookup_failed" || n != 1 {
		t.Fatal("one indeterminate failure keeps it active", s, r, n)
	}
	f.check(a, "example.com", customdomain.ResultLookupFailed, "")
	if s, _, n, _ := state(); s != "failing" || n != 2 {
		t.Fatal("two in a row fail it", s, n)
	}
	f.check(a, "example.com", customdomain.ResultOK, mainName)
	if s, _, n, _ := state(); s != "active" || n != 0 {
		t.Fatal("failing recovers on the first ok", s, n)
	}
	f.check(a, "example.com", customdomain.ResultOtherIP, secondName)
	if s, r, _, next := state(); s != "failing" || r != "other_ip" || time.Until(next) > 11*time.Minute {
		t.Fatal("a definitive failure fails it at once, rechecked in 10 minutes", s, r, next)
	}
	d = f.check(a, "example.com", customdomain.ResultNotCNAME, "")
	if d.ObservedTarget != nil {
		t.Fatal("observed target kept", d)
	}
	var actions []string
	rows, err := f.db.QueryContext(f.ctx, `SELECT action FROM audit_events WHERE app_id=$1 AND action LIKE 'bounce_domain.%' ORDER BY occurred_at,id`, a)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, s)
	}
	rows.Close()
	// added, then activated/failing/activated/failing: changes only, none for
	// the checks that left the state alone.
	want := []string{"bounce_domain.added", "bounce_domain.activated", "bounce_domain.failing", "bounce_domain.activated", "bounce_domain.failing"}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatal("audited changes", actions)
	}
}

func TestBounceDomainCheckLeases(t *testing.T) {
	f := newBounceFixture(t)
	ctx := f.ctx
	a := f.app("A")
	f.sender(a, "example.com", true)
	if _, err := f.repo.SetBounceDomain(ctx, "operator", a, "example.com", "bounces.example.com"); err != nil {
		t.Fatal(err)
	}

	t.Run("a manual check beats a background check in flight", func(t *testing.T) {
		due, err := f.repo.ClaimDueBounceDomains(ctx, 10)
		if err != nil || len(due) != 1 || due[0].Target.Name != mainName {
			t.Fatal("claim", due, err)
		}
		if again, err := f.repo.ClaimDueBounceDomains(ctx, 10); err != nil || len(again) != 0 {
			t.Fatal("a leased row was claimed twice", again, err)
		}
		d, err := f.repo.GetBounceDomain(ctx, a, "example.com")
		if err != nil || !d.Checking {
			t.Fatal("a check in flight shows as checking", d, err)
		}
		manual, err := f.repo.BeginBounceCheck(ctx, a, "example.com")
		if err != nil {
			t.Fatal(err)
		}
		// The background result arrives first, with a stale token: dropped.
		if _, written, err := f.repo.FinishBounceCheck(ctx, BackgroundCheckActor, due[0], customdomain.ResultMissing, ""); err != nil || written {
			t.Fatal("a stale result was written", written, err)
		}
		d, written, err := f.repo.FinishBounceCheck(ctx, "operator", manual, customdomain.ResultOK, mainName)
		if err != nil || !written || d.State != "active" {
			t.Fatal("the manual result", d, written, err)
		}
		// A token is single use.
		if _, written, _ = f.repo.FinishBounceCheck(ctx, "operator", manual, customdomain.ResultMissing, ""); written {
			t.Fatal("a result was written twice")
		}
	})

	t.Run("a lease that expired can be taken again and the old holder loses", func(t *testing.T) {
		f.exec(`UPDATE bounce_domains SET next_check_at=now()-interval '1 minute'`)
		first, err := f.repo.ClaimDueBounceDomains(ctx, 10)
		if err != nil || len(first) != 1 {
			t.Fatal(first, err)
		}
		f.exec(`UPDATE bounce_domains SET check_until=now()-interval '1 second'`)
		second, err := f.repo.ClaimDueBounceDomains(ctx, 10)
		if err != nil || len(second) != 1 || second[0].Token == first[0].Token {
			t.Fatal("expired lease not reclaimed", second, err)
		}
		if _, written, _ := f.repo.FinishBounceCheck(ctx, BackgroundCheckActor, first[0], customdomain.ResultOK, mainName); written {
			t.Fatal("the replica that lost its lease wrote")
		}
		if _, written, err := f.repo.FinishBounceCheck(ctx, BackgroundCheckActor, second[0], customdomain.ResultOK, mainName); err != nil || !written {
			t.Fatal(written, err)
		}
	})

	t.Run("a removed domain drops the result of its check", func(t *testing.T) {
		manual, err := f.repo.BeginBounceCheck(ctx, a, "example.com")
		if err != nil {
			t.Fatal(err)
		}
		if err = f.repo.DeleteBounceDomain(ctx, "operator", a, "example.com"); err != nil {
			t.Fatal(err)
		}
		if _, written, err := f.repo.FinishBounceCheck(ctx, "operator", manual, customdomain.ResultOK, mainName); err != nil || written {
			t.Fatal("a result was written for a removed domain", written, err)
		}
	})

	t.Run("replicas never take the same rows", func(t *testing.T) {
		for i := 0; i < 24; i++ {
			sub := fmt.Sprintf("sub%d.example.com", i)
			f.sender(a, sub, true)
			if _, err := f.repo.SetBounceDomain(ctx, "operator", a, sub, "b."+sub); err != nil {
				t.Fatal(err)
			}
		}
		var mu sync.Mutex
		seen := map[string]int{}
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := f.repo.ClaimDueBounceDomains(ctx, 5)
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				for _, c := range got {
					seen[c.ID]++
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
		for id, n := range seen {
			if n != 1 {
				t.Fatal("row claimed more than once", id, n)
			}
		}
		if len(seen) != 24 && len(seen) != 30 {
			t.Log("claimed", len(seen))
		}
		if len(seen) == 0 {
			t.Fatal("nothing claimed")
		}
	})

	t.Run("a closed gate stops claims and writes", func(t *testing.T) {
		f.exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
		defer f.exec(`UPDATE runtime_gate SET mode='open' WHERE singleton`)
		if _, err := f.repo.ClaimDueBounceDomains(ctx, 5); !errors.Is(err, ErrRuntimeClosed) {
			t.Fatal("claimed through a closed gate", err)
		}
		if _, err := f.repo.BeginBounceCheck(ctx, a, "sub0.example.com"); !errors.Is(err, ErrRuntimeClosed) {
			t.Fatal("began a check through a closed gate", err)
		}
	})
}

func TestBounceDomainIPChangeTriggersRecheck(t *testing.T) {
	f := newBounceFixture(t)
	ctx := f.ctx
	a := f.app("A")
	f.sender(a, "example.com", true)
	if _, err := f.repo.SetBounceDomain(ctx, "operator", a, "example.com", "bounces.example.com"); err != nil {
		t.Fatal(err)
	}
	f.check(a, "example.com", customdomain.ResultOK, mainName)
	var next time.Time
	if err := f.db.QueryRowContext(ctx, `SELECT next_check_at FROM bounce_domains`).Scan(&next); err != nil || time.Until(next) < 20*time.Minute {
		t.Fatal("an active domain is not due yet", next, err)
	}
	target, err := f.repo.BounceTarget(ctx, a)
	if err != nil || target.Name != mainName || target.Address != mainAddr || len(target.Reported) != 2 {
		t.Fatal("target before pin", target, err)
	}
	inFlight, err := f.repo.BeginBounceCheck(ctx, a, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	var second string
	if err = f.db.QueryRowContext(ctx, `SELECT id FROM sending_ips WHERE helo_name=$1`, secondName).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.SetAppSendingIP(ctx, "operator", a, &second); err != nil {
		t.Fatal(err)
	}
	var due bool
	var lease sql.NullString
	if err = f.db.QueryRowContext(ctx, `SELECT next_check_at<=clock_timestamp(),check_token FROM bounce_domains`).Scan(&due, &lease); err != nil || !due || lease.Valid {
		t.Fatal("the IP change did not make the domain due", due, lease, err)
	}
	if _, written, _ := f.repo.FinishBounceCheck(ctx, "operator", inFlight, customdomain.ResultOK, mainName); written {
		t.Fatal("a check against the old IP's name was recorded")
	}
	if target, err = f.repo.BounceTarget(ctx, a); err != nil || target.Name != secondName || target.Address != secondAddr {
		t.Fatal("target after pin", target, err)
	}
	// Other apps' domains are untouched.
	b := f.app("B")
	f.sender(b, "example.com", true)
	if _, err = f.repo.SetBounceDomain(ctx, "operator", b, "example.com", "bounces.example.com"); err != nil {
		t.Fatal(err)
	}
	f.check(b, "example.com", customdomain.ResultOK, mainName)
	if _, err = f.repo.SetAppSendingIP(ctx, "operator", a, nil); err != nil {
		t.Fatal(err)
	}
	if err = f.db.QueryRowContext(ctx, `SELECT next_check_at<=clock_timestamp() FROM bounce_domains WHERE app_id=$1`, b).Scan(&due); err != nil || due {
		t.Fatal("another app's domain was made due", due, err)
	}
}

// The claim decides the Return-Path from live rows. Every case below leaves one
// condition different from the one before it.
func TestBounceDomainClaimMatrix(t *testing.T) {
	f := newBounceFixture(t)
	ctx := f.ctx
	a, other := f.app("A"), f.app("Other")
	for _, d := range []string{"example.com", "mail.example.com", "not" + "example.com"} {
		f.sender(a, d, true)
	}
	f.sender(other, "example.com", true)
	set := func(app, sender, name string) {
		if _, err := f.repo.SetBounceDomain(ctx, "operator", app, sender, name); err != nil {
			t.Fatal(err)
		}
		f.check(app, sender, customdomain.ResultOK, mainName)
	}
	set(a, "example.com", "bounces.example.com")

	// expect claims the single queued delivery and checks the Return-Path.
	expect := func(name string, senderDomain string, caps delivery.EdgeCapabilities, want string) {
		t.Helper()
		f.resetDeliveries()
		d := f.queue(a, senderDomain)
		claim := f.claim(caps)
		if claim.DeliveryID != d {
			t.Fatalf("%s: claimed %s", name, claim.DeliveryID)
		}
		custom := want != mainName
		rp := f.returnPath(d)
		if claim.BounceDomain != want || claim.HeloName != mainName || claim.SourceName != mainName || claim.SourceAddress != mainAddr || rp.Valid != custom || custom && rp.String != want {
			t.Fatalf("%s: bounce domain %q helo %q recorded %v, want %q", name, claim.BounceDomain, claim.HeloName, rp, want)
		}
		if custom && claim.BounceTarget != mainName || !custom && claim.BounceTarget != "" {
			t.Fatalf("%s: bounce target %q", name, claim.BounceTarget)
		}
	}

	expect("active, matching target, capable edge", "example.com", f.caps(true), "bounces.example.com")
	expect("an edge without bounce-any-domain/1", "example.com", f.caps(false), mainName)
	expect("a message from a sender domain with no bounce domain", "not"+"example.com", f.caps(true), mainName)
	expect("a message with no recorded sender domain", "", f.caps(true), mainName)

	t.Run("target mismatch", func(t *testing.T) {
		// The domain was verified against the main IP's name; the worker now
		// reports a different name for that address.
		renamed := f.caps(true)
		renamed.Sources[0].Hostname = "mx2.example.net"
		f.resetDeliveries()
		d := f.queue(a, "example.com")
		claim := f.claim(renamed)
		if claim.DeliveryID != d || claim.BounceDomain != "mx2.example.net" || f.returnPath(d).Valid {
			t.Fatalf("custom domain used for a stale target: %+v", claim)
		}
	})

	t.Run("an app pinned to another IP is addressed from it and falls back", func(t *testing.T) {
		var second string
		if err := f.db.QueryRowContext(ctx, `SELECT id FROM sending_ips WHERE helo_name=$1`, secondName).Scan(&second); err != nil {
			t.Fatal(err)
		}
		if _, err := f.repo.SetAppSendingIP(ctx, "operator", a, &second); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := f.repo.SetAppSendingIP(ctx, "operator", a, nil); err != nil {
				t.Fatal(err)
			}
		}()
		f.resetDeliveries()
		d := f.queue(a, "example.com")
		claim := f.claim(f.caps(true))
		if claim.DeliveryID != d || claim.SourceAddress != secondAddr || claim.BounceDomain != secondName || f.returnPath(d).Valid {
			t.Fatalf("verified for the main IP's name, used with another IP: %+v", claim)
		}
	})

	t.Run("stale last_ok_at", func(t *testing.T) {
		f.exec(`UPDATE bounce_domains SET last_ok_at=clock_timestamp()-interval '3 hours 1 minute'`)
		expect("a check older than three hours", "example.com", f.caps(true), mainName)
		f.exec(`UPDATE bounce_domains SET last_ok_at=clock_timestamp()-interval '2 hours 59 minutes'`)
		expect("a check just inside three hours", "example.com", f.caps(true), "bounces.example.com")
		f.exec(`UPDATE bounce_domains SET last_ok_at=clock_timestamp()`)
	})

	t.Run("states other than active", func(t *testing.T) {
		for _, state := range []string{"pending", "failing"} {
			f.exec(`UPDATE bounce_domains SET state=$1`, state)
			expect(state, "example.com", f.caps(true), mainName)
		}
		f.exec(`UPDATE bounce_domains SET state='active'`)
	})

	t.Run("inbound collision", func(t *testing.T) {
		// Inserted directly: the admin paths refuse the collision, and the claim
		// must hold even if a row got in some other way.
		for _, state := range []string{"active", "disabled"} {
			f.exec(`INSERT INTO inbound_domains(id,app_id,domain,state,verified_at) VALUES ($1,$2,'bounces.example.com',$3,now())`, f.id(), other, state)
			expect("a "+state+" receiving domain", "example.com", f.caps(true), mainName)
			f.exec(`DELETE FROM inbound_domains`)
		}
		f.exec(`INSERT INTO inbound_domains(id,app_id,domain,verification_hash) VALUES ($1,$2,'bounces.example.com',decode(repeat('ab',32),'hex'))`, f.id(), other)
		expect("a pending claim on the name does not stop it", "example.com", f.caps(true), "bounces.example.com")
		f.exec(`DELETE FROM inbound_domains`)
	})

	t.Run("reported HELO collision", func(t *testing.T) {
		// The worker now reports the custom domain as the name of its other
		// address (an operator renamed an IP after the customer set up DNS).
		renamed := f.caps(true)
		renamed.Sources[1].Hostname = "bounces.example.com"
		expect("the name of another reported address", "example.com", renamed, mainName)
	})

	t.Run("subdomain scope, most specific wins", func(t *testing.T) {
		expect("a verified subdomain uses its parent's bounce domain", "mail.example.com", f.caps(true), "bounces.example.com")
		set(a, "mail.example.com", "b.mail.example.com")
		expect("its own bounce domain wins over its parent's", "mail.example.com", f.caps(true), "b.mail.example.com")
		expect("the parent's own mail still uses the parent's", "example.com", f.caps(true), "bounces.example.com")
		expect("a name that merely ends with the same letters is not a subdomain", "not"+"example.com", f.caps(true), mainName)
		// Another app's domain on the same names never applies.
		set(other, "example.com", "other-bounces.example.com")
		expect("another app's bounce domain", "example.com", f.caps(true), "bounces.example.com")
	})
}

// Adding a bounce domain and verifying a receiving domain for the same name
// race under the advisory lock: exactly one wins, whoever is first.
func TestBounceInboundExclusion(t *testing.T) {
	f := newBounceFixture(t)
	ctx := f.ctx
	a, b := f.app("A"), f.app("B")
	f.sender(a, "example.com", true)
	digestOf := func(value string) [32]byte { return sha256.Sum256([]byte(value)) }

	t.Run("sequential, each direction", func(t *testing.T) {
		if _, err := f.repo.SetBounceDomain(ctx, "operator", a, "example.com", "bounces.example.com"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.repo.RequestInboundDomain(ctx, "operator", b, "bounces.example.com"); !errors.Is(err, admin.ErrBounceDomainClash) {
			t.Fatal("requested a receiving domain that is a bounce domain", err)
		}
		if err := f.repo.DeleteBounceDomain(ctx, "operator", a, "example.com"); err != nil {
			t.Fatal(err)
		}
		challenge, err := f.repo.RequestInboundDomain(ctx, "operator", b, "bounces.example.com")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.repo.VerifyInboundDomain(ctx, "operator", b, "bounces.example.com", digestOf(challenge.DNSValue)); err != nil {
			t.Fatal(err)
		}
		if _, err = f.repo.SetBounceDomain(ctx, "operator", a, "example.com", "bounces.example.com"); !errors.Is(err, admin.ErrInboundDomainClash) {
			t.Fatal("added a bounce domain that receives mail", err)
		}
		// A disabled receiving domain still owns the name.
		if _, err = f.repo.SetInboundDomainEnabled(ctx, "operator", b, "bounces.example.com", false); err != nil {
			t.Fatal(err)
		}
		if _, err = f.repo.SetBounceDomain(ctx, "operator", a, "example.com", "bounces.example.com"); !errors.Is(err, admin.ErrInboundDomainClash) {
			t.Fatal("added a bounce domain over a disabled receiving domain", err)
		}
		if err = f.repo.DeleteInboundDomain(ctx, "operator", b, "bounces.example.com"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		for round := 0; round < 12; round++ {
			name := fmt.Sprintf("r%d.example.com", round)
			challenge, err := f.repo.RequestInboundDomain(ctx, "operator", b, name)
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			var bounceErr, inboundErr error
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, bounceErr = f.repo.SetBounceDomain(ctx, "operator", a, "example.com", name)
			}()
			go func() {
				defer wg.Done()
				<-start
				_, inboundErr = f.repo.VerifyInboundDomain(ctx, "operator", b, name, digestOf(challenge.DNSValue))
			}()
			close(start)
			wg.Wait()
			if (bounceErr == nil) == (inboundErr == nil) {
				t.Fatalf("round %d: bounce %v, inbound %v: exactly one must win", round, bounceErr, inboundErr)
			}
			if bounceErr != nil && !errors.Is(bounceErr, admin.ErrInboundDomainClash) || inboundErr != nil && !errors.Is(inboundErr, admin.ErrBounceDomainClash) {
				t.Fatalf("round %d: unexpected errors %v %v", round, bounceErr, inboundErr)
			}
			var both bool
			if err = f.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM bounce_domains bd JOIN inbound_domains i ON i.domain=bd.domain AND i.state IN ('active','disabled') WHERE bd.domain=$1)`, name).Scan(&both); err != nil || both {
				t.Fatal("a name is both a bounce domain and a receiving domain", err)
			}
			f.exec(`DELETE FROM bounce_domains WHERE domain=$1`, name)
		}
	})
}

// The runtime role gets exactly the reviewed grants on the new table. The
// restricted-role fixture needs PostgreSQL 17 (the SET privilege).
func TestBounceDomainsRuntimeGrants(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	f := newBounceFixture(t)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	var schema string
	if err = f.db.QueryRowContext(f.ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	runtime, cleanup := restrictedRuntimeForTest(t, f.ctx, f.db, u.String())
	defer cleanup()
	a := f.app("A")
	f.sender(a, "example.com", true)
	for _, statement := range []string{
		`INSERT INTO bounce_domains(id,app_id,sender_domain,domain) VALUES ('` + f.id() + `','` + a + `','example.com','b.example.com')`,
		`UPDATE bounce_domains SET failures=1`,
		`SELECT count(*) FROM bounce_domains`,
		`DELETE FROM bounce_domains`,
		`UPDATE delivery_attempts SET return_path_domain=NULL WHERE false`,
	} {
		if _, err := runtime.ExecContext(f.ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := runtime.ExecContext(f.ctx, `TRUNCATE bounce_domains`); err == nil {
		t.Fatal("runtime role may truncate bounce_domains")
	}
}

// Recording a check (bounce row, then an audit insert that needs a key-share
// lock on the app) and pinning the app's IP (app lock, then bounce rows) must
// not deadlock.
func TestBounceCheckAndIPChangeDoNotDeadlock(t *testing.T) {
	f := newBounceFixture(t)
	ctx := f.ctx
	a := f.app("A")
	f.sender(a, "example.com", true)
	if _, err := f.repo.SetBounceDomain(ctx, "operator", a, "example.com", "bounces.example.com"); err != nil {
		t.Fatal(err)
	}
	var second string
	if err := f.db.QueryRowContext(ctx, `SELECT id FROM sending_ips WHERE helo_name=$1`, secondName).Scan(&second); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 400)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			c, err := f.repo.BeginBounceCheck(ctx, a, "example.com")
			if err != nil {
				errs <- err
				return
			}
			// Alternate results so every other check changes state and audits.
			result := customdomain.ResultOK
			if i%2 == 1 {
				result = customdomain.ResultMissing
			}
			if _, _, err = f.repo.FinishBounceCheck(ctx, "operator", c, result, ""); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			target := &second
			if i%2 == 1 {
				target = nil
			}
			if _, err := f.repo.SetAppSendingIP(ctx, "operator", a, target); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
