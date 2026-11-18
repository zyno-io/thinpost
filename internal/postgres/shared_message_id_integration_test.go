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

	"github.com/zyno-io/thinpost/internal/preparation"
)

// TestMigration038 applies migrations through 37 over prepared messages, then
// migrates: existing messages use the per-delivery form, and the CHECK refuses
// the shared form without a recorded sender domain.
func TestMigration038(t *testing.T) {
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
	schema := "migration38_" + hex.EncodeToString(random[:])
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
	if migrations[37].version != 38 {
		t.Fatal("this test targets migration 38")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:37] {
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
		INSERT INTO messages(id,app_id,object_key,enhanced_protection,state,subject,recipient_count,sender_domain,sender_domain_rev) VALUES ('with-domain','app','k1',false,'prepared','s',1,'example.com','com.example.');
		INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('pre-28','app','k2',false,'prepared')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var shared int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE shared_message_id`).Scan(&shared); err != nil || shared != 0 {
		t.Fatal("existing messages must keep the per-delivery form", shared, err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE messages SET shared_message_id=true WHERE id='pre-28'`); err == nil {
		t.Fatal("the shared form was accepted without a sender domain")
	}
	if _, err = db.ExecContext(ctx, `UPDATE messages SET shared_message_id=true WHERE id='with-domain'`); err != nil {
		t.Fatal(err)
	}
}

// A message prepared at schema 38 records the shared form with its deliveries;
// a claim whose domain differs from the recorded sender domain commits nothing.
func TestSharedMessageIDPreparationCommit(t *testing.T) {
	f := newWebhookFixture(t, "sharedprep_")
	ctx := f.ctx
	app := f.app("shared")
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, app)
	f.repo.Objects = objectVerifierFunc(func(context.Context, string) error { return nil })
	message := f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,sender_domain,sender_domain_rev,subject,recipient_count) VALUES ($1,$2,'submissions/shared',true,'example.com','com.example.','s',2)`, message, app)
	flag := func() bool {
		var shared bool
		if err := f.owner.QueryRowContext(ctx, `SELECT shared_message_id FROM messages WHERE id=$1`, message).Scan(&shared); err != nil {
			t.Fatal(err)
		}
		return shared
	}
	claim, err := f.repo.ClaimPreparation(ctx, f.id(), 30*time.Second)
	if err != nil || claim.MessageID != message || claim.SenderDomain != "example.com" {
		t.Fatal("claim", claim, err)
	}
	a, b := f.id(), f.id()
	deliveries := []preparation.Delivery{{ID: a, RecipientIndex: 0, ObjectKey: fixtureObjectKey("final", a)}, {ID: b, RecipientIndex: 1, ObjectKey: fixtureObjectKey("final", b)}}
	// other.example is verified for the app, so only the sender_domain guard in
	// CommitPreparation's WHERE can reject this commit.
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'other.example',true,now())`, app)
	claim.SigningDomain = "other.example"
	if err = f.repo.CommitPreparation(ctx, claim, deliveries); !errors.Is(err, preparation.ErrLostLease) {
		t.Fatal("a signing domain that differs from the recorded sender domain was not refused as a lost lease", err)
	}
	if flag() || f.count(`SELECT count(*) FROM deliveries WHERE message_id=$1`, message) != 0 {
		t.Fatal("a rejected commit changed the message")
	}
	claim.SigningDomain = "example.com"
	if err = f.repo.CommitPreparation(ctx, claim, deliveries); err != nil {
		t.Fatal(err)
	}
	if !flag() || f.count(`SELECT count(*) FROM deliveries WHERE message_id=$1`, message) != 2 {
		t.Fatal("commit did not record the shared form with the deliveries")
	}
}

// The native webhook and the admin API report the delivery's actual Message-ID:
// shared form, per-delivery form for a legacy message, and nothing without a
// recorded sender domain or a delivery.
func TestMessageIDHeaderReporting(t *testing.T) {
	f := newWebhookFixture(t, "msgidheader_")
	ctx := f.ctx
	app := f.app("header")
	f.create(app, "https://native.example.com/")
	sharedMessage, sharedDeliveries, sharedAttempts := f.message(app, false, "s")
	f.exec(`UPDATE messages SET shared_message_id=true WHERE id=$1`, sharedMessage)
	legacyMessage, legacyDeliveries, legacyAttempts := f.message(app, false, "s")
	preMessage, preDeliveries, preAttempts := f.message(app, false, "s")
	f.exec(`UPDATE messages SET subject=NULL,recipient_count=NULL,sender_domain=NULL,sender_domain_rev=NULL,sender_address=NULL,sender_name=NULL WHERE id=$1`, preMessage)
	f.exec(`DELETE FROM message_recipients WHERE message_id=$1`, preMessage)

	claimHeader := func(event string) (string, string) {
		t.Helper()
		f.fanout(event, app)
		claim, err := f.repo.ClaimWebhook(ctx, f.id(), time.Minute)
		if err != nil || claim.Event.ID != event {
			t.Fatal("claim", claim.Event, err)
		}
		return claim.Event.MessageIDHeader, claim.Event.DeliveryID
	}
	shared := "<" + sharedMessage + "@sender.example>"
	if header, _ := claimHeader(f.deliveryEvent(app, sharedMessage, sharedDeliveries[0], sharedAttempts[0], "accepted", 250, "accepted")); header != shared {
		t.Fatal("shared event header", header)
	}
	if header, _ := claimHeader(f.deliveryEvent(app, sharedMessage, sharedDeliveries[1], sharedAttempts[1], "accepted", 250, "accepted")); header != shared {
		t.Fatal("second recipient of a shared message", header)
	}
	if header, _ := claimHeader(f.deliveryEvent(app, legacyMessage, legacyDeliveries[1], legacyAttempts[1], "accepted", 250, "accepted")); header != "<"+legacyDeliveries[1]+"@sender.example>" {
		t.Fatal("legacy event header", header)
	}
	if header, _ := claimHeader(f.deliveryEvent(app, preMessage, preDeliveries[0], preAttempts[0], "accepted", 250, "accepted")); header != "" {
		t.Fatal("a message without a sender domain reported a header", header)
	}
	event := f.id()
	f.exec(`INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ($1,$2,$3,'preparation_failed',0)`, event, app, legacyMessage)
	if header, _ := claimHeader(event); header != "" {
		t.Fatal("an unprepared event reported a header", header)
	}
	// A preparation_failed event of a message a later retry prepared in the shared form.
	event = f.id()
	f.exec(`INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ($1,$2,$3,'preparation_failed',0)`, event, app, sharedMessage)
	if header, _ := claimHeader(event); header != "" {
		t.Fatal("a preparation_failed event of a shared message reported a header", header)
	}

	for _, tc := range []struct {
		name, message string
		deliveries    []string
		want          func(i int) string
	}{
		{"shared", sharedMessage, sharedDeliveries, func(int) string { return shared }},
		{"legacy", legacyMessage, legacyDeliveries, func(i int) string { return "<" + legacyDeliveries[i] + "@sender.example>" }},
		{"pre-28", preMessage, preDeliveries, func(int) string { return "" }},
	} {
		detail, err := f.repo.GetMessage(ctx, "operator", app, tc.message)
		if err != nil || len(detail.Deliveries) != len(tc.deliveries) {
			t.Fatal(tc.name, err)
		}
		for i, d := range detail.Deliveries {
			got := ""
			if d.MessageIDHeader != nil {
				got = *d.MessageIDHeader
			}
			if d.ID != tc.deliveries[i] || got != tc.want(i) {
				t.Fatal(tc.name, "delivery", i, got)
			}
		}
	}
}
