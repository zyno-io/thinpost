package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/webhook"
)

// A database restored at a schema before 39 has no inbound_messages, and the
// recovery inventory runs before the deploy migrates it. Every other kind must
// still page through.
func TestRecoveryObjectInventoryBeforeSchema39(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
	schema := "recovery_s38_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
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
	defer db.Close()
	if migrations[37].version != 38 {
		t.Fatal("this test targets schema 38")
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:38] {
		body, checksum, bodyErr := migrationBody(item)
		if bodyErr != nil {
			t.Fatal(bodyErr)
		}
		if _, err = db.ExecContext(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, item.version, checksum); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('app','app');
		INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('m1','app','source/1',true,'done');
		INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','m1',0,'final/1','accepted');
		INSERT INTO bounce_receipts(id,delivery_id,object_key,fingerprint) VALUES ('b1','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','bounce/1',decode(repeat('00',32),'hex'));
		INSERT INTO tracking_objects(delivery_id,object_key,expires_at,event_limit) VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','tracking/v1/0000000000/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',now(),1)`); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT 1 WHERE to_regclass('inbound_messages') IS NULL`).Scan(new(int)); err != nil {
		t.Fatal("fixture is not a pre-039 schema", err)
	}
	const gen = "11111111111111111111111111111111"
	const run = "22222222222222222222222222222222"
	if err = RecoveryBegin(ctx, db, 1, gen, run, "operator"); err != nil {
		t.Fatal(err)
	}
	if complete, qerr := RecoveryQuarantineBatch(ctx, db, gen, run, "operator"); qerr != nil || !complete {
		t.Fatal(complete, qerr)
	}
	verify := func(context.Context, string) error { return nil }
	kinds := map[string]int{}
	var cursor RecoveryObjectCursor
	for {
		page, pageErr := RecoveryObjectInventoryPage(ctx, db, gen, run, 3, cursor, 1, verify)
		if pageErr != nil {
			t.Fatal("inventory failed on a pre-039 schema", pageErr)
		}
		if len(page.Failed) != 0 {
			t.Fatal(page.Failed)
		}
		if page.Next != nil {
			kinds[page.Next.Kind]++
		}
		if page.Next == nil {
			break
		}
		cursor = *page.Next
	}
	for _, kind := range []string{"bounce", "delivery", "message"} {
		if kinds[kind] == 0 {
			t.Fatal("kind not paged through:", kind, kinds)
		}
	}
	// A cursor left at the inbound kind (from a run that began after 039) resumes at messages.
	page, err := RecoveryObjectInventoryPage(ctx, db, gen, run, 3, RecoveryObjectCursor{Kind: "inbound", ID: "x"}, 100, verify)
	if err != nil || page.Checked != 2 {
		t.Fatal("resume past a missing kind", page, err)
	}
}

// The exhausted-attempt sweep in the outbound claim never fails an inbound-lane row.
func TestOutboundClaimSweepIgnoresInboundLane(t *testing.T) {
	f := newWebhookFixture(t, "inb_sweep_")
	app := f.app("sweep")
	hook := f.create(app, "https://sweep.example.com/")
	event := f.acceptedEvent(app)
	token := f.id()
	f.exec(`INSERT INTO webhook_deliveries(event_id,subscription_id,app_id,state,attempts,claim_token,claim_until,lane) VALUES ($1,$2,$3,'in_flight',$4,$5,now()-interval '1 minute','inbound')`, event, hook.ID, app, maxWebhookAttempts, token)
	f.exec(`INSERT INTO webhook_attempts(id,event_id,subscription_id) VALUES ($1,$2,$3)`, token, event, hook.ID)
	if _, err := f.repo.ClaimWebhook(f.ctx, f.id(), time.Minute); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal("claim", err)
	}
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE event_id=$1 AND state='in_flight' AND lane='inbound'`, event) != 1 {
		t.Fatal("the event-lane sweep failed an inbound-lane delivery")
	}
	f.exec(`UPDATE webhook_deliveries SET lane='event' WHERE event_id=$1`, event)
	if _, err := f.repo.ClaimWebhook(f.ctx, f.id(), time.Minute); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal("claim", err)
	}
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE event_id=$1 AND state='failed'`, event) != 1 {
		t.Fatal("an exhausted event-lane delivery was not failed")
	}
}
