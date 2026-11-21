package postgres

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/customdomain"
)

// outcome is a tracking check result with DNS fixed, since DNS never decides.
func outcome(https customdomain.HTTPSResult) customdomain.TrackingOutcome {
	return customdomain.TrackingOutcome{DNS: customdomain.DNSWrongTarget, HTTPS: https}
}

// trackingCheck runs a manual check with the given outcome and returns the row.
func (f *bounceFixture) trackingCheck(appID, sender string, o customdomain.TrackingOutcome) admin.TrackingDomain {
	f.t.Helper()
	c, err := f.repo.BeginTrackingCheck(f.ctx, appID, sender)
	if err != nil {
		f.t.Fatal(err)
	}
	d, written, err := f.repo.FinishTrackingCheck(f.ctx, "operator", c, o)
	if err != nil || !written {
		f.t.Fatal("check not recorded", written, err)
	}
	return d
}

func (f *bounceFixture) trackingState(appID, domain string) (state string, failures int, next time.Time) {
	f.t.Helper()
	if err := f.db.QueryRowContext(f.ctx, `SELECT state,failures,next_check_at FROM tracking_domains WHERE app_id=$1 AND domain=$2`, appID, domain).Scan(&state, &failures, &next); err != nil {
		f.t.Fatal(err)
	}
	return
}

