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

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/delivery"
)

// Automatic hard-bounce suppression (S5): the insert rule, its precedence over
// existing entries, and everything that must not trigger it.
func TestAutomaticHardBounceSuppressionPostgres(t *testing.T) {
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
	schema := "autobounce_" + hex.EncodeToString(random[:])
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
	const generation = "00000000000000000000000000000eee"
	const journalID = "0123456789abcdef0123456789abcdef"
	counter := 0
	id := func() string { counter++; return fmt.Sprintf("%032x", counter+5000) }
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := owner.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	app, other := id(), id()
	exec(`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation)
	exec(`INSERT INTO apps(id,name) VALUES ($1,'One'),($2,'Other')`, app, other)
	db, cleanup := restrictedRuntimeForTest(t, ctx, owner, u.String())
	defer cleanup()
	repo := &Repository{DB: db, Generation: generation}

	// send creates a message in appID with one in-flight, start-authorized
	// delivery to address (no recipient row when address is empty).
	type sent struct{ message, delivery, attempt string }
	send := func(appID, address string) sent {
		s := sent{id(), id(), id()}
		exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,$3,false,'prepared')`, s.message, appID, "source-"+s.message)
		exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,current_attempt_id) VALUES ($1,$2,0,$3,'in_flight',$4)`, s.delivery, s.message, "signed-"+s.delivery, s.attempt)
		if address != "" {
			lower, domain, rev, _ := recipientKeys(address)
			exec(`INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ($1,$2,0,$3,$4,$5,$6,'to')`, s.message, appID, address, lower, domain, rev)
		}
		exec(`INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized,start_descriptor_hash,start_session_id,start_journal_id) VALUES ($1,$2,'worker',1,true,decode(repeat('ab',32),'hex'),'0123456789abcdef0123456789abcdef',$3)`, s.attempt, s.delivery, journalID)
		return s
	}
	result := func(s sent, outcome string, code int, stage, enhanced string) delivery.Result {
		return delivery.Result{AttemptID: s.attempt, DeliveryID: s.delivery, WorkerID: "worker", JournalID: journalID, Epoch: 1, Outcome: outcome, SMTPCode: code, SMTPStage: stage, SMTPEnhanced: enhanced}
	}
	type entry struct{ reason, origin, source, address string }
	lookup := func(appID, lower string) (entry, bool) {
		var e entry
		var source sql.NullString
		err := owner.QueryRow(`SELECT reason,origin,source_message_id,address FROM suppressions WHERE app_id=$1 AND address_lower=$2`, appID, lower).Scan(&e.reason, &e.origin, &source, &e.address)
		if err == sql.ErrNoRows {
			return e, false
		}
		if err != nil {
			t.Fatal(err)
		}
		e.source = source.String
		return e, true
	}
	var state string
	deliveryState := func(s sent) string {
		t.Helper()
		if err := owner.QueryRow(`SELECT state FROM deliveries WHERE id=$1`, s.delivery).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}

	t.Run("RCPT 5.1.1 and 5.1.10 suppress", func(t *testing.T) {
		for i, enhanced := range []string{"5.1.1", "5.1.10"} {
			address := fmt.Sprintf("Gone%d@Example.COM", i)
			s := send(app, address)
			committed, err := repo.CommitDeliveryResultIdentified(ctx, result(s, "rejected", 550, "RCPT", enhanced))
			if err != nil || committed.Replayed {
				t.Fatal(enhanced, committed, err)
			}
			got, ok := lookup(app, strings.ToLower(address))
			if !ok || got.reason != "hard_bounce" || got.origin != "recipient" || got.source != s.message || got.address != address {
				t.Fatalf("%s: %+v %v", enhanced, got, ok)
			}
			if deliveryState(s) != "failed" {
				t.Fatal("delivery state", state)
			}
			var stage, recorded sql.NullString
			if err = owner.QueryRow(`SELECT smtp_stage,smtp_enhanced FROM delivery_attempts WHERE id=$1`, s.attempt).Scan(&stage, &recorded); err != nil || stage.String != "RCPT" || recorded.String != enhanced {
				t.Fatal("attempt columns", stage, recorded, err)
			}
			// The suppression is per app.
			if _, ok = lookup(other, strings.ToLower(address)); ok {
				t.Fatal("suppression leaked to another app")
			}
		}
	})

	t.Run("non triggers", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			outcome  string
			code     int
			stage    string
			enhanced string
			state    string
		}{
			{"MAIL stage", "rejected", 550, "MAIL", "5.1.1", "failed"},
			{"DATA stage", "rejected", 550, "DATA", "5.1.1", "failed"},
			{"no stage", "rejected", 550, "", "5.1.1", "failed"},
			{"no enhanced code", "rejected", 550, "RCPT", "", "failed"},
			{"other enhanced code", "rejected", 550, "RCPT", "5.5.0", "failed"},
			{"5.1.2 bad domain", "rejected", 550, "RCPT", "5.1.2", "failed"},
			{"5.2.1 mailbox disabled", "rejected", 550, "RCPT", "5.2.1", "failed"},
			{"4xx with 5.1.1", "rejected", 450, "RCPT", "5.1.1", "queued"},
			{"route rejected", "route_rejected", 550, "RCPT", "5.1.1", "queued"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				address := fmt.Sprintf("nt%d@example.net", counter)
				s := send(app, address)
				if err := repo.CommitDeliveryResult(ctx, result(s, tc.outcome, tc.code, tc.stage, tc.enhanced)); err != nil {
					t.Fatal(err)
				}
				if got, ok := lookup(app, address); ok {
					t.Fatalf("suppressed: %+v", got)
				}
				if deliveryState(s) != tc.state {
					t.Fatal("state", state)
				}
			})
		}
		t.Run("accepted", func(t *testing.T) {
			s := send(app, "fine@example.net")
			if err := repo.CommitDeliveryResult(ctx, result(s, "accepted", 250, "RCPT", "5.1.1")); err != nil {
				t.Fatal(err)
			}
			if _, ok := lookup(app, "fine@example.net"); ok {
				t.Fatal("accepted delivery suppressed")
			}
			var stage sql.NullString
			if err := owner.QueryRow(`SELECT smtp_stage FROM delivery_attempts WHERE id=$1`, s.attempt).Scan(&stage); err != nil || stage.Valid {
				t.Fatal("stage recorded for a 2xx result", stage, err)
			}
		})
		t.Run("null_mx and domain_not_found", func(t *testing.T) {
			for _, outcome := range []string{delivery.OutcomeNullMX, delivery.OutcomeDomainNotFound} {
				s := send(app, outcome+"@example.net")
				r := result(s, outcome, 0, "RCPT", "5.1.1")
				r.JournalID = ""
				exec(`UPDATE delivery_attempts SET start_authorized=false,start_descriptor_hash=NULL,start_session_id=NULL,start_journal_id=NULL WHERE id=$1`, s.attempt)
				if err := repo.CommitDeliveryResult(ctx, r); err != nil {
					t.Fatal(err)
				}
				if _, ok := lookup(app, outcome+"@example.net"); ok {
					t.Fatal(outcome, "suppressed")
				}
			}
		})
		t.Run("message without recipient rows", func(t *testing.T) {
			s := send(app, "")
			if err := repo.CommitDeliveryResult(ctx, result(s, "rejected", 550, "RCPT", "5.1.1")); err != nil {
				t.Fatal(err)
			}
			if deliveryState(s) != "failed" {
				t.Fatal("state", state)
			}
		})
	})

	t.Run("precedence over existing entries", func(t *testing.T) {
		for _, tc := range []struct {
			name, reason, origin   string
			wantReason, wantOrigin string
			upgraded               bool
		}{
			{"manual console is replaced", "manual", "console", "hard_bounce", "recipient", true},
			{"manual api is replaced", "manual", "api", "hard_bounce", "recipient", true},
			{"hard_bounce stays", "hard_bounce", "recipient", "hard_bounce", "recipient", false},
			{"spam_complaint wins", "spam_complaint", "recipient", "spam_complaint", "recipient", false},
			{"unsubscribe wins", "unsubscribe", "recipient", "unsubscribe", "recipient", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				address := fmt.Sprintf("p%d@example.org", counter)
				existing := id()
				exec(`INSERT INTO suppressions(id,app_id,address,address_lower,domain_lower,reason,origin) VALUES ($1,$2,$3,$3,'example.org',$4,$5)`, existing, app, address, tc.reason, tc.origin)
				s := send(app, strings.ToUpper(address))
				if err := repo.CommitDeliveryResult(ctx, result(s, "rejected", 550, "RCPT", "5.1.1")); err != nil {
					t.Fatal(err)
				}
				got, _ := lookup(app, address)
				var rowID string
				if err := owner.QueryRow(`SELECT id FROM suppressions WHERE app_id=$1 AND address_lower=$2`, app, address).Scan(&rowID); err != nil || rowID != existing {
					t.Fatal("row identity changed", rowID, err)
				}
				wantSource := ""
				if tc.upgraded {
					wantSource = s.message
				}
				if got.reason != tc.wantReason || got.origin != tc.wantOrigin || got.source != wantSource {
					t.Fatalf("%+v", got)
				}
				// Upgrading keeps the stored address as first written.
				if got.address != address {
					t.Fatal("address rewritten")
				}
			})
		}
	})

	t.Run("replay does not re-add a removed entry", func(t *testing.T) {
		s := send(app, "replay@example.com")
		r := result(s, "rejected", 550, "RCPT", "5.1.1")
		if err := repo.CommitDeliveryResult(ctx, r); err != nil {
			t.Fatal(err)
		}
		exec(`DELETE FROM suppressions WHERE app_id=$1 AND address_lower='replay@example.com'`, app)
		committed, err := repo.CommitDeliveryResultIdentified(ctx, r)
		if err != nil || !committed.Replayed {
			t.Fatal("replay", committed, err)
		}
		if _, ok := lookup(app, "replay@example.com"); ok {
			t.Fatal("replay re-added a removed suppression")
		}
	})

	t.Run("claim then enforces the new entry", func(t *testing.T) {
		lease, err := repo.AcquireControllerLease(ctx, "worker2", "controller", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		m, d := id(), id()
		exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,$3,false,'prepared')`, m, app, "source-"+m)
		exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,retry_after) VALUES ($1,$2,0,$3,now()-interval '1 hour')`, d, m, "signed-"+d)
		exec(`INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ($1,$2,0,'gone0@example.com','gone0@example.com','example.com','com.example.','to')`, m, app)
		if _, err = repo.ClaimDelivery(ctx, lease, id(), delivery.EdgeCapabilities{}); err == nil {
			t.Fatal("a delivery to an automatically suppressed recipient was claimed")
		}
		if got := func() string {
			var outcome string
			_ = owner.QueryRow(`SELECT outcome FROM delivery_attempts WHERE delivery_id=$1`, d).Scan(&outcome)
			return outcome
		}(); got != "suppressed" {
			t.Fatal("outcome", got)
		}
	})

	t.Run("runtime role may update only the precedence columns", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `UPDATE suppressions SET reason='manual',origin='console',source_message_id=NULL WHERE false`); err != nil {
			t.Fatal("precedence columns", err)
		}
		for _, column := range []string{"address='x@example.com'", "address_lower='x@example.com'", "domain_lower='x.example'", "app_id='x'", "created_at=now()"} {
			if _, err := db.ExecContext(ctx, `UPDATE suppressions SET `+column+` WHERE false`); err == nil {
				t.Fatal("runtime role may update", column)
			}
		}
	})
}
