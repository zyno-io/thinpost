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
)

func TestOperationsAndTrackingEngagementPostgres(t *testing.T) {
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
	schema := "operations_" + hex.EncodeToString(random[:])
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
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := owner.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	app, other := id(), id()
	exec(`UPDATE runtime_gate SET generation=$1,mode='closed',changed_by='deploy:ns/rel:r1' WHERE singleton`, generation)
	exec(`INSERT INTO apps(id,name) VALUES ($1,'One'),($2,'Other')`, app, other)
	db, cleanup := restrictedRuntimeForTest(t, ctx, owner, u.String())
	defer cleanup()
	repo := &Repository{DB: db, Generation: generation}

	msg := func(owner string, state string, pending bool, prepareNext string) string {
		m := id()
		exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state,pending,prepare_next_at) VALUES ($1,$2,$3,false,$4,$5,`+prepareNext+`)`, m, owner, "source-"+m, state, pending)
		return m
	}
	deliver := func(m, state, retry string, index int) string {
		d := id()
		exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,retry_after) VALUES ($1,$2,$3,$4,$5,`+retry+`)`, d, m, index, "signed-"+d, state)
		return d
	}
	t.Run("empty snapshot works while the gate is closed", func(t *testing.T) {
		ops, err := repo.Operations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ops.Gate.Mode != "closed" || !ops.Gate.GenerationMatches || ops.Gate.ChangedBy == nil || *ops.Gate.ChangedBy != "deploy:ns/rel:r1" || ops.GeneratedAt.IsZero() {
			t.Fatal("gate", ops.Gate)
		}
		if ops.Messages != (admin.MessageBacklog{}) || ops.Deliveries != (admin.DeliveryBacklog{}) || ops.Webhooks != (admin.WebhookBacklog{}) || len(ops.Workers) != 0 {
			t.Fatal("empty database reports backlog", ops)
		}
		if ops.RetryPolicy.Revision != 1 || ops.RetryPolicy.MaxAttempts != 128 {
			t.Fatal("retry policy", ops.RetryPolicy)
		}
		if _, err = repo.SetRetryPolicy(ctx, "operator", admin.RetryPolicyUpdate{MaxAttempts: 5, InitialDelayMS: 60000, MaxDelayMS: 60000, MaxAgeMS: 3600000, ExpectedRevision: 1}); !errors.Is(err, ErrRuntimeClosed) {
			t.Fatal("mutation allowed while closed", err)
		}
		wrong := &Repository{DB: db, Generation: "00000000000000000000000000000ddd"}
		if ops, err = wrong.Operations(ctx); err != nil || ops.Gate.GenerationMatches {
			t.Fatal("generation mismatch not reported", err)
		}
	})

	// Backlog fixture: counts by state plus identifiers that must not leak.
	exec(`INSERT INTO controller_leases(worker_id,controller_id,epoch,expires_at) VALUES ('w1','c1',3,clock_timestamp()+interval '1 minute'),('w2','c2',1,clock_timestamp()-interval '1 minute')`)
	msg(app, "queued", true, "clock_timestamp()-interval '10 minutes'")
	msg(app, "queued", true, "clock_timestamp()+interval '1 hour'")
	msg(app, "preparing", true, "clock_timestamp()")
	failedMsg := msg(app, "preparation_failed", true, "clock_timestamp()")
	holdMsg := msg(other, "recovery_hold", true, "clock_timestamp()")
	done := msg(app, "prepared", false, "clock_timestamp()")
	deliver(done, "accepted", "clock_timestamp()", 0)
	deliver(done, "failed", "clock_timestamp()", 1)
	active := msg(app, "prepared", true, "clock_timestamp()")
	deliver(active, "queued", "clock_timestamp()-interval '5 minutes'", 0)
	deliver(active, "queued", "clock_timestamp()+interval '2 hours'", 1)
	deliver(active, "in_flight", "clock_timestamp()", 2)
	uncertain := deliver(active, "uncertain", "clock_timestamp()", 3)
	policyHeld := deliver(active, "recipient_policy_held", "clock_timestamp()", 4)
	trackHeld := deliver(active, "tracking_held", "clock_timestamp()", 5)
	deliver(active, "recovery_hold", "clock_timestamp()", 6)
	sub := id()
	exec(`INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext,active) VALUES ($1,$2,'https://hooks.example/','\x01'::bytea,true)`, sub, app)
	for i, state := range []string{"queued", "queued", "in_flight", "recovery_hold", "delivered", "failed"} {
		event := id()
		exec(`INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ($1,$2,$3,'preparation_failed',0)`, event, app, done)
		next := "clock_timestamp()-interval '3 minutes'"
		if i == 1 {
			next = "clock_timestamp()+interval '1 hour'"
		}
		exec(`INSERT INTO webhook_deliveries(event_id,subscription_id,app_id,state,next_attempt_at) VALUES ($1,$2,$3,$4,`+next+`)`, event, sub, app, state)
	}
	t.Run("backlog counts", func(t *testing.T) {
		ops, err := repo.Operations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m, d, w := ops.Messages, ops.Deliveries, ops.Webhooks
		if m.QueuedDue != 1 || m.QueuedWaiting != 1 || m.Preparing != 1 || m.PreparationFailed != 1 || m.RecoveryHold != 1 || m.OldestDueAt == nil {
			t.Fatal("messages", m)
		}
		if d.QueuedDue != 1 || d.QueuedWaiting != 1 || d.InFlight != 1 || d.Uncertain != 1 || d.RecipientPolicyHeld != 1 || d.TrackingHeld != 1 || d.RecoveryHold != 1 || d.OldestDueAt == nil {
			t.Fatal("deliveries", d)
		}
		if w.QueuedDue != 1 || w.QueuedWaiting != 1 || w.InFlight != 1 || w.RecoveryHold != 1 || w.OldestDueAt == nil {
			t.Fatal("webhooks", w)
		}
		if len(ops.Workers) != 2 || !ops.Workers[0].Live || ops.Workers[1].Live || ops.Workers[0].Epoch != 3 {
			t.Fatal("workers", ops.Workers)
		}
	})
	t.Run("new indexes exist", func(t *testing.T) {
		// Planner choice on a tiny table is not guaranteed, so only the definitions are checked.
		var defs string
		if err := owner.QueryRowContext(ctx, `SELECT string_agg(indexdef,' ') FROM pg_indexes WHERE schemaname=current_schema() AND indexname IN ('webhook_deliveries_unfinished_idx','tracking_links_delivery_idx')`).Scan(&defs); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(defs, "state = ANY") || !strings.Contains(defs, "tracking_links USING btree (delivery_id)") {
			t.Fatal("indexes missing", defs)
		}
		tx, err := owner.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SET LOCAL enable_seqscan=off`); err != nil {
			t.Fatal(err)
		}
		for index, query := range map[string]string{
			"webhook_deliveries_unfinished_idx": `SELECT count(*) FROM webhook_deliveries WHERE state IN ('queued','in_flight','recovery_hold')`,
			"tracking_links_delivery_idx":       `SELECT count(*) FROM tracking_links WHERE delivery_id='x'`,
		} {
			rows, err := tx.QueryContext(ctx, `EXPLAIN `+query)
			if err != nil {
				t.Fatal(err)
			}
			var plan strings.Builder
			for rows.Next() {
				var line string
				if err = rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				plan.WriteString(line)
			}
			rows.Close()
			if !strings.Contains(plan.String(), index) {
				t.Fatal("query does not match its index", index, plan.String())
			}
		}
	})
	t.Run("attention pages across apps without addresses", func(t *testing.T) {
		page, err := repo.Attention(ctx, "", 3)
		if err != nil || len(page.Items) != 3 || page.NextCursor == "" {
			t.Fatal("first page", page, err)
		}
		all := page.Items
		for page.NextCursor != "" {
			if page, err = repo.Attention(ctx, page.NextCursor, 3); err != nil {
				t.Fatal(err)
			}
			all = append(all, page.Items...)
		}
		kinds := map[string]int{}
		seen := map[string]bool{}
		for _, item := range all {
			kinds[item.Kind]++
			key := item.MessageID + "/" + item.DeliveryID
			if seen[key] {
				t.Fatal("duplicate item", key)
			}
			seen[key] = true
			if item.AppName == "" || item.Since.IsZero() {
				t.Fatal("incomplete item", item)
			}
		}
		if len(all) != 6 || kinds["uncertain"] != 1 || kinds["recipient_policy_held"] != 1 || kinds["tracking_held"] != 1 || kinds["preparation_failed"] != 1 || kinds["recovery_hold"] != 2 {
			t.Fatal("unexpected attention set", kinds)
		}
		if !seen[failedMsg+"/"] || !seen[holdMsg+"/"] || !seen[active+"/"+uncertain] || !seen[active+"/"+policyHeld] || !seen[active+"/"+trackHeld] {
			t.Fatal("missing items", seen)
		}
		if _, err = repo.Attention(ctx, "bad", 3); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatal("bad cursor accepted", err)
		}
		if _, err = repo.Attention(ctx, "", 0); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatal("bad limit accepted", err)
		}
	})
	t.Run("operations reads are read-only", func(t *testing.T) {
		tx, err := repo.beginOperationsRead(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `UPDATE delivery_retry_policy SET max_attempts=2`); err == nil {
			t.Fatal("operations transaction can write")
		}
	})

	exec(`UPDATE runtime_gate SET mode='open' WHERE singleton`)
	t.Run("per-delivery tracking engagement", func(t *testing.T) {
		m := msg(app, "prepared", true, "clock_timestamp()")
		tracked := deliver(m, "accepted", "clock_timestamp()", 0)
		plain := deliver(m, "accepted", "clock_timestamp()", 1)
		empty := deliver(m, "accepted", "clock_timestamp()", 2)
		exec(`INSERT INTO tracking_objects(delivery_id,object_key,expires_at,event_limit) VALUES ($1,$2,clock_timestamp()+interval '1 day',10),($3,$4,clock_timestamp()+interval '1 day',10)`,
			tracked, "tracking/v1/0000000000/"+tracked, empty, "tracking/v1/0000000000/"+empty)
		open, click1, click2, emptyOpen := id(), id(), id(), id()
		exec(`INSERT INTO tracking_links(id,token_hash,delivery_id,kind,event_count) VALUES
			($1,decode(repeat('01',32),'hex'),$2,'open',2),($3,decode(repeat('02',32),'hex'),$2,'click',1),($4,decode(repeat('03',32),'hex'),$2,'click',3),($5,decode(repeat('04',32),'hex'),$6,'open',0)`,
			open, tracked, click1, click2, emptyOpen, empty)
		exec(`INSERT INTO tracking_events(id,link_id,delivery_id,kind,outcome,occurred_at) VALUES
			($1,$2,$3,'open','opened','2026-01-01T00:00:00Z'),($4,$5,$3,'click','clicked','2026-01-03T00:00:00Z'),($6,$7,$3,'click','clicked','2026-01-02T00:00:00Z')`,
			id(), open, tracked, id(), click1, id(), click2)
		detail, err := repo.GetMessage(ctx, "operator", app, m)
		if err != nil {
			t.Fatal(err)
		}
		if len(detail.Deliveries) != 3 {
			t.Fatal("deliveries", detail.Deliveries)
		}
		byID := map[string]admin.DeliverySummary{}
		for _, d := range detail.Deliveries {
			byID[d.ID] = d
		}
		tr := byID[tracked].Tracking
		if tr == nil || tr.Opens != 2 || tr.Clicks != 4 || tr.LastEventAt == nil || !tr.LastEventAt.Equal(time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)) {
			t.Fatal("tracked engagement", tr)
		}
		if byID[plain].Tracking != nil {
			t.Fatal("untracked delivery reports tracking")
		}
		if e := byID[empty].Tracking; e == nil || e.Opens != 0 || e.Clicks != 0 || e.LastEventAt != nil {
			t.Fatal("tracked delivery without events", e)
		}
	})
}