func TestTrackingDomainRules(t *testing.T) {
	f := newBounceFixture(t)
	ctx := f.ctx
	a, b := f.app("A"), f.app("B")
	f.sender(a, "example.com", true)
	f.sender(a, "pending.example", false)
	f.sender(a, "other.example", true)
	f.sender(b, "example.com", true)

	for name, call := range map[string]func() error{
		"an unverified sender domain": func() error {
			_, err := f.repo.SetTrackingDomain(ctx, "operator", a, "pending.example", "links.pending.example")
			return err
		},
		"an unknown sender domain": func() error {
			_, err := f.repo.SetTrackingDomain(ctx, "operator", a, "nope.example", "links.nope.example")
			return err
		},
		"an unknown app": func() error {
			_, err := f.repo.SetTrackingDomain(ctx, "operator", f.id(), "example.com", "links.example.com")
			return err
		},
		"the sender domain itself": func() error {
			_, err := f.repo.SetTrackingDomain(ctx, "operator", a, "example.com", "example.com")
			return err
		},
		"a lookalike suffix": func() error {
			_, err := f.repo.SetTrackingDomain(ctx, "operator", a, "example.com", "links.not"+"example.com")
			return err
		},
		"a name over 253 bytes": func() error {
			_, err := f.repo.SetTrackingDomain(ctx, "operator", a, "example.com", strings.Repeat("a.", 126)+"example.com")
			return err
		},
	} {
		if err := call(); err == nil {
			t.Error("accepted:", name)
		}
	}
	if _, err := f.repo.SetTrackingDomain(ctx, "operator", a, "pending.example", "links.pending.example"); !errors.Is(err, admin.ErrSenderDomainUnverified) {
		t.Error("unverified:", err)
	}

	d, err := f.repo.SetTrackingDomain(ctx, "operator", a, "example.com", "links.example.com")
	if err != nil || d.State != "pending" || d.Name != "links.example.com" {
		t.Fatal(d, err)
	}
	// The same name again changes nothing.
	if again, err := f.repo.SetTrackingDomain(ctx, "operator", a, "example.com", "links.example.com"); err != nil || !again.CreatedAt.Equal(d.CreatedAt) {
		t.Fatal("same name", again, err)
	}
	// Another app may hold the same name (M9).
	if _, err = f.repo.SetTrackingDomain(ctx, "operator", b, "example.com", "links.example.com"); err != nil {
		t.Fatal("two apps may share a tracking domain", err)
	}
	// A deeper name under two verified sender domains belongs to one of them only.
	f.sender(a, "links.example.com", true)
	if _, err = f.repo.SetTrackingDomain(ctx, "operator", a, "links.example.com", "x.links.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.SetTrackingDomain(ctx, "operator", a, "example.com", "x.links.example.com"); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("a name held under another sender domain of the app is a conflict", err)
	}
	if _, err = f.repo.SetTrackingDomain(ctx, "operator", a, "other.example", "t.other.example"); err != nil {
		t.Fatal(err)
	}

	// Pausing the app refuses changes.
	f.exec(`UPDATE apps SET active=false WHERE id=$1`, b)
	if _, err = f.repo.SetTrackingDomain(ctx, "operator", b, "example.com", "z.example.com"); !errors.Is(err, admin.ErrAppPaused) {
		t.Fatal("paused", err)
	}
	f.exec(`UPDATE apps SET active=true WHERE id=$1`, b)

	// Replacing: the old name becomes a tombstone.
	f.trackingCheck(a, "example.com", outcome(customdomain.HTTPSOK))
	if _, err = f.repo.SetTrackingDomain(ctx, "operator", a, "example.com", "go.example.com"); err != nil {
		t.Fatal(err)
	}
	if s, _, next := f.trackingState(a, "links.example.com"); s != "removed" || time.Until(next) < 23*time.Hour {
		t.Fatal("the replaced domain is a tombstone checked daily", s, next)
	}
	list, err := f.repo.ListTrackingDomains(ctx, a)
	if err != nil || list["example.com"].Name != "go.example.com" || len(list) != 3 {
		t.Fatal("the app's list shows the live domains only", list, err)
	}
	if got, err := f.repo.GetTrackingDomain(ctx, a, "example.com"); err != nil || got.Name != "go.example.com" {
		t.Fatal(got, err)
	}

	// Revival: the removed name is added again and starts over as pending.
	revived, err := f.repo.SetTrackingDomain(ctx, "operator", a, "example.com", "links.example.com")
	if err != nil || revived.State != "pending" || revived.HTTPSResult != nil || revived.RemovedAt != nil || revived.VerifiedAt != nil || revived.LastOKAt != nil {
		t.Fatal("revived", revived, err)
	}
	var rows int
	if err = f.db.QueryRowContext(ctx, `SELECT count(*) FROM tracking_domains WHERE app_id=$1 AND domain='links.example.com'`, a).Scan(&rows); err != nil || rows != 1 {
		t.Fatal("a revived name reuses its row", rows, err)
	}

	// Delete leaves a tombstone, and the operator list keeps it.
	if err = f.repo.DeleteTrackingDomain(ctx, "operator", a, "example.com"); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.DeleteTrackingDomain(ctx, "operator", a, "example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("delete twice", err)
	}
	if _, err = f.repo.GetTrackingDomain(ctx, a, "example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("a removed domain is not served as the sender's", err)
	}
	if _, err = f.repo.BeginTrackingCheck(ctx, a, "example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("a removed domain has no manual check", err)
	}
	all, err := f.repo.ListAllTrackingDomains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	apps := map[string]bool{}
	for _, item := range all {
		apps[item.AppID] = true
		if item.State == "removed" {
			removed++
			if item.RemovedAt == nil {
				t.Fatal("removed_at missing", item)
			}
		}
	}
	if removed != 2 || !apps[a] || !apps[b] || len(all) != 5 {
		t.Fatal("operator list", removed, apps, len(all))
	}

	// Changes are audited in the transaction that makes them (so the order within
	// one is not fixed): five adds, one activation, three removals.
	counts := map[string]int{}
	rowsAudit, err := f.db.QueryContext(ctx, `SELECT action FROM audit_events WHERE app_id=$1 AND action LIKE 'tracking_domain.%'`, a)
	if err != nil {
		t.Fatal(err)
	}
	for rowsAudit.Next() {
		var s string
		if err = rowsAudit.Scan(&s); err != nil {
			t.Fatal(err)
		}
		counts[s]++
	}
	rowsAudit.Close()
	if counts["tracking_domain.added"] != 5 || counts["tracking_domain.activated"] != 1 || counts["tracking_domain.removed"] != 3 || len(counts) != 3 {
		t.Fatal("audit", counts)
	}
}

