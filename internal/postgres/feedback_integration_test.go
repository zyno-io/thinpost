package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/tracking"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

func (f *webhookFixture) deliveryState(delivery string) string {
	f.t.Helper()
	var state string
	if err := f.owner.QueryRowContext(f.ctx, `SELECT state FROM deliveries WHERE id=$1`, delivery).Scan(&state); err != nil {
		f.t.Fatal(err)
	}
	return state
}

// suppression returns reason and origin of the app's entry for the address, or "" when none.
func (f *webhookFixture) suppression(app, address string) (reason, origin, source string) {
	f.t.Helper()
	var src sql.NullString
	err := f.owner.QueryRowContext(f.ctx, `SELECT reason,origin,source_message_id FROM suppressions WHERE app_id=$1 AND address_lower=$2`, app, strings.ToLower(address)).Scan(&reason, &origin, &src)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return reason, origin, src.String
}

func TestRecipientFeedbackPostgres(t *testing.T) {
	f := newWebhookFixture(t, "feedback_")
	ctx := f.ctx
	app, other := f.app("feedback"), f.app("other")
	// Subscriptions: native (all events), Postmark and Mailgun (default: all of
	// their events) and Resend (cannot represent the event).
	native := f.create(app, "https://native.example.com/")
	pm := f.createFormat(app, "postmark", "https://pm.example.com/", nil, false, &admin.SealedWebhookAuth{Ciphertext: []byte("a"), Summary: admin.WebhookAuthSummary{Username: "u"}})
	rs := f.createFormat(app, "resend", "https://rs.example.com/", nil, true, nil)
	mg := f.createFormat(app, "mailgun", "https://mg.example.com/", []string{"unsubscribed"}, true, nil)
	pmOther := f.createFormat(app, "postmark", "https://pm2.example.com/", []string{"Delivery"}, false, &admin.SealedWebhookAuth{Ciphertext: []byte("a"), Summary: admin.WebhookAuthSummary{Username: "u"}})

	t.Run("runtime grants", func(t *testing.T) {
		for _, statement := range []string{
			`UPDATE recipient_feedback SET kind='unsubscribe'`, `DELETE FROM recipient_feedback`, `TRUNCATE recipient_feedback`,
			`UPDATE suppressions SET address='x@example.com'`, `UPDATE suppressions SET address_lower='x@example.com'`, `UPDATE suppressions SET app_id='x'`, `UPDATE suppressions SET created_at=now()`, `TRUNCATE suppressions`,
		} {
			if _, err := f.repo.DB.ExecContext(ctx, statement); err == nil {
				t.Fatalf("runtime permitted %q", statement)
			}
		}
		for _, statement := range []string{`SELECT * FROM recipient_feedback`, `UPDATE suppressions SET reason=reason,origin=origin,source_message_id=source_message_id`} {
			if _, err := f.repo.DB.ExecContext(ctx, statement); err != nil {
				t.Fatalf("runtime refused %q: %v", statement, err)
			}
		}
	})

	t.Run("a one-click POST commits feedback, suppression and event atomically", func(t *testing.T) {
		message, deliveries, _ := f.message(app, true, "secret subject")
		before := f.deliveryState(deliveries[0])
		if err := f.repo.RecordUnsubscribe(ctx, deliveries[0]); err != nil {
			t.Fatal(err)
		}
		var feedbackID, kind, channel string
		var reporter sql.NullString
		if err := f.owner.QueryRowContext(ctx, `SELECT id,kind,channel,reporter_domain FROM recipient_feedback WHERE delivery_id=$1`, deliveries[0]).Scan(&feedbackID, &kind, &channel, &reporter); err != nil || kind != "unsubscribe" || channel != "one_click" || reporter.Valid {
			t.Fatal("feedback row", kind, channel, err)
		}
		if reason, origin, source := f.suppression(app, "first@example.net"); reason != "unsubscribe" || origin != "recipient" || source != message {
			t.Fatal("suppression", reason, origin, source)
		}
		if reason, _, _ := f.suppression(app, "second@example.org"); reason != "" {
			t.Fatal("another recipient of the message was suppressed")
		}
		if reason, _, _ := f.suppression(other, "first@example.net"); reason != "" {
			t.Fatal("another app was suppressed")
		}
		var eventID, outcome string
		var code int
		var attempt, state sql.NullString
		if err := f.owner.QueryRowContext(ctx, `SELECT id,outcome,smtp_code,attempt_id,delivery_state FROM webhook_events WHERE feedback_id=$1`, feedbackID).Scan(&eventID, &outcome, &code, &attempt, &state); err != nil || eventID != feedbackID || outcome != "unsubscribed" || code != 0 || attempt.Valid || state.Valid {
			t.Fatal("event", eventID, outcome, err)
		}
		if f.deliveryState(deliveries[0]) != before {
			t.Fatal("delivery state changed")
		}
		queued := f.queued(eventID)
		if len(queued) != 3 || !queued[native.ID] || !queued[pm.ID] || !queued[mg.ID] || queued[rs.ID] || queued[pmOther.ID] {
			t.Fatal("fan-out", queued)
		}
		// A repeat, and many concurrent repeats, change nothing.
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- f.repo.RecordUnsubscribe(ctx, deliveries[0]) }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal("duplicate", err)
			}
		}
		if f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, deliveries[0]) != 1 || f.count(`SELECT count(*) FROM webhook_events WHERE delivery_id=$1 AND outcome='unsubscribed'`, deliveries[0]) != 1 ||
			f.count(`SELECT count(*) FROM webhook_deliveries WHERE event_id=$1`, eventID) != 3 {
			t.Fatal("a duplicate created rows")
		}
		// The webhook is claimable: Postmark renders a SubscriptionChange with the recipient.
		for i := 0; i < 3; i++ {
			claim, err := f.repo.ClaimWebhook(ctx, f.id(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if claim.Event.Outcome != "unsubscribed" || claim.Event.AttemptID != "" || claim.Event.DeliveryID != deliveries[0] {
				t.Fatal("claim", claim.Event)
			}
			if claim.Format == format.Postmark {
				impl, _ := format.Lookup(format.Postmark)
				req, err := impl.Build(format.Input{Event: claim.Event, Context: claim.Context, Now: time.Now()})
				if err != nil || !strings.Contains(string(req.Body), `"RecordType":"SubscriptionChange"`) || !strings.Contains(string(req.Body), `"Recipient":"first@example.net"`) || strings.Contains(string(req.Body), "secret subject") {
					t.Fatal("postmark body", string(req.Body), err)
				}
			}
		}
	})

	t.Run("precedence of recipient-originated entries", func(t *testing.T) {
		seed := func(address, reason, origin string) {
			f.exec(`INSERT INTO suppressions(id,app_id,address,address_lower,domain_lower,reason,origin) VALUES ($1,$2,$3,lower($3),split_part(lower($3),'@',2),$4,$5)`, f.id(), app, address, reason, origin)
		}
		for _, tc := range []struct{ name, reason, origin, wantReason, wantOrigin string }{
			{"manual console", "manual", "console", "unsubscribe", "recipient"},
			{"manual api", "manual", "api", "unsubscribe", "recipient"},
			{"hard bounce", "hard_bounce", "recipient", "unsubscribe", "recipient"},
			{"existing unsubscribe", "unsubscribe", "recipient", "unsubscribe", "recipient"},
			{"spam complaint outranks", "spam_complaint", "recipient", "spam_complaint", "recipient"},
		} {
			address := "Prec." + strings.ReplaceAll(tc.name, " ", ".") + "@Example.NET"
			message := f.id()
			delivery := f.id()
			f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'k'||$1,false,'prepared')`, message, app)
			f.exec(`INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ($1,$2,0,$3,lower($3),'example.net','net.example.','to')`, message, app, address)
			f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'f'||$1)`, delivery, message)
			seed(strings.ToLower(address), tc.reason, tc.origin)
			if err := f.repo.RecordUnsubscribe(ctx, delivery); err != nil {
				t.Fatal(tc.name, err)
			}
			reason, origin, _ := f.suppression(app, address)
			if reason != tc.wantReason || origin != tc.wantOrigin || f.count(`SELECT count(*) FROM suppressions WHERE app_id=$1 AND address_lower=lower($2)`, app, address) != 1 {
				t.Fatal(tc.name, reason, origin)
			}
			// Feedback and the event are recorded even when the entry is not replaced.
			if f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, delivery) != 1 || f.count(`SELECT count(*) FROM webhook_events WHERE delivery_id=$1`, delivery) != 1 {
				t.Fatal(tc.name, "feedback or event missing")
			}
		}
	})

	t.Run("a repeat POST after a console removal suppresses again without a second event", func(t *testing.T) {
		message, delivery := f.id(), f.id()
		f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'k'||$1,false,'prepared')`, message, app)
		f.exec(`INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ($1,$2,0,'again@example.net','again@example.net','example.net','net.example.','to')`, message, app)
		f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'f'||$1)`, delivery, message)
		if err := f.repo.RecordUnsubscribe(ctx, delivery); err != nil {
			t.Fatal(err)
		}
		var id string
		if err := f.owner.QueryRowContext(ctx, `SELECT id FROM suppressions WHERE app_id=$1 AND address_lower='again@example.net'`, app).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if n, err := f.repo.DeleteSuppressions(ctx, "operator", app, []string{id}); err != nil || n != 1 {
			t.Fatal("console delete", n, err)
		}
		if reason, _, _ := f.suppression(app, "again@example.net"); reason != "" {
			t.Fatal("entry not removed")
		}
		if err := f.repo.RecordUnsubscribe(ctx, delivery); err != nil {
			t.Fatal(err)
		}
		if reason, origin, source := f.suppression(app, "again@example.net"); reason != "unsubscribe" || origin != "recipient" || source != message {
			t.Fatal("repeat did not suppress again", reason, origin)
		}
		if f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, delivery) != 1 || f.count(`SELECT count(*) FROM webhook_events WHERE delivery_id=$1`, delivery) != 1 {
			t.Fatal("repeat created a second feedback row or event")
		}
	})

	t.Run("missing delivery or recipient is not found and writes nothing", func(t *testing.T) {
		before := f.count(`SELECT count(*) FROM recipient_feedback`)
		if err := f.repo.RecordUnsubscribe(ctx, f.id()); !errors.Is(err, tracking.ErrNotFound) {
			t.Fatal("unknown delivery", err)
		}
		if err := f.repo.RecordUnsubscribe(ctx, "not-an-id"); !errors.Is(err, tracking.ErrNotFound) {
			t.Fatal("malformed delivery", err)
		}
		// A message from before schema 28 has no recipient rows.
		message, delivery := f.id(), f.id()
		f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'k'||$1,false,'prepared')`, message, app)
		f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'f'||$1)`, delivery, message)
		if err := f.repo.RecordUnsubscribe(ctx, delivery); !errors.Is(err, tracking.ErrNotFound) {
			t.Fatal("pre-schema-28 delivery", err)
		}
		if f.count(`SELECT count(*) FROM recipient_feedback`) != before {
			t.Fatal("rows written for a missing recipient")
		}
	})

	t.Run("a paused app still records, a closed gate does not", func(t *testing.T) {
		paused := f.app("paused")
		message, delivery := f.id(), f.id()
		f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'k'||$1,false,'prepared')`, message, paused)
		f.exec(`INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ($1,$2,0,'p@example.net','p@example.net','example.net','net.example.','to')`, message, paused)
		f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'f'||$1)`, delivery, message)
		f.exec(`UPDATE apps SET active=false WHERE id=$1`, paused)
		f.exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
		err := f.repo.RecordUnsubscribe(ctx, delivery)
		f.exec(`UPDATE runtime_gate SET mode='open' WHERE singleton`)
		if !errors.Is(err, ErrRuntimeClosed) || f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, delivery) != 0 {
			t.Fatal("closed gate", err)
		}
		if err = f.repo.RecordUnsubscribe(ctx, delivery); err != nil {
			t.Fatal("paused app", err)
		}
		if reason, _, _ := f.suppression(paused, "p@example.net"); reason != "unsubscribe" {
			t.Fatal("paused app did not record")
		}
	})

	t.Run("the real tracking handler over the repository", func(t *testing.T) {
		keys, err := tracking.NewLinkKeys([]byte("0123456789abcdef0123456789abcdef"), nil)
		if err != nil {
			t.Fatal(err)
		}
		const origin = "https://track.example.com"
		h := &tracking.Handler{Origin: origin, Links: keys, Unsubscribes: f.repo}
		_, deliveries, _ := f.message(app, false, "s")
		link, err := keys.UnsubscribeLink(origin, deliveries[1])
		if err != nil {
			t.Fatal(err)
		}
		do := func(method, target, body string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, target, strings.NewReader(body))
			if body != "" {
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			return w
		}
		path := strings.TrimPrefix(link, origin)
		// GET and HEAD never write.
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			if w := do(method, path, ""); w.Code != 200 || f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, deliveries[1]) != 0 {
				t.Fatal(method, "wrote or failed", w.Code)
			}
		}
		// A tampered signature is a 404 before storage.
		tampered := path[:len(path)-2] + map[bool]string{true: "AA", false: "BB"}[!strings.HasSuffix(path, "AA")]
		if w := do(http.MethodPost, tampered, "List-Unsubscribe=One-Click"); w.Code != 404 || f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, deliveries[1]) != 0 {
			t.Fatal("tampered POST", w.Code)
		}
		// A signature for a delivery that does not exist is a 404 after storage.
		ghost, _ := keys.UnsubscribeLink(origin, f.id())
		if w := do(http.MethodPost, strings.TrimPrefix(ghost, origin), "List-Unsubscribe=One-Click"); w.Code != 404 {
			t.Fatal("missing delivery", w.Code)
		}
		// A closed gate is a 503 that records nothing; reopening makes the retry work.
		f.exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
		w := do(http.MethodPost, path, "List-Unsubscribe=One-Click")
		f.exec(`UPDATE runtime_gate SET mode='open' WHERE singleton`)
		if w.Code != 503 || f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, deliveries[1]) != 0 {
			t.Fatal("closed gate POST", w.Code)
		}
		for i := 0; i < 2; i++ {
			if w = do(http.MethodPost, path, "List-Unsubscribe=One-Click"); w.Code != 200 || !strings.Contains(w.Body.String(), "unsubscribed") {
				t.Fatal("POST", i, w.Code)
			}
		}
		if f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, deliveries[1]) != 1 {
			t.Fatal("duplicate POST wrote twice")
		}
		if reason, origin, _ := f.suppression(app, "second@example.org"); reason != "unsubscribe" || origin != "recipient" {
			t.Fatal("suppression", reason, origin)
		}
	})

	t.Run("the opt-in is audited, idempotent and under the gate", func(t *testing.T) {
		toggled := f.app("toggled")
		if got, err := f.repo.GetListUnsubscribe(ctx, toggled); err != nil || got.Enabled {
			t.Fatal("default", got, err)
		}
		if _, err := f.repo.GetListUnsubscribe(ctx, f.id()); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("unknown app", err)
		}
		if _, err := f.repo.SetListUnsubscribe(ctx, "operator", f.id(), true); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("set on unknown app", err)
		}
		if _, err := f.repo.SetListUnsubscribe(ctx, "operator", "bad", true); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatal("malformed app", err)
		}
		audits := func() int {
			return f.count(`SELECT count(*) FROM audit_events WHERE action='app.list_unsubscribe.updated' AND app_id=$1`, toggled)
		}
		if got, err := f.repo.SetListUnsubscribe(ctx, "operator", toggled, true); err != nil || !got.Enabled || audits() != 1 {
			t.Fatal("enable", got, err)
		}
		if got, err := f.repo.SetListUnsubscribe(ctx, "operator", toggled, true); err != nil || !got.Enabled || audits() != 1 {
			t.Fatal("identical enable audited", got, err)
		}
		var details string
		if err := f.owner.QueryRowContext(ctx, `SELECT details::text FROM audit_events WHERE action='app.list_unsubscribe.updated' AND app_id=$1`, toggled).Scan(&details); err != nil || details != `{"enabled": true, "previous": false}` {
			t.Fatal("audit details", details, err)
		}
		f.exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
		_, err := f.repo.SetListUnsubscribe(ctx, "operator", toggled, false)
		f.exec(`UPDATE runtime_gate SET mode='open' WHERE singleton`)
		if !errors.Is(err, ErrRuntimeClosed) {
			t.Fatal("changed under a closed gate", err)
		}
		if got, err := f.repo.SetListUnsubscribe(ctx, "operator", toggled, false); err != nil || got.Enabled || audits() != 2 {
			t.Fatal("disable", got, err)
		}
	})

	t.Run("authentication and preparation claims carry the flag", func(t *testing.T) {
		flagged := f.app("flagged")
		f.exec(`UPDATE apps SET list_unsubscribe=true WHERE id=$1`, flagged)
		digest := sha256.Sum256([]byte("key-" + flagged))
		f.exec(`INSERT INTO api_keys(id,digest,app_id) VALUES ($1,$2,$3)`, f.id(), digest[:], flagged)
		got, err := f.repo.LookupKey(ctx, digest)
		if err != nil || got.ID != flagged || !got.ListUnsubscribe {
			t.Fatal("lookup", got, err)
		}
		f.exec(`UPDATE messages SET state='prepared'`)
		queued := f.id()
		f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'k'||$1,false,'queued')`, queued, flagged)
		claim, err := f.repo.ClaimPreparation(ctx, f.id(), 30*time.Second)
		if err != nil || claim.MessageID != queued || !claim.ListUnsubscribe {
			t.Fatal("claim", claim, err)
		}
		f.exec(`UPDATE messages SET state='prepared' WHERE id=$1`, queued)
		plain := f.id()
		f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'k'||$1,false,'queued')`, plain, app)
		claim, err = f.repo.ClaimPreparation(ctx, f.id(), 30*time.Second)
		if err != nil || claim.MessageID != plain || claim.ListUnsubscribe {
			t.Fatal("claim of an app without the flag", claim, err)
		}
		_ = preparation.ErrNoWork
	})

	t.Run("message detail lists feedback without content", func(t *testing.T) {
		_, deliveries, _ := f.message(app, false, "s")
		if err := f.repo.RecordUnsubscribe(ctx, deliveries[0]); err != nil {
			t.Fatal(err)
		}
		var messageID string
		if err := f.owner.QueryRowContext(ctx, `SELECT message_id FROM deliveries WHERE id=$1`, deliveries[0]).Scan(&messageID); err != nil {
			t.Fatal(err)
		}
		detail, err := f.repo.GetMessage(ctx, "operator", app, messageID)
		if err != nil || len(detail.Deliveries) != 2 {
			t.Fatal("detail", err)
		}
		first, second := detail.Deliveries[0], detail.Deliveries[1]
		if len(first.Feedback) != 1 || first.Feedback[0].Kind != "unsubscribe" || first.Feedback[0].Channel != "one_click" || first.Feedback[0].ReceivedAt.IsZero() || len(second.Feedback) != 0 {
			t.Fatal("feedback", first.Feedback, second.Feedback)
		}
	})
}

