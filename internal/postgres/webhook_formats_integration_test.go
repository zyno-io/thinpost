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

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook"
)

// message inserts a message with recorded metadata and two recipients, each
// with a delivery and a delivery attempt.
func (f *webhookFixture) message(app string, protected bool, subject string) (message string, deliveries, attempts []string) {
	f.t.Helper()
	message = f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev,sender_address,sender_name) VALUES ($1,$2,'k'||$1,$3,$4,2,'sender.example','example.sender.','Sender@Sender.example','Sender Name')`, message, app, protected, subject)
	for i, address := range []string{"first@example.net", "second@example.org"} {
		f.exec(`INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ($1,$2,$3,$4,$4,split_part($4,'@',2),reverse(split_part($4,'@',2))||'.','to')`, message, app, i, address)
		delivery, attempt := f.id(), f.id()
		f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,$3,'f'||$1)`, delivery, message, i)
		f.exec(`INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch) VALUES ($1,$2,'w',1)`, attempt, delivery)
		deliveries, attempts = append(deliveries, delivery), append(attempts, attempt)
	}
	return message, deliveries, attempts
}

func (f *webhookFixture) deliveryEvent(app, message, delivery, attempt, outcome string, code int, state string) string {
	f.t.Helper()
	event := f.id()
	var st any
	if state != "" {
		st = state
	}
	f.exec(`INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code,delivery_state) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, event, app, message, delivery, attempt, outcome, code, st)
	return event
}

func (f *webhookFixture) createFormat(app, name, endpoint string, events []string, secret bool, auth *admin.SealedWebhookAuth) admin.Webhook {
	f.t.Helper()
	var sealed []byte
	if secret {
		sealed = []byte("sealed")
	}
	item, err := f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), admin.WebhookInput{Format: name, Endpoint: endpoint, Events: events}, sealed, auth)
	if err != nil {
		f.t.Fatal(name, err)
	}
	return item
}

func (f *webhookFixture) queued(event string) map[string]bool {
	f.t.Helper()
	rows, err := f.owner.QueryContext(f.ctx, `SELECT subscription_id FROM webhook_deliveries WHERE event_id=$1`, event)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		got[id] = true
	}
	return got
}

func TestWebhookFormatsPostgres(t *testing.T) {
	f := newWebhookFixture(t, "webhook_formats_")
	invalid := func(err error) bool { return errors.Is(err, admin.ErrInvalidInput) }

	t.Run("endpoint storage per format", func(t *testing.T) {
		app, other := f.app("storage"), f.app("storage-other")
		auth := &admin.SealedWebhookAuth{Ciphertext: []byte("sealed-auth"), Summary: admin.WebhookAuthSummary{Username: "user", HeaderNames: []string{"X-Token", "X-Other"}}}
		pm := f.createFormat(app, "postmark", "https://pm.example.com/", []string{"Delivery", "Bounce"}, false, auth)
		if pm.Format != "postmark" || pm.SecretCreatedAt != nil || pm.Auth == nil || pm.Auth.Username != "user" || len(pm.Auth.HeaderNames) != 2 || len(pm.Events) != 2 {
			t.Fatalf("postmark %+v", pm)
		}
		rs := f.createFormat(app, "resend", "https://rs.example.com/", nil, true, nil)
		mg := f.createFormat(app, "mailgun", "https://mg.example.com/", []string{"permanent_fail"}, true, nil)
		native := f.create(app, "https://native.example.com/", "accepted")
		if rs.SecretCreatedAt == nil || rs.Auth != nil || mg.Format != "mailgun" || native.Format != "thinpost" {
			t.Fatal("formats", rs, mg, native)
		}
		for name, tc := range map[string]struct {
			in     admin.WebhookInput
			sealed []byte
			auth   *admin.SealedWebhookAuth
		}{
			"unknown format":             {admin.WebhookInput{Format: "sendgrid", Endpoint: "https://x.example.com/"}, []byte("s"), nil},
			"postmark with a secret":     {admin.WebhookInput{Format: "postmark", Endpoint: "https://x.example.com/"}, []byte("s"), nil},
			"signed without a secret":    {admin.WebhookInput{Format: "resend", Endpoint: "https://x.example.com/"}, nil, nil},
			"event of another format":    {admin.WebhookInput{Format: "postmark", Endpoint: "https://x.example.com/", Events: []string{"accepted"}}, nil, nil},
			"native with provider event": {admin.WebhookInput{Format: "thinpost", Endpoint: "https://x.example.com/", Events: []string{"Delivery"}}, []byte("s"), nil},
			"auth without a summary":     {admin.WebhookInput{Format: "postmark", Endpoint: "https://x.example.com/"}, nil, &admin.SealedWebhookAuth{Ciphertext: []byte("x")}},
		} {
			if _, err := f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), tc.in, tc.sealed, tc.auth); !invalid(err) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// Events are narrowed to the row's own format on update.
		bad, good := []string{"accepted"}, []string{"Bounce"}
		if _, err := f.repo.UpdateWebhook(f.ctx, "operator", app, pm.ID, admin.WebhookPatch{Events: &bad}, nil); !invalid(err) {
			t.Fatal("native event accepted by a postmark endpoint", err)
		}
		if got, err := f.repo.UpdateWebhook(f.ctx, "operator", app, pm.ID, admin.WebhookPatch{Events: &good}, nil); err != nil || len(got.Events) != 1 || got.Auth == nil || got.Format != "postmark" {
			t.Fatal("valid event update", got, err)
		}
		// Auth replace, then unchanged, then remove; the summary follows the ciphertext.
		replacement := &admin.SealedWebhookAuth{Ciphertext: []byte("sealed-two"), Summary: admin.WebhookAuthSummary{HeaderNames: []string{"X-New"}}}
		got, err := f.repo.UpdateWebhook(f.ctx, "operator", app, pm.ID, admin.WebhookPatch{}, replacement)
		if err != nil || got.Auth == nil || got.Auth.Username != "" || len(got.Auth.HeaderNames) != 1 || got.Auth.HeaderNames[0] != "X-New" {
			t.Fatal("replace auth", got, err)
		}
		off := false
		if got, err = f.repo.UpdateWebhook(f.ctx, "operator", app, pm.ID, admin.WebhookPatch{Active: &off}, nil); err != nil || got.Auth == nil {
			t.Fatal("auth changed by an unrelated patch", got, err)
		}
		if got, err = f.repo.UpdateWebhook(f.ctx, "operator", app, pm.ID, admin.WebhookPatch{}, &admin.SealedWebhookAuth{}); err != nil || got.Auth != nil {
			t.Fatal("remove auth", got, err)
		}
		if n := f.count(`SELECT count(*) FROM webhook_subscriptions WHERE id=$1 AND auth_ciphertext IS NULL AND auth_username IS NULL AND auth_header_names IS NULL`, pm.ID); n != 1 {
			t.Fatal("auth columns not cleared together")
		}
		// An unsigned endpoint has no secret to rotate; the format must match.
		if _, err = f.repo.ReplaceWebhookSecret(f.ctx, "operator", app, pm.ID, "postmark", []byte("x")); !invalid(err) {
			t.Fatal("rotated an unsigned endpoint", err)
		}
		if _, err = f.repo.ReplaceWebhookSecret(f.ctx, "operator", app, rs.ID, "mailgun", []byte("x")); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("rotation ignored the endpoint's format", err)
		}
		if _, err = f.repo.ReplaceWebhookSecret(f.ctx, "operator", app, rs.ID, "resend", []byte("x")); err != nil {
			t.Fatal(err)
		}
		if name, err := f.repo.WebhookFormat(f.ctx, app, rs.ID); err != nil || name != "resend" {
			t.Fatal("format lookup", name, err)
		}
		if _, err = f.repo.WebhookFormat(f.ctx, other, rs.ID); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("cross-app format lookup", err)
		}
		// Target carries format, sealed secret and sealed auth; delete erases both.
		withAuth := f.createFormat(app, "resend", "https://auth.example.com/", nil, true, auth)
		target, err := f.repo.WebhookTarget(f.ctx, "operator", app, withAuth.ID)
		if err != nil || target.Format != "resend" || string(target.SealedAuth) != "sealed-auth" || string(target.SealedSecret) != "sealed" {
			t.Fatal("target", target, err)
		}
		if err = f.repo.DeleteWebhook(f.ctx, "operator", app, withAuth.ID); err != nil {
			t.Fatal(err)
		}
		if n := f.count(`SELECT count(*) FROM webhook_subscriptions WHERE id=$1 AND secret_ciphertext IS NULL AND auth_ciphertext IS NULL AND auth_username IS NULL AND auth_header_names IS NULL AND deleted_at IS NOT NULL`, withAuth.ID); n != 1 {
			t.Fatal("delete kept secrets or auth")
		}
	})

	t.Run("fan-out classifies per format", func(t *testing.T) {
		app := f.app("fanout")
		message, deliveries, attempts := f.message(app, false, "Receipt")
		pm := f.createFormat(app, "postmark", "https://pm.example.com/", nil, false, nil)
		pmBounce := f.createFormat(app, "postmark", "https://pmb.example.com/", []string{"Bounce"}, false, nil)
		rs := f.createFormat(app, "resend", "https://rs.example.com/", nil, true, nil)
		rsDelayed := f.createFormat(app, "resend", "https://rsd.example.com/", []string{"email.delivery_delayed"}, true, nil)
		mg := f.createFormat(app, "mailgun", "https://mg.example.com/", []string{"temporary_fail"}, true, nil)
		native := f.create(app, "https://native.example.com/", "rejected")
		check := func(label, event string, want ...admin.Webhook) {
			t.Helper()
			f.fanout(event, app)
			got := f.queued(event)
			if len(got) != len(want) {
				t.Fatalf("%s: queued %d endpoints, want %d", label, len(got), len(want))
			}
			for _, w := range want {
				if !got[w.ID] {
					t.Fatalf("%s: %s endpoint %s missed", label, w.Format, w.Endpoint)
				}
			}
		}
		// A retried 4xx is a delay: Postmark retries silently and sends nothing.
		check("deferred 4xx", f.deliveryEvent(app, message, deliveries[0], attempts[0], "rejected", 451, "queued"), rs, rsDelayed, mg, native)
		// The same outcome once the retry limit is exhausted is a soft bounce.
		check("exhausted 4xx", f.deliveryEvent(app, message, deliveries[1], attempts[1], "rejected", 452, "failed"), pm, pmBounce, rs, native)
		check("permanent 5xx", f.deliveryEvent(app, message, deliveries[0], f.attempt(deliveries[0]), "rejected", 550, "failed"), pm, pmBounce, rs, native)
		// route_rejected precedes any recipient: soft at any code, never permanent.
		check("route_rejected", f.deliveryEvent(app, message, deliveries[1], f.attempt(deliveries[1]), "route_rejected", 554, "failed"), pm, pmBounce, rs)
		check("accepted", f.deliveryEvent(app, message, deliveries[0], f.attempt(deliveries[0]), "accepted", 250, "accepted"), pm, rs)
		check("uncertain has no provider event", f.deliveryEvent(app, message, deliveries[1], f.attempt(deliveries[1]), "uncertain", 0, "uncertain"))
		// A pre-030 event has no recorded state; a 5xx rejection is still final.
		check("pre-030 rejected 550", f.deliveryEvent(app, message, deliveries[0], f.attempt(deliveries[0]), "rejected", 550, ""), pm, pmBounce, rs, native)
	})

	t.Run("fan-out cap", func(t *testing.T) {
		app := f.app("cap")
		message, deliveries, attempts := f.message(app, false, "Receipt")
		f.exec(`INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext,format) SELECT md5('cap-'||n),$1,'https://example.net/events','\x01'::bytea,'resend' FROM generate_series(1,11) n`, app)
		event := f.deliveryEvent(app, message, deliveries[0], attempts[0], "accepted", 250, "accepted")
		tx, err := f.repo.beginRuntime(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err = enqueueWebhookDeliveries(f.ctx, tx, event, app); !errors.Is(err, errWebhookFanoutExceeded) {
			t.Fatal("more than ten active endpoints must fail the event transaction", err)
		}
	})

	t.Run("claim context", func(t *testing.T) {
		drain := func() {
			for {
				claim, err := f.repo.ClaimWebhook(f.ctx, f.id(), time.Minute)
				if errors.Is(err, webhook.ErrNoWork) {
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err = f.repo.CommitWebhook(f.ctx, claim, 204); err != nil {
					t.Fatal(err)
				}
			}
		}
		drain()
		claimOne := func(app, event string) webhook.Claim {
			t.Helper()
			f.fanout(event, app)
			claim, err := f.repo.ClaimWebhook(f.ctx, f.id(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.repo.CommitWebhook(f.ctx, claim, 204); err != nil {
				t.Fatal(err)
			}
			return claim
		}
		// Enhanced Protection: the subject is masked in SQL and never reaches Go.
		app := f.app("claim-ep")
		pm := f.createFormat(app, "postmark", "https://pm.example.com/", nil, false, &admin.SealedWebhookAuth{Ciphertext: []byte("sealed-auth"), Summary: admin.WebhookAuthSummary{Username: "u"}})
		message, deliveries, attempts := f.message(app, true, "TOP-SECRET-SUBJECT")
		claim := claimOne(app, f.deliveryEvent(app, message, deliveries[1], attempts[1], "accepted", 250, "accepted"))
		c := claim.Context
		if claim.Format != "postmark" || claim.SubscriptionID != pm.ID || claim.SealedSecret != nil || string(claim.SealedAuth) != "sealed-auth" || claim.Event.DeliveryState != "accepted" {
			t.Fatal("claim", claim)
		}
		if c.Subject != "" || !c.EnhancedProtection || c.SenderDomain != "sender.example" || c.SenderAddress != "Sender@Sender.example" || c.SenderName != "Sender Name" || c.AcceptedAt.IsZero() || len(c.Recipients) != 1 || c.Recipients[0] != "second@example.org" {
			t.Fatalf("EP context %+v", c)
		}
		// Not protected: the subject is available, one recipient per delivery.
		app = f.app("claim-plain")
		f.createFormat(app, "resend", "https://rs.example.com/", nil, true, nil)
		message, deliveries, attempts = f.message(app, false, "Receipt")
		claim = claimOne(app, f.deliveryEvent(app, message, deliveries[0], attempts[0], "accepted", 250, "accepted"))
		if claim.Context.Subject != "Receipt" || len(claim.Context.Recipients) != 1 || claim.Context.Recipients[0] != "first@example.net" || claim.SealedAuth != nil || string(claim.SealedSecret) != "sealed" {
			t.Fatalf("plain context %+v", claim.Context)
		}
		// A message-level event lists every recipient.
		event := f.id()
		f.exec(`INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ($1,$2,$3,'preparation_failed',0)`, event, app, message)
		if claim = claimOne(app, event); len(claim.Context.Recipients) != 2 || claim.Context.Recipients[0] != "first@example.net" || claim.Context.Recipients[1] != "second@example.org" {
			t.Fatalf("message-level recipients %+v", claim.Context.Recipients)
		}
		// Native endpoints load no context at all.
		app = f.app("claim-native")
		f.create(app, "https://native.example.com/")
		message, deliveries, attempts = f.message(app, false, "Receipt")
		if claim = claimOne(app, f.deliveryEvent(app, message, deliveries[0], attempts[0], "accepted", 250, "accepted")); claim.Format != "thinpost" || claim.Context.Subject != "" || len(claim.Context.Recipients) != 0 {
			t.Fatalf("native claim %+v", claim)
		}
		// Opens: only the earliest event on a link is the first open.
		app = f.app("claim-open")
		f.createFormat(app, "postmark", "https://pm.example.com/", []string{"Open"}, false, nil)
		message, deliveries, _ = f.message(app, false, "Receipt")
		link := f.id()
		f.exec(`INSERT INTO tracking_objects(delivery_id,object_key,expires_at,event_limit) VALUES ($1,'tracking/v1/0000000001/'||$1,now()+interval '1 day',10)`, deliveries[0])
		f.exec(`INSERT INTO tracking_links(id,token_hash,delivery_id,kind) VALUES ($1,decode(repeat('ab',32),'hex'),$2,'open')`, link, deliveries[0])
		var opens []string
		for i := 0; i < 2; i++ {
			te, we := f.id(), f.id()
			f.exec(`INSERT INTO tracking_events(id,link_id,delivery_id,kind,outcome,occurred_at) VALUES ($1,$2,$3,'open','opened',now()+$4*interval '1 second')`, te, link, deliveries[0], i)
			f.exec(`INSERT INTO webhook_events(id,app_id,message_id,delivery_id,outcome,smtp_code,tracking_event_id) VALUES ($1,$2,$3,$4,'opened',0,$5)`, we, app, message, deliveries[0], te)
			opens = append(opens, we)
		}
		for i, event := range opens {
			if claim = claimOne(app, event); claim.Context.FirstOpen == nil || *claim.Context.FirstOpen != (i == 0) {
				t.Fatalf("open %d first-open flag %v", i, claim.Context.FirstOpen)
			}
		}
	})

	t.Run("mapping drift cancels without an attempt", func(t *testing.T) {
		app := f.app("drift")
		pm := f.createFormat(app, "postmark", "https://pm.example.com/", nil, false, nil)
		message, deliveries, attempts := f.message(app, false, "Receipt")
		// Queued directly, as if a deploy changed the mapping after fan-out.
		event := f.deliveryEvent(app, message, deliveries[0], attempts[0], "uncertain", 0, "uncertain")
		f.exec(`INSERT INTO webhook_deliveries(event_id,subscription_id,app_id) VALUES ($1,$2,$3)`, event, pm.ID, app)
		if _, err := f.repo.ClaimWebhook(f.ctx, f.id(), time.Minute); !errors.Is(err, webhook.ErrSkipped) {
			t.Fatal("unrepresentable event was claimed", err)
		}
		if n := f.count(`SELECT count(*) FROM webhook_deliveries WHERE event_id=$1 AND state='cancelled' AND claim_token IS NULL`, event); n != 1 {
			t.Fatal("delivery not cancelled")
		}
		if n := f.count(`SELECT count(*) FROM webhook_attempts WHERE event_id=$1`, event); n != 0 {
			t.Fatal("a skipped delivery recorded an attempt")
		}
		if _, err := f.repo.ClaimWebhook(f.ctx, f.id(), time.Minute); !errors.Is(err, webhook.ErrNoWork) {
			t.Fatal("skipped delivery claimed again", err)
		}
	})
}

func (f *webhookFixture) attempt(delivery string) string {
	f.t.Helper()
	id := f.id()
	f.exec(`INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch) VALUES ($1,$2,'w',1)`, id, delivery)
	return id
}

// TestWebhookFormatsMigration applies migration 30 over a schema-29 database
// holding native endpoints and events.
func TestWebhookFormatsMigration(t *testing.T) {
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
	schema := "webhook_formats_migration_" + hex.EncodeToString(random[:])
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
	if _, err = db.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:29] {
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
		INSERT INTO messages(id,app_id,object_key,enhanced_protection) VALUES ('m','app','k',true);
		INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext,outcomes) VALUES ('filtered','app','https://a.example.com/','\x01','{accepted,opened}'),('plain','app','https://b.example.com/','\x01',NULL);
		INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ('e1','app','m','preparation_failed',0);
		INSERT INTO webhook_deliveries(event_id,subscription_id,app_id) VALUES ('e1','plain','app')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE runtime_gate SET mode='closed' WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var filter string
	var formatName string
	if err = db.QueryRowContext(ctx, `SELECT format,array_to_string(events,',') FROM webhook_subscriptions WHERE id='filtered'`).Scan(&formatName, &filter); err != nil || formatName != "thinpost" || filter != "accepted,opened" {
		t.Fatal("existing endpoint not carried over", formatName, filter, err)
	}
	if n := func() (n int) {
		_ = db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_subscriptions WHERE format='thinpost' AND events IS NOT NULL OR id='plain' AND events IS NULL`).Scan(&n)
		return
	}(); n != 2 {
		t.Fatal("filters not preserved", n)
	}
	var state sql.NullString
	if err = db.QueryRowContext(ctx, `SELECT delivery_state FROM webhook_events WHERE id='e1'`).Scan(&state); err != nil || state.Valid {
		t.Fatal("earlier events must keep NULL delivery_state", state, err)
	}
	var state2 string
	if err = db.QueryRowContext(ctx, `SELECT state FROM webhook_deliveries WHERE event_id='e1'`).Scan(&state2); err != nil || state2 != "queued" {
		t.Fatal("queued work was disturbed", state2, err)
	}
	for name, statement := range map[string]string{
		"bad format shape":             `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext,format) VALUES ('x1','app','https://x.example.com/','\x01','Bad Format')`,
		"signed format without secret": `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,format) VALUES ('x2','app','https://x.example.com/','resend')`,
		"postmark with a secret":       `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext,format) VALUES ('x3','app','https://x.example.com/','\x01','postmark')`,
		"auth without a summary":       `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext,auth_ciphertext) VALUES ('x4','app','https://x.example.com/','\x01','\x01')`,
		"summary without auth":         `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext,auth_username) VALUES ('x5','app','https://x.example.com/','\x01','u')`,
		"six header names":             `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext,auth_ciphertext,auth_header_names) VALUES ('x6','app','https://x.example.com/','\x01','\x01','{a,b,c,d,e,f}')`,
		"tombstone keeping auth":       `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,deleted_at,active,auth_ciphertext,auth_username) VALUES ('x7','app','https://x.example.com/',now(),false,'\x01','u')`,
		"tombstone keeping a secret":   `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,deleted_at,active,secret_ciphertext) VALUES ('x8','app','https://x.example.com/',now(),false,'\x01')`,
	} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, statement := range map[string]string{
		"unsigned postmark without auth": `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,format) VALUES ('ok1','app','https://x.example.com/','postmark')`,
		"postmark with basic auth":       `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,format,auth_ciphertext,auth_username) VALUES ('ok2','app','https://x.example.com/','postmark','\x01','u')`,
		"resend with header auth":        `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,format,secret_ciphertext,auth_ciphertext,auth_header_names) VALUES ('ok3','app','https://x.example.com/','resend','\x01','\x01','{X-Token}')`,
	} {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
}