func TestTrackingDomainTransitions(t *testing.T) {
	f := newBounceFixture(t)
	a := f.app("A")
	f.sender(a, "example.com", true)
	if _, err := f.repo.SetTrackingDomain(f.ctx, "operator", a, "example.com", "links.example.com"); err != nil {
		t.Fatal(err)
	}
	const domain = "links.example.com"
	expires := time.Now().Add(60 * 24 * time.Hour).UTC().Truncate(time.Second)

	d := f.trackingCheck(a, "example.com", outcome(customdomain.HTTPSCertificate))
	if s, n, _ := f.trackingState(a, domain); s != "pending" || n != 1 || d.VerifiedAt != nil || d.LastOKAt != nil {
		t.Fatal("pending stays pending on a failure", s, n)
	}
	// DNS is informational: a wrong target with a passing HTTPS activates.
	d = f.trackingCheck(a, "example.com", customdomain.TrackingOutcome{DNS: customdomain.DNSWrongTarget, Observed: "proxy.cdn.example", HTTPS: customdomain.HTTPSOK, CertificateExpires: expires})
	s, n, next := f.trackingState(a, domain)
	if s != "active" || n != 0 || d.VerifiedAt == nil || d.LastOKAt == nil || time.Until(next) < 29*time.Minute || time.Until(next) > 31*time.Minute {
		t.Fatal("active after an HTTPS pass, rechecked in 30 minutes", s, n, next)
	}
	if d.DNSResult == nil || *d.DNSResult != "wrong_target" || d.ObservedTarget == nil || *d.ObservedTarget != "proxy.cdn.example" || d.CertificateExpiresAt == nil || !d.CertificateExpiresAt.Equal(expires) {
		t.Fatal("results recorded", d)
	}
	// A failed handshake has no certificate: the last expiry is kept.
	d = f.trackingCheck(a, "example.com", outcome(customdomain.HTTPSUnreachable))
	if s, n, _ := f.trackingState(a, domain); s != "active" || n != 1 || d.CertificateExpiresAt == nil {
		t.Fatal("one unreachable keeps it active", s, n)
	}
	f.trackingCheck(a, "example.com", outcome(customdomain.HTTPSUnreachable))
	if s, n, _ := f.trackingState(a, domain); s != "failing" || n != 2 {
		t.Fatal("two unreachable in a row fail it", s, n)
	}
	f.trackingCheck(a, "example.com", outcome(customdomain.HTTPSOK))
	if s, n, _ := f.trackingState(a, domain); s != "active" || n != 0 {
		t.Fatal("failing recovers on the first pass", s, n)
	}
	// A DNS failure alone never takes an active domain out of use.
	f.trackingCheck(a, "example.com", customdomain.TrackingOutcome{DNS: customdomain.DNSMissing, HTTPS: customdomain.HTTPSOK})
	if s, _, _ := f.trackingState(a, domain); s != "active" {
		t.Fatal("DNS decided the state", s)
	}
	f.trackingCheck(a, "example.com", outcome(customdomain.HTTPSRedirected))
	if s, _, next := f.trackingState(a, domain); s != "failing" || time.Until(next) > 11*time.Minute {
		t.Fatal("a definitive failure fails it at once, rechecked in 10 minutes", s, next)
	}
	f.trackingCheck(a, "example.com", outcome(customdomain.HTTPSNotThinpost))

	// Removed rows are checked and stay removed; a passing check never
	// resurrects them.
	if err := f.repo.DeleteTrackingDomain(f.ctx, "operator", a, "example.com"); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE tracking_domains SET next_check_at=now()`)
	due, err := f.repo.ClaimDueTrackingDomains(f.ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatal("a removed row is checked", due, err)
	}
	got, written, err := f.repo.FinishTrackingCheck(f.ctx, BackgroundCheckActor, due[0], outcome(customdomain.HTTPSOK))
	if err != nil || !written || got.State != "removed" || got.HTTPSResult == nil || *got.HTTPSResult != "ok" {
		t.Fatal("removed stays removed", got, written, err)
	}
	if s, _, next := f.trackingState(a, domain); s != "removed" || time.Until(next) < 23*time.Hour {
		t.Fatal("a removed row is checked daily", s, next)
	}

	var actions []string
	rows, err := f.db.QueryContext(f.ctx, `SELECT action FROM audit_events WHERE app_id=$1 AND action LIKE 'tracking_domain.%' ORDER BY occurred_at,id`, a)
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
	want := []string{"tracking_domain.added", "tracking_domain.activated", "tracking_domain.failing", "tracking_domain.activated", "tracking_domain.failing", "tracking_domain.removed"}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatal("audited changes", actions)
	}
}

func TestTrackingDomainCheckLeases(t *testing.T) {
	f := newBounceFixture(t)
	ctx := f.ctx
	a := f.app("A")
	f.sender(a, "example.com", true)
	if _, err := f.repo.SetTrackingDomain(ctx, "operator", a, "example.com", "links.example.com"); err != nil {
		t.Fatal(err)
	}

	t.Run("a manual check beats a background check in flight", func(t *testing.T) {
		due, err := f.repo.ClaimDueTrackingDomains(ctx, 10)
		if err != nil || len(due) != 1 || due[0].Domain != "links.example.com" {
			t.Fatal("claim", due, err)
		}
		if again, err := f.repo.ClaimDueTrackingDomains(ctx, 10); err != nil || len(again) != 0 {
			t.Fatal("a leased row was claimed twice", again, err)
		}
		d, err := f.repo.GetTrackingDomain(ctx, a, "example.com")
		if err != nil || !d.Checking {
			t.Fatal("a check in flight shows as checking", d, err)
		}
		manual, err := f.repo.BeginTrackingCheck(ctx, a, "example.com")
		if err != nil {
			t.Fatal(err)
		}
		if _, written, err := f.repo.FinishTrackingCheck(ctx, BackgroundCheckActor, due[0], outcome(customdomain.HTTPSCertificate)); err != nil || written {
			t.Fatal("a stale result was written", written, err)
		}
		d, written, err := f.repo.FinishTrackingCheck(ctx, "operator", manual, outcome(customdomain.HTTPSOK))
		if err != nil || !written || d.State != "active" || d.Checking {
			t.Fatal("the manual result", d, written, err)
		}
		if _, written, _ = f.repo.FinishTrackingCheck(ctx, "operator", manual, outcome(customdomain.HTTPSCertificate)); written {
			t.Fatal("a token was used twice")
		}
	})

	t.Run("a removal drops the lease, so an in-flight result is not written", func(t *testing.T) {
		f.exec(`UPDATE tracking_domains SET next_check_at=now(),check_token=NULL,check_until=NULL`)
		due, err := f.repo.ClaimDueTrackingDomains(ctx, 10)
		if err != nil || len(due) != 1 {
			t.Fatal(due, err)
		}
		if err = f.repo.DeleteTrackingDomain(ctx, "operator", a, "example.com"); err != nil {
			t.Fatal(err)
		}
		if _, written, err := f.repo.FinishTrackingCheck(ctx, BackgroundCheckActor, due[0], outcome(customdomain.HTTPSOK)); err != nil || written {
			t.Fatal("a result was written after removal", written, err)
		}
	})

	t.Run("an expired lease is claimable again, and a batch is bounded", func(t *testing.T) {
		f.exec(`UPDATE tracking_domains SET next_check_at=now(),check_token=NULL,check_until=NULL`)
		for i := 0; i < 5; i++ {
			s := fmt.Sprintf("s%d.example", i)
			f.sender(a, s, true)
			if _, err := f.repo.SetTrackingDomain(ctx, "operator", a, s, "links."+s); err != nil {
				t.Fatal(err)
			}
		}
		first, err := f.repo.ClaimDueTrackingDomains(ctx, 3)
		if err != nil || len(first) != 3 {
			t.Fatal("limit", first, err)
		}
		f.exec(`UPDATE tracking_domains SET check_until=now()-interval '1 second' WHERE check_token=$1`, first[0].Token)
		again, err := f.repo.ClaimDueTrackingDomains(ctx, 10)
		if err != nil || len(again) != 6 {
			t.Fatal("expired leases are claimable again", len(again), err)
		}
	})

	t.Run("replicas never claim the same row", func(t *testing.T) {
		f.exec(`UPDATE tracking_domains SET next_check_at=now(),check_token=NULL,check_until=NULL`)
		var mu sync.Mutex
		seen := map[string]int{}
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := f.repo.ClaimDueTrackingDomains(ctx, 2)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, c := range got {
					seen[c.ID]++
				}
			}()
		}
		wg.Wait()
		for id, n := range seen {
			if n != 1 {
				t.Fatal("row claimed twice", id, n)
			}
		}
		if len(seen) != 6 {
			t.Fatal("every due row is claimed once", len(seen))
		}
	})

	t.Run("invalid results are refused", func(t *testing.T) {
		c, err := f.repo.BeginTrackingCheck(ctx, a, "s0.example")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = f.repo.FinishTrackingCheck(ctx, "operator", c, customdomain.TrackingOutcome{DNS: "bogus", HTTPS: customdomain.HTTPSOK}); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatal(err)
		}
	})
}

// Preparation uses a custom tracking domain only when it is the app's, active,
// checked within 3 hours, and under the message's sender domain; the most
// specific usable one wins.
func TestPreparationClaimsTheTrackingDomain(t *testing.T) {
	f := newBounceFixture(t)
	ctx := f.ctx
	a, b := f.app("A"), f.app("B")
	for _, app := range []string{a, b} {
		f.sender(app, "example.com", true)
		f.sender(app, "news.example.com", true)
	}
	queue := func(app, sender string) {
		m := f.id()
		if sender == "" {
			f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection) VALUES ($1,$2,$3,false)`, m, app, "source-"+m)
			return
		}
		f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,sender_domain,sender_domain_rev,subject,recipient_count) VALUES ($1,$2,$3,false,$4,reverse($4::text)||'.','s',1)`, m, app, "source-"+m, sender)
	}
	claimed := func(app, sender string) string {
		t.Helper()
		f.exec(`DELETE FROM messages`)
		queue(app, sender)
		claim, err := f.repo.ClaimPreparation(ctx, f.id(), 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return claim.TrackingDomain
	}
	add := func(app, sender, name string, https customdomain.HTTPSResult) {
		t.Helper()
		if _, err := f.repo.SetTrackingDomain(ctx, "operator", app, sender, name); err != nil {
			t.Fatal(err)
		}
		if https != "" {
			f.trackingCheck(app, sender, outcome(https))
		}
	}
	if got := claimed(a, "example.com"); got != "" {
		t.Fatal("no domain configured", got)
	}
	add(a, "example.com", "links.example.com", customdomain.HTTPSCertificate)
	if got := claimed(a, "example.com"); got != "" {
		t.Fatal("a pending domain is never used", got)
	}
	f.trackingCheck(a, "example.com", outcome(customdomain.HTTPSOK))
	if got := claimed(a, "example.com"); got != "links.example.com" {
		t.Fatal("an active, fresh domain is used", got)
	}
	if got := claimed(a, "news.example.com"); got != "links.example.com" {
		t.Fatal("a verified subdomain uses its parent's", got)
	}
	if got := claimed(b, "example.com"); got != "" {
		t.Fatal("another app's domain is not used", got)
	}
	if got := claimed(a, "other.example"); got != "" {
		t.Fatal("an unrelated sender domain", got)
	}
	if got := claimed(a, "not"+"example.com"); got != "" {
		t.Fatal("a lookalike suffix", got)
	}
	// Most specific wins, but only among usable ones.
	add(a, "news.example.com", "go.news.example.com", customdomain.HTTPSOK)
	if got := claimed(a, "news.example.com"); got != "go.news.example.com" {
		t.Fatal("the most specific domain wins", got)
	}
	if got := claimed(a, "example.com"); got != "links.example.com" {
		t.Fatal("the parent is unaffected", got)
	}
	f.trackingCheck(a, "news.example.com", outcome(customdomain.HTTPSCertificate))
	f.trackingCheck(a, "news.example.com", outcome(customdomain.HTTPSCertificate))
	if got := claimed(a, "news.example.com"); got != "links.example.com" {
		t.Fatal("a subdomain whose own domain is failing falls back to the parent's", got)
	}
	// Staleness: older than 3 hours is not used.
	f.exec(`UPDATE tracking_domains SET last_ok_at=now()-interval '181 minutes' WHERE domain='links.example.com'`)
	if got := claimed(a, "example.com"); got != "" {
		t.Fatal("a stale check is not used", got)
	}
	f.exec(`UPDATE tracking_domains SET last_ok_at=now()-interval '179 minutes' WHERE domain='links.example.com'`)
	if got := claimed(a, "example.com"); got != "links.example.com" {
		t.Fatal("a fresh check is used", got)
	}
	// A domain's DNS result does not matter, and removal stops use at once.
	f.exec(`UPDATE tracking_domains SET dns_result='missing' WHERE domain='links.example.com'`)
	if got := claimed(a, "example.com"); got != "links.example.com" {
		t.Fatal("DNS result blocked use", got)
	}
	f.exec(`UPDATE sender_domains SET active=false WHERE app_id=$1 AND domain='example.com'`, a)
	if got := claimed(a, "example.com"); got != "" {
		t.Fatal("an unverified sender domain", got)
	}
	f.exec(`UPDATE sender_domains SET active=true WHERE app_id=$1 AND domain='example.com'`, a)
	if err := f.repo.DeleteTrackingDomain(ctx, "operator", a, "example.com"); err != nil {
		t.Fatal(err)
	}
	if got := claimed(a, "example.com"); got != "" {
		t.Fatal("a removed domain is never used", got)
	}
	// A message from before schema 28 has no sender domain.
	if got := claimed(a, ""); got != "" {
		t.Fatal("no sender domain", got)
	}
}

// The runtime role gets exactly the reviewed grants on the new table. The
// restricted-role fixture needs PostgreSQL 17 (the SET privilege).
func TestTrackingDomainsRuntimeGrants(t *testing.T) {
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
		`INSERT INTO tracking_domains(id,app_id,sender_domain,domain) VALUES ('` + f.id() + `','` + a + `','example.com','links.example.com')`,
		`UPDATE tracking_domains SET failures=1`,
		`SELECT count(*) FROM tracking_domains`,
		`DELETE FROM tracking_domains`,
	} {
		if _, err := runtime.ExecContext(f.ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := runtime.ExecContext(f.ctx, `TRUNCATE tracking_domains`); err == nil {
		t.Fatal("runtime role may truncate tracking_domains")
	}
}
