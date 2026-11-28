package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/submission"
)

func TestProviderSuppressionsPostgres(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	schema := "provsup_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	owner, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err = Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	const generation = "00000000000000000000000000000ccc"
	counter := 0
	id := func() string { counter++; return fmt.Sprintf("%032x", counter+5000) }
	app, other := id(), id()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := owner.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation)
	exec(`INSERT INTO apps(id,name) VALUES ($1,'One'),($2,'Other')`, app, other)
	key := id()
	exec(`INSERT INTO api_keys(id,digest,app_id) VALUES ($1,$2,$3)`, key, []byte(strings.Repeat("k", 32)), app)
	kapp := submission.App{ID: app, KeyID: key}
	db, cleanup := restrictedRuntimeForTest(t, ctx, owner, u.String())
	defer cleanup()
	repo := &Repository{DB: db, Generation: generation}
	// seed inserts an entry with an explicit age so ordering is deterministic.
	seed := func(appID, address, reason, origin string, age time.Duration) string {
		sid := id()
		lower, domain, _, _ := recipientKeys(address)
		exec(`INSERT INTO suppressions(id,app_id,address,address_lower,domain_lower,reason,origin,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,now()-$8::interval)`,
			sid, appID, address, lower, domain, reason, origin, fmt.Sprintf("%d seconds", int(age.Seconds())))
		return sid
	}
	var ids []string
	for i, e := range []struct{ address, reason, origin string }{
		{"bounce@example.net", "hard_bounce", "recipient"},
		{"complaint@example.net", "spam_complaint", "recipient"},
		{"unsub@example.net", "unsubscribe", "recipient"},
		{"manual@example.net", "manual", "console"},
		{"api@example.net", "manual", "api"},
	} {
		ids = append(ids, seed(app, e.address, e.reason, e.origin, time.Duration(10-i)*time.Minute))
	}
	seed(other, "bounce@example.net", "manual", "api", time.Minute)

	t.Run("list filters, order and cursors", func(t *testing.T) {
		items, more, err := repo.ListProviderSuppressions(ctx, app, submission.ProviderSuppressionFilter{Limit: 100})
		if err != nil || more || len(items) != 5 || items[0].ID != ids[4] || items[4].ID != ids[0] {
			t.Fatal("all", len(items), more, err)
		}
		items, _, err = repo.ListProviderSuppressions(ctx, app, submission.ProviderSuppressionFilter{Limit: 100, Reasons: []string{"manual", "unsubscribe"}, Origins: []string{"api", "console"}})
		if err != nil || len(items) != 2 {
			t.Fatal("reason+origin", len(items), err)
		}
		items, _, _ = repo.ListProviderSuppressions(ctx, app, submission.ProviderSuppressionFilter{Limit: 100, From: time.Now().Add(-7*time.Minute - 30*time.Second), To: time.Now().Add(-5*time.Minute - 30*time.Second)})
		if len(items) != 2 {
			t.Fatal("date window", len(items))
		}
		page1, more, err := repo.ListProviderSuppressions(ctx, app, submission.ProviderSuppressionFilter{Limit: 2})
		if err != nil || !more || len(page1) != 2 {
			t.Fatal("page1", len(page1), more, err)
		}
		page2, more, err := repo.ListProviderSuppressions(ctx, app, submission.ProviderSuppressionFilter{Limit: 2, After: page1[1].ID})
		if err != nil || !more || len(page2) != 2 || page2[0].ID != ids[2] || page2[1].ID != ids[1] {
			t.Fatal("after", page2, more, err)
		}
		page3, more, err := repo.ListProviderSuppressions(ctx, app, submission.ProviderSuppressionFilter{Limit: 2, After: page2[1].ID})
		if err != nil || more || len(page3) != 1 || page3[0].ID != ids[0] {
			t.Fatal("last", page3, more, err)
		}
		// Before returns the newer neighbors, still newest first.
		back, more, err := repo.ListProviderSuppressions(ctx, app, submission.ProviderSuppressionFilter{Limit: 2, Before: ids[1]})
		if err != nil || !more || len(back) != 2 || back[0].ID != ids[3] || back[1].ID != ids[2] {
			t.Fatal("before", back, more, err)
		}
		// An ID from another app or unknown is invalid, never a leak.
		_, _, err = repo.ListProviderSuppressions(ctx, other, submission.ProviderSuppressionFilter{Limit: 2, After: ids[0]})
		if err != submission.ErrInvalidMessage {
			t.Fatal("cross-app cursor", err)
		}
		if _, _, err = repo.ListProviderSuppressions(ctx, app, submission.ProviderSuppressionFilter{Limit: 2, After: id()}); err != submission.ErrInvalidMessage {
			t.Fatal("unknown cursor", err)
		}
		if _, _, err = repo.ListProviderSuppressions(ctx, app, submission.ProviderSuppressionFilter{Limit: 0}); err == nil {
			t.Fatal("limit 0")
		}
		items, _, _ = repo.ListProviderSuppressions(ctx, other, submission.ProviderSuppressionFilter{Limit: 100})
		if len(items) != 1 || items[0].Origin != "api" {
			t.Fatal("app isolation")
		}
	})

	t.Run("add is api origin, idempotent and audited without addresses", func(t *testing.T) {
		if err := repo.AddProviderSuppressions(ctx, kapp, []string{"New@Example.net", "bounce@example.net", "new@example.net"}); err != nil {
			t.Fatal(err)
		}
		var reason, origin string
		var n int
		if err := owner.QueryRowContext(ctx, `SELECT reason,origin FROM suppressions WHERE app_id=$1 AND address_lower='new@example.net'`, app).Scan(&reason, &origin); err != nil || reason != "manual" || origin != "api" {
			t.Fatal("new entry", reason, origin, err)
		}
		// The first reason wins for an existing address.
		if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM suppressions WHERE app_id=$1 AND address_lower='bounce@example.net' AND reason='hard_bounce' AND origin='recipient'`, app).Scan(&n); err != nil || n != 1 {
			t.Fatal("existing entry changed")
		}
		var audits int
		var details string
		if err := owner.QueryRowContext(ctx, `SELECT count(*), COALESCE(max(details::text),'') FROM audit_events WHERE actor_id=$1 AND action='suppression.added'`, "key:"+key).Scan(&audits, &details); err != nil || audits != 1 || strings.Contains(details, "@") {
			t.Fatal("audit", audits, details, err)
		}
		if err := repo.AddProviderSuppressions(ctx, kapp, []string{"nope"}); err == nil {
			t.Fatal("invalid address accepted")
		}
		if err := repo.AddProviderSuppressions(ctx, submission.App{ID: id(), KeyID: key}, []string{"a@example.net"}); err == nil {
			t.Fatal("unknown app accepted")
		}
	})

	t.Run("delete removes only api and manual entries", func(t *testing.T) {
		got, err := repo.DeleteProviderSuppressions(ctx, kapp, []string{"BOUNCE@example.net", "complaint@example.net", "unsub@example.net", "api@example.net", "manual@example.net", "absent@example.net"})
		want := []bool{false, false, false, true, true, true}
		if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatal("results", got, err)
		}
		var n int
		if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM suppressions WHERE app_id=$1 AND origin='recipient'`, app).Scan(&n); err != nil || n != 3 {
			t.Fatal("recipient entries removed", n)
		}
		var audits int
		var details string
		if err := owner.QueryRowContext(ctx, `SELECT count(*), COALESCE(max(details::text),'') FROM audit_events WHERE actor_id=$1 AND action='suppression.removed'`, "key:"+key).Scan(&audits, &details); err != nil || audits != 2 || strings.Contains(details, "@") {
			t.Fatal("audit", audits, details, err)
		}
		// Another app's identical address is untouched.
		got, err = repo.DeleteProviderSuppressions(ctx, kapp, []string{"bounce@example.net"})
		if err != nil || got[0] {
			t.Fatal("protected again", got, err)
		}
		if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM suppressions WHERE app_id=$1`, other).Scan(&n); err != nil || n != 1 {
			t.Fatal("other app changed", n)
		}
		if _, err = repo.DeleteProviderSuppressions(ctx, submission.App{ID: id(), KeyID: key}, []string{"a@example.net"}); err != submission.ErrInvalidKey {
			t.Fatal("unknown app", err)
		}
	})

	t.Run("suppressed recipient count is app scoped and case insensitive", func(t *testing.T) {
		n, err := repo.SuppressedRecipients(ctx, app, []string{"bounce@example.net", "complaint@example.net", "free@example.net"})
		if err != nil || n != 2 {
			t.Fatal("count", n, err)
		}
		if n, err = repo.SuppressedRecipients(ctx, other, []string{"complaint@example.net"}); err != nil || n != 0 {
			t.Fatal("other app", n, err)
		}
		if _, err = repo.SuppressedRecipients(ctx, app, nil); err == nil {
			t.Fatal("empty list accepted")
		}
	})

	t.Run("revoked key or inactive app writes nothing", func(t *testing.T) {
		var before int
		count := func() int {
			var n int
			if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM suppressions`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		before = count()
		exec(`UPDATE api_keys SET active=false WHERE id=$1`, key)
		if err := repo.AddProviderSuppressions(ctx, kapp, []string{"revoked@example.net"}); err != submission.ErrInvalidKey {
			t.Fatal("add with revoked key", err)
		}
		if _, err := repo.DeleteProviderSuppressions(ctx, kapp, []string{"manual@example.net"}); err != submission.ErrInvalidKey {
			t.Fatal("delete with revoked key", err)
		}
		if count() != before {
			t.Fatal("revoked key wrote")
		}
		exec(`UPDATE api_keys SET active=true WHERE id=$1`, key)
		// A key of another app is not valid for this app.
		if err := repo.AddProviderSuppressions(ctx, submission.App{ID: other, KeyID: key}, []string{"cross@example.net"}); err != submission.ErrInvalidKey {
			t.Fatal("cross-app key", err)
		}
		exec(`UPDATE apps SET active=false WHERE id=$1`, app)
		if err := repo.AddProviderSuppressions(ctx, kapp, []string{"inactive@example.net"}); err != submission.ErrInvalidKey {
			t.Fatal("add with inactive app", err)
		}
		if _, err := repo.DeleteProviderSuppressions(ctx, kapp, []string{"manual@example.net"}); err != submission.ErrInvalidKey {
			t.Fatal("delete with inactive app", err)
		}
		exec(`UPDATE apps SET active=true WHERE id=$1`, app)
		if count() != before {
			t.Fatal("inactive app wrote")
		}
		if err := repo.AddProviderSuppressions(ctx, kapp, []string{"restored@example.net"}); err != nil || count() != before+1 {
			t.Fatal("reactivated key cannot write", err)
		}
	})

	t.Run("closed gate fails closed", func(t *testing.T) {
		exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
		defer exec(`UPDATE runtime_gate SET mode='open' WHERE singleton`)
		if _, _, err := repo.ListProviderSuppressions(ctx, app, submission.ProviderSuppressionFilter{Limit: 1}); err == nil {
			t.Fatal("list with closed gate")
		}
		if err := repo.AddProviderSuppressions(ctx, kapp, []string{"a@example.net"}); err == nil {
			t.Fatal("add with closed gate")
		}
		if _, err := repo.SuppressedRecipients(ctx, app, []string{"a@example.net"}); err == nil {
			t.Fatal("check with closed gate")
		}
	})
}
