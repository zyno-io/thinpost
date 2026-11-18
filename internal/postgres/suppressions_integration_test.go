package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
)

func TestSuppressionsPostgres(t *testing.T) {
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
	schema := "suppressions_" + hex.EncodeToString(random[:])
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
	const generation = "00000000000000000000000000000bbb"
	counter := 0
	id := func() string { counter++; return fmt.Sprintf("%032x", counter+1000) }
	app, other := id(), id()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := owner.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation)
	exec(`INSERT INTO apps(id,name) VALUES ($1,'One'),($2,'Other')`, app, other)
	// message adds a prepared message with one queued delivery per address; a
	// nil address list makes a message without recipient rows (pre-schema-28).
	message := func(addresses ...string) (string, []string) {
		m := id()
		exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,$3,false,'prepared')`, m, app, "source-"+m)
		var ids []string
		for i, address := range addresses {
			d := id()
			ids = append(ids, d)
			exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,$3,$4)`, d, m, i, "signed-"+d)
			lower, domain, rev, _ := recipientKeys(address)
			exec(`INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ($1,$2,$3,$4,$5,$6,$7,'to')`, m, app, i, address, lower, domain, rev)
		}
		return m, ids
	}

	db, cleanup := restrictedRuntimeForTest(t, ctx, owner, u.String())
	defer cleanup()
	repo := &Repository{DB: db, Generation: generation}

	t.Run("grants", func(t *testing.T) {
		// The runtime role can add and delete, and (schema 34) replace only the
		// reason, origin and source of an entry; never its address or app.
		if _, err := db.ExecContext(ctx, `UPDATE suppressions SET address='x@example.com' WHERE false`); err == nil {
			t.Fatal("runtime role may rewrite suppression addresses")
		}
		if _, err := db.ExecContext(ctx, `TRUNCATE suppressions`); err == nil {
			t.Fatal("runtime role may truncate suppressions")
		}
		if _, err := db.ExecContext(ctx, `SELECT suppression_reason FROM webhook_events LIMIT 1`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM webhook_events`); err == nil {
			t.Fatal("runtime role may delete webhook events")
		}
	})

	lease, err := repo.AcquireControllerLease(ctx, "worker", "controller", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var firstID string
	t.Run("admin add exists delete search page", func(t *testing.T) {
		results, err := repo.AddSuppressions(ctx, "operator", app, []string{"Mixed.Case@Example.COM", "b@example.org"})
		if err != nil || len(results) != 2 || results[0].Status != "added" || results[1].Status != "added" || results[0].Suppression.Reason != "manual" || results[0].Suppression.Origin != "console" {
			t.Fatal("add", results, err)
		}
		firstID = results[0].Suppression.ID
		// Case-insensitive duplicate, within and across requests.
		results, err = repo.AddSuppressions(ctx, "operator", app, []string{"mixed.case@example.com", "MIXED.CASE@example.com", "c@example.org"})
		if err != nil || results[0].Status != "exists" || results[1].Status != "exists" || results[2].Status != "added" || results[0].Suppression.ID != firstID || results[0].Suppression.Address != "Mixed.Case@Example.COM" {
			t.Fatal("exists", results, err)
		}
		// App isolation.
		results, err = repo.AddSuppressions(ctx, "operator", other, []string{"b@example.org"})
		if err != nil || results[0].Status != "added" {
			t.Fatal("other app", results, err)
		}
		if _, err = repo.AddSuppressions(ctx, "operator", id(), []string{"x@example.org"}); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("unknown app", err)
		}
		for _, bad := range []string{"", "a", "a@", strings.Repeat("a", 999) + "@example.com", "a\x00@example.com", "a@example.com\xff"} {
			if _, err = repo.AddSuppressions(ctx, "operator", app, []string{bad}); !errors.Is(err, admin.ErrInvalidInput) {
				t.Fatalf("invalid address %q accepted: %v", bad, err)
			}
		}

		page, err := repo.ListSuppressions(ctx, app, admin.SuppressionQuery{Limit: 2})
		if err != nil || len(page.Suppressions) != 2 || page.NextCursor == "" {
			t.Fatal("page one", page, err)
		}
		next, err := repo.ListSuppressions(ctx, app, admin.SuppressionQuery{Limit: 2, Cursor: page.NextCursor})
		if err != nil || len(next.Suppressions) != 1 || next.NextCursor != "" || next.Suppressions[0].ID == page.Suppressions[0].ID || next.Suppressions[0].ID == page.Suppressions[1].ID {
			t.Fatal("page two", next, err)
		}
		if _, err = repo.ListSuppressions(ctx, app, admin.SuppressionQuery{Limit: 2, Cursor: "garbage"}); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatal("bad cursor", err)
		}
		page, err = repo.ListSuppressions(ctx, app, admin.SuppressionQuery{Q: "MIXED", Field: "address", Limit: 10})
		if err != nil || len(page.Suppressions) != 1 || page.Suppressions[0].ID != firstID {
			t.Fatal("address prefix search", page, err)
		}
		page, err = repo.ListSuppressions(ctx, app, admin.SuppressionQuery{Q: "%", Field: "address", Limit: 10})
		if err != nil || len(page.Suppressions) != 0 {
			t.Fatal("LIKE metacharacters must match literally", page, err)
		}
		page, err = repo.ListSuppressions(ctx, app, admin.SuppressionQuery{Q: "example.org", Field: "domain", Limit: 10})
		if err != nil || len(page.Suppressions) != 2 {
			t.Fatal("domain search", page, err)
		}
		page, err = repo.ListSuppressions(ctx, app, admin.SuppressionQuery{Q: "example.org", Field: "domain", Reason: "unsubscribe", Limit: 10})
		if err != nil || len(page.Suppressions) != 0 {
			t.Fatal("reason filter", page, err)
		}

		// Delete: other app's ID and unknown IDs are ignored.
		otherPage, _ := repo.ListSuppressions(ctx, other, admin.SuppressionQuery{Limit: 10})
		if n, err := repo.DeleteSuppressions(ctx, "operator", app, []string{otherPage.Suppressions[0].ID, id()}); err != nil || n != 0 {
			t.Fatal("cross-app delete", n, err)
		}
		if n, err := repo.DeleteSuppressions(ctx, "operator", app, []string{firstID}); err != nil || n != 1 {
			t.Fatal("delete", n, err)
		}
		if n, err := repo.DeleteSuppressions(ctx, "operator", app, []string{firstID}); err != nil || n != 0 {
			t.Fatal("repeat delete", n, err)
		}
		if _, err = repo.DeleteSuppressions(ctx, "operator", app, []string{"mixed.case@example.com"}); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatal("address as ID accepted", err)
		}
		// Re-add after delete works.
		if results, err = repo.AddSuppressions(ctx, "operator", app, []string{"mixed.case@example.com"}); err != nil || results[0].Status != "added" || results[0].Suppression.Address != "mixed.case@example.com" {
			t.Fatal("re-add", results, err)
		}

		// Audit: one row per change, no address anywhere in the row.
		var rows int
		var leaked bool
		if err = owner.QueryRowContext(ctx, `SELECT count(*), COALESCE(bool_or(to_jsonb(a)::text LIKE '%@%' OR to_jsonb(a)::text ILIKE '%example%'),false) FROM audit_events a WHERE action LIKE 'suppression.%'`).Scan(&rows, &leaked); err != nil {
			t.Fatal(err)
		}
		// 2+1 added in the first app, 1 in the other, 1 removed, 1 re-added.
		if rows != 6 || leaked {
			t.Fatal("audit rows", rows, leaked)
		}
		var details string
		if err = owner.QueryRowContext(ctx, `SELECT details::text FROM audit_events WHERE action='suppression.removed' AND resource_id=$1`, firstID).Scan(&details); err != nil || details != `{"origin": "console", "reason": "manual"}` {
			t.Fatal("removed audit details", details, err)
		}
	})

	t.Run("claim enforcement", func(t *testing.T) {
		exec(`DELETE FROM suppressions WHERE app_id=$1`, app)
		exec(`INSERT INTO suppressions(id,app_id,address,address_lower,domain_lower,reason,origin) VALUES ($1,$2,'Blocked@Example.com','blocked@example.com','example.com','spam_complaint','recipient')`, id(), app)
		// Many suppressed deliveries (case-insensitive match), then one clear
		// delivery and one without recipient rows: a single claim call must
		// resolve all suppressed ones and return the clear delivery.
		var suppressed []string
		var messages []string
		for i := 0; i < 5; i++ {
			m, ds := message("BLOCKED@example.COM")
			messages = append(messages, m)
			suppressed = append(suppressed, ds...)
			exec(`UPDATE deliveries SET retry_after=now()-interval '1 hour' WHERE id=$1`, ds[0])
		}
		clearMessage, clearDeliveries := message("clear@example.com")
		exec(`UPDATE deliveries SET retry_after=now()-interval '1 minute' WHERE id=$1`, clearDeliveries[0])
		claimID := id()
		claim, err := repo.ClaimDelivery(ctx, lease, claimID, delivery.EdgeCapabilities{})
		if err != nil || claim.DeliveryID != clearDeliveries[0] || claim.MessageID != clearMessage || claim.AttemptID != claimID {
			t.Fatal("claim must skip suppressed deliveries", claim, err)
		}
		for _, d := range suppressed {
			var state, outcome, reason string
			var code int
			var pending bool
			if err = owner.QueryRowContext(ctx, `SELECT d.state,a.outcome,a.smtp_code,e.suppression_reason,m.pending FROM deliveries d
				JOIN delivery_attempts a ON a.delivery_id=d.id JOIN webhook_events e ON e.attempt_id=a.id JOIN messages m ON m.id=d.message_id WHERE d.id=$1`, d).Scan(&state, &outcome, &code, &reason, &pending); err != nil {
				t.Fatal(err)
			}
			if state != "failed" || outcome != "suppressed" || code != 0 || reason != "spam_complaint" || pending {
				t.Fatal("suppressed delivery", state, outcome, code, reason, pending)
			}
			var eventOutcome, eventState string
			if err = owner.QueryRowContext(ctx, `SELECT outcome,delivery_state FROM webhook_events WHERE delivery_id=$1`, d).Scan(&eventOutcome, &eventState); err != nil || eventOutcome != "suppressed" || eventState != "failed" {
				t.Fatal("event", eventOutcome, eventState, err)
			}
		}
		// The unsuppressed delivery is in flight with no suppression event.
		var state string
		var events int
		if err = owner.QueryRowContext(ctx, `SELECT d.state,(SELECT count(*) FROM webhook_events WHERE delivery_id=d.id) FROM deliveries d WHERE d.id=$1`, claim.DeliveryID).Scan(&state, &events); err != nil || state != "in_flight" || events != 0 {
			t.Fatal("clear delivery", state, events, err)
		}
		// The Edge cannot forge the local outcome.
		forged := delivery.Result{AttemptID: claim.AttemptID, DeliveryID: claim.DeliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch, Outcome: delivery.OutcomeSuppressed}
		if err = repo.CommitDeliveryResult(ctx, forged); err == nil {
			t.Fatal("forged suppressed result accepted")
		}
		forged.Outcome, forged.SuppressionReason = "rejected", "manual"
		if err = repo.CommitDeliveryResult(ctx, forged); err == nil {
			t.Fatal("result carrying a suppression reason accepted")
		}
		if err = repo.CommitDeliveryResult(ctx, delivery.Result{AttemptID: claim.AttemptID, DeliveryID: claim.DeliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch, Outcome: "rejected"}); err != nil {
			t.Fatal(err)
		}

		// Status: every recipient suppressed is "suppressed"; mixed is not.
		page, err := repo.ListMessages(ctx, "operator", app, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		statuses := map[string]admin.MessageSummary{}
		for _, item := range page.Messages {
			statuses[item.ID] = item
		}
		if s := statuses[messages[0]]; s.Status != "suppressed" || s.SuppressedCount != 1 || s.FailedCount != 1 {
			t.Fatal("suppressed status", s.Status, s.SuppressedCount, s.FailedCount)
		}
		mixed, mixedDeliveries := message("blocked@example.com", "ok@example.com")
		exec(`UPDATE deliveries SET retry_after=now()-interval '1 second' WHERE message_id=$1`, mixed)
		nextID := id()
		next, err := repo.ClaimDelivery(ctx, lease, nextID, delivery.EdgeCapabilities{})
		if err != nil || next.DeliveryID != mixedDeliveries[1] || next.AttemptID != nextID {
			t.Fatal("mixed claim", next, err)
		}
		detail, err := repo.GetMessage(ctx, "operator", app, mixed)
		if err != nil || detail.Message.Status == "suppressed" || detail.Message.SuppressedCount != 1 {
			t.Fatal("mixed status", detail.Message.Status, detail.Message.SuppressedCount, err)
		}

		// A pre-schema-28 message has no recipient rows: sent as before.
		legacy, _ := message()
		exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,retry_after) VALUES ($1,$2,0,$3,now()-interval '1 second')`, id(), legacy, "signed-"+legacy)
		legacyClaim, err := repo.ClaimDelivery(ctx, lease, id(), delivery.EdgeCapabilities{})
		if err != nil || legacyClaim.MessageID != legacy {
			t.Fatal("legacy message without recipient rows", legacyClaim, err)
		}
	})

	t.Run("suppression added mid-flight does not affect the attempt", func(t *testing.T) {
		m, ds := message("midflight@example.com")
		exec(`UPDATE deliveries SET retry_after=now()-interval '1 second' WHERE id=$1`, ds[0])
		claim, err := repo.ClaimDelivery(ctx, lease, id(), delivery.EdgeCapabilities{})
		if err != nil || claim.DeliveryID != ds[0] {
			t.Fatal("claim", claim, err)
		}
		if _, err = repo.AddSuppressions(ctx, "operator", app, []string{"midflight@example.com"}); err != nil {
			t.Fatal(err)
		}
		var state string
		if err = owner.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE id=$1`, ds[0]).Scan(&state); err != nil || state != "in_flight" {
			t.Fatal("in-flight attempt changed", state, err)
		}
		_ = m
	})
}

// One claim call resolves at most 32 suppressed deliveries, then yields.
func TestSuppressionClaimPassLimit(t *testing.T) {
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
	schema := "supp_limit_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	u, _ := url.Parse(dsn)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	const generation = "00000000000000000000000000000ddd"
	n := 0
	id := func() string { n++; return fmt.Sprintf("%032x", n+9000) }
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	app := id()
	exec(`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation)
	exec(`INSERT INTO apps(id,name) VALUES ($1,'One')`, app)
	exec(`INSERT INTO suppressions(id,app_id,address,address_lower,domain_lower,reason,origin) VALUES ($1,$2,'blocked@example.net','blocked@example.net','example.net','manual','console')`, id(), app)
	for i := 0; i < 33; i++ {
		m, d := id(), id()
		exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,$3,false,'prepared')`, m, app, "source-"+m)
		exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,retry_after) VALUES ($1,$2,0,$3,now()-$4*interval '1 second')`, d, m, "signed-"+d, 100-i)
		exec(`INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ($1,$2,0,'blocked@example.net','blocked@example.net','example.net','net.example.','to')`, m, app)
	}
	repo := &Repository{DB: db, Generation: generation}
	lease, err := repo.AcquireControllerLease(ctx, "worker", "controller", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ClaimDelivery(ctx, lease, id(), delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrNoWork) {
		t.Fatal("first call", err)
	}
	var queued, failed int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE state='queued'),count(*) FILTER (WHERE state='failed') FROM deliveries`).Scan(&queued, &failed); err != nil || queued != 1 || failed != 32 {
		t.Fatal("pass limit", queued, failed, err)
	}
	if _, err = repo.ClaimDelivery(ctx, lease, id(), delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrNoWork) {
		t.Fatal("second call", err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE state='queued'),count(*) FILTER (WHERE state='failed') FROM deliveries`).Scan(&queued, &failed); err != nil || queued != 0 || failed != 33 {
		t.Fatal("remaining delivery not resolved", queued, failed, err)
	}
}