// TestMigration035 covers the constraint swap over historical data, the
// refusal of a historical collision, and the new source branch.
func TestMigration035(t *testing.T) {
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
	prior := func(t *testing.T) *sql.DB {
		t.Helper()
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			t.Fatal(err)
		}
		schema := "migration35_" + hex.EncodeToString(random[:])
		if _, err := root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
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
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			t.Fatal(err)
		}
		if migrations[34].version != 35 {
			t.Fatal("this test targets migration 35")
		}
		// Schema 34: every migration before 35 (Migrate then applies 35 and later ones).
		for _, item := range migrations[:34] {
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
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return db
	}
	seed := `INSERT INTO apps(id,name) VALUES ('app','test');
		INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('msg','app','source',false,'prepared');
		INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ('d1','msg',0,'final','accepted');
		INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,state,outcome,smtp_code) VALUES ('a1','d1','w',1,'completed','accepted',250);
		INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ('e-accepted','app','msg','d1','a1','accepted',250)`

	t.Run("a historical feedback outcome blocks the migration", func(t *testing.T) {
		db := prior(t)
		if _, err := db.ExecContext(ctx, seed+`; INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,state,outcome,smtp_code) VALUES ('a2','d1','w',1,'completed','accepted',250);
			INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ('e-collide','app','msg','d1','a2','unsubscribed',0)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE runtime_gate SET mode='closed' WHERE singleton`); err != nil {
			t.Fatal(err)
		}
		err := Migrate(ctx, db)
		var applyErr *migrationApplyError
		if !errors.As(err, &applyErr) || applyErr.version != 35 || !strings.Contains(err.Error(), "historical feedback outcome needs review") {
			t.Fatal("collision not refused", err)
		}
		var count int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM thinpost_schema_migrations`).Scan(&count); err != nil || count != 34 {
			t.Fatal("refused migration changed the ledger", count, err)
		}
		if err = db.QueryRowContext(ctx, `SELECT to_regclass('recipient_feedback') IS NOT NULL`).Scan(new(bool)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("historical events survive and the new branch is enforced", func(t *testing.T) {
		db := prior(t)
		if _, err := db.ExecContext(ctx, seed+`; UPDATE runtime_gate SET mode='closed' WHERE singleton`); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		var events int
		var flag bool
		if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM webhook_events WHERE outcome='accepted'),(SELECT list_unsubscribe FROM apps WHERE id='app')`).Scan(&events, &flag); err != nil || events != 1 || flag {
			t.Fatal("carry-over", events, flag, err)
		}
		insertFeedback := `INSERT INTO recipient_feedback(id,delivery_id,kind,channel) VALUES ('0000000000000000000000000000000f','d1','unsubscribe','one_click')`
		if _, err := db.ExecContext(ctx, insertFeedback); err != nil {
			t.Fatal(err)
		}
		const feedbackEvent = `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,feedback_id,outcome,smtp_code) VALUES ('0000000000000000000000000000000f','app','msg','d1','0000000000000000000000000000000f','unsubscribed',0)`
		if _, err := db.ExecContext(ctx, feedbackEvent); err != nil {
			t.Fatal("valid feedback event refused", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO recipient_feedback(id,delivery_id,kind,channel) VALUES ('0000000000000000000000000000000e','d1','spam_complaint','arf')`); err != nil {
			t.Fatal(err)
		}
		for name, statement := range map[string]string{
			"duplicate feedback per delivery and kind": `INSERT INTO recipient_feedback(id,delivery_id,kind,channel) VALUES ('0000000000000000000000000000000d','d1','unsubscribe','one_click')`,
			"unknown kind":                       `INSERT INTO recipient_feedback(id,delivery_id,kind,channel) VALUES ('0000000000000000000000000000000c','d1','other','arf')`,
			"unknown channel":                    `INSERT INTO recipient_feedback(id,delivery_id,kind,channel) VALUES ('0000000000000000000000000000000c','d1','spam_complaint','mailto')`,
			"unknown delivery":                   `INSERT INTO recipient_feedback(id,delivery_id,kind,channel) VALUES ('0000000000000000000000000000000c','nope','spam_complaint','arf')`,
			"oversize reporter domain":           `INSERT INTO recipient_feedback(id,delivery_id,kind,channel,reporter_domain) VALUES ('0000000000000000000000000000000c','d1','spam_complaint','arf','` + strings.Repeat("a", 254) + `')`,
			"second event for the same feedback": `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,feedback_id,outcome,smtp_code) VALUES ('0000000000000000000000000000000b','app','msg','d1','0000000000000000000000000000000f','unsubscribed',0)`,
			"feedback outcome without feedback":  `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,outcome,smtp_code) VALUES ('0000000000000000000000000000000a','app','msg','d1','unsubscribed',0)`,
			"feedback outcome from an attempt":   `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ('0000000000000000000000000000000a','app','msg','d1','a1','unsubscribed',0)`,
			"complaint outcome from an attempt":  `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ('0000000000000000000000000000000a','app','msg','d1','a1','complained',0)`,
			"feedback with an attempt":           `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,feedback_id,outcome,smtp_code) VALUES ('0000000000000000000000000000000a','app','msg','d1','a1','0000000000000000000000000000000e','complained',0)`,
			"feedback with another outcome":      `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,feedback_id,outcome,smtp_code) VALUES ('0000000000000000000000000000000a','app','msg','d1','0000000000000000000000000000000e','opened',0)`,
			"feedback event with an SMTP code":   `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,feedback_id,outcome,smtp_code) VALUES ('0000000000000000000000000000000a','app','msg','d1','0000000000000000000000000000000e','complained',250)`,
			"feedback of another delivery":       `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,feedback_id,outcome,smtp_code) VALUES ('0000000000000000000000000000000a','app','msg','d2','0000000000000000000000000000000e','complained',0)`,
		} {
			if name == "feedback of another delivery" {
				if _, err := db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ('d2','msg',1,'final2','accepted')`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.ExecContext(ctx, statement); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		// The complaint branch is usable by the later migration without a constraint swap.
		if _, err := db.ExecContext(ctx, `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,feedback_id,outcome,smtp_code) VALUES ('0000000000000000000000000000000e','app','msg','d1','0000000000000000000000000000000e','complained',0)`); err != nil {
			t.Fatal("complaint event refused", err)
		}
	})
}
