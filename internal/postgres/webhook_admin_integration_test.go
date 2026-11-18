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
	"github.com/zyno-io/thinpost/internal/webhook"
)

const webhookTestGeneration = "00000000000000000000000000000aaa"

type webhookFixture struct {
	t     *testing.T
	ctx   context.Context
	owner *sql.DB
	repo  *Repository
	next  int
}

func (f *webhookFixture) id() string {
	f.next++
	return fmt.Sprintf("%032x", f.next)
}

func (f *webhookFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.owner.ExecContext(f.ctx, query, args...); err != nil {
		f.t.Fatal(err)
	}
}

// newWebhookFixture returns a fully migrated schema and a repository using the
// restricted runtime role, so every case below runs with production grants.
func newWebhookFixture(t *testing.T, prefix string) *webhookFixture {
	t.Helper()
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
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
	schema := prefix + hex.EncodeToString(random[:])
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
	owner, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	if err = Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, webhookTestGeneration); err != nil {
		t.Fatal(err)
	}
	runtimeDB, cleanup := restrictedRuntimeForTest(t, ctx, owner, u.String())
	t.Cleanup(cleanup)
	return &webhookFixture{t: t, ctx: ctx, owner: owner, repo: &Repository{DB: runtimeDB, Generation: webhookTestGeneration}}
}

func (f *webhookFixture) app(name string) string {
	id := f.id()
	f.exec(`INSERT INTO apps(id,name) VALUES ($1,$2)`, id, name)
	return id
}

func (f *webhookFixture) create(app, endpoint string, outcomes ...string) admin.Webhook {
	f.t.Helper()
	item, err := f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), admin.WebhookInput{Endpoint: endpoint, Events: outcomes}, []byte("sealed"), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return item
}

// event inserts a preparation_failed event, which needs no delivery.
func (f *webhookFixture) event(app string) string {
	f.t.Helper()
	message, event := f.id(), f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection) VALUES ($1,$2,'k'||$1,true)`, message, app)
	f.exec(`INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ($1,$2,$3,'preparation_failed',0)`, event, app, message)
	return event
}

// acceptedEvent inserts an "accepted" event with its delivery and attempt.
func (f *webhookFixture) acceptedEvent(app string) string {
	f.t.Helper()
	message, delivery, attempt, event := f.id(), f.id(), f.id(), f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection) VALUES ($1,$2,'k'||$1,true)`, message, app)
	f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'f')`, delivery, message)
	f.exec(`INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch) VALUES ($1,$2,'w',1)`, attempt, delivery)
	f.exec(`INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ($1,$2,$3,$4,$5,'accepted',250)`, event, app, message, delivery, attempt)
	return event
}

// fanout runs the event-transaction fan-out and commits.
func (f *webhookFixture) fanout(event, app string) {
	f.t.Helper()
	tx, err := f.repo.beginRuntime(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	if err = enqueueWebhookDeliveries(f.ctx, tx, event, app); err != nil {
		f.t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
}

func (f *webhookFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.owner.QueryRowContext(f.ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestWebhookEndpointsPostgres(t *testing.T) {
	f := newWebhookFixture(t, "webhook_endpoints_")
	app, other := f.app("one"), f.app("other")
	notFound := func(err error) bool { return errors.Is(err, admin.ErrNotFound) }
	audits := func(action string) int {
		return f.count(`SELECT count(*) FROM audit_events WHERE action=$1`, action)
	}

	t.Run("create list and validation", func(t *testing.T) {
		created, err := f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), admin.WebhookInput{Endpoint: "https://Hooks.Example.COM:443/events?token=a%2Fb", Description: "primary", Events: []string{"accepted", "rejected"}}, []byte("sealed"), nil)
		if err != nil || created.Endpoint != "https://hooks.example.com/events?token=a%2Fb" || !created.Active || created.Description != "primary" || len(created.Events) != 2 || created.CreatedAt.IsZero() || created.SecretCreatedAt == nil {
			t.Fatal("create", created, err)
		}
		plain := f.create(app, "https://plain.example.com/hook")
		if len(plain.Events) != 0 {
			t.Fatal("unfiltered endpoint has outcomes", plain.Events)
		}
		items, err := f.repo.ListWebhooks(f.ctx, app)
		if err != nil || len(items) != 2 || items[0].ID != created.ID || items[1].ID != plain.ID || items[1].Events != nil {
			t.Fatal("list", items, err)
		}
		if items, err = f.repo.ListWebhooks(f.ctx, other); err != nil || len(items) != 0 {
			t.Fatal("cross-app list", items, err)
		}
		if _, err = f.repo.ListWebhooks(f.ctx, f.id()); !notFound(err) {
			t.Fatal("list for a missing app", err)
		}
		var stored []byte
		if err = f.owner.QueryRowContext(f.ctx, `SELECT secret_ciphertext FROM webhook_subscriptions WHERE id=$1`, created.ID).Scan(&stored); err != nil || string(stored) != "sealed" {
			t.Fatal("sealed secret not stored", err)
		}
		if audits("webhook.created") != 2 {
			t.Fatal("create audits")
		}
		for name, in := range map[string]admin.WebhookInput{
			"http":         {Endpoint: "http://hooks.example.com/"},
			"port":         {Endpoint: "https://hooks.example.com:8443/"},
			"ip":           {Endpoint: "https://127.0.0.1/"},
			"fragment":     {Endpoint: "https://hooks.example.com/#x"},
			"outcome":      {Endpoint: "https://hooks.example.com/", Events: []string{"bogus"}},
			"duplicate":    {Endpoint: "https://hooks.example.com/", Events: []string{"accepted", "accepted"}},
			"description":  {Endpoint: "https://hooks.example.com/", Description: strings.Repeat("d", 257)},
			"control char": {Endpoint: "https://hooks.example.com/", Description: "a\nb"},
		} {
			if _, err = f.repo.InsertWebhook(f.ctx, "operator", app, f.id(), in, []byte("sealed"), nil); !errors.Is(err, admin.ErrInvalidInput) {
				t.Errorf("%s accepted: %v", name, err)
			}
		}
		if _, err = f.repo.InsertWebhook(f.ctx, "operator", f.id(), f.id(), admin.WebhookInput{Endpoint: "https://hooks.example.com/"}, []byte("sealed"), nil); !notFound(err) {
			t.Fatal("create for a missing app", err)
		}
		f.exec(`UPDATE apps SET active=false WHERE id=$1`, other)
		if _, err = f.repo.InsertWebhook(f.ctx, "operator", other, f.id(), admin.WebhookInput{Endpoint: "https://hooks.example.com/"}, []byte("sealed"), nil); !notFound(err) {
			t.Fatal("create for an inactive app", err)
		}
		f.exec(`UPDATE apps SET active=true WHERE id=$1`, other)
	})

	t.Run("cap of ten enabled on create and enable", func(t *testing.T) {
		capped := f.app("capped")
		var ids []string
		for i := 0; i < maxActiveWebhooks; i++ {
			ids = append(ids, f.create(capped, fmt.Sprintf("https://h%d.example.com/", i)).ID)
		}
		if _, err := f.repo.InsertWebhook(f.ctx, "operator", capped, f.id(), admin.WebhookInput{Endpoint: "https://extra.example.com/"}, []byte("s"), nil); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("eleventh endpoint accepted", err)
		}
		off, on := false, true
		if _, err := f.repo.UpdateWebhook(f.ctx, "operator", capped, ids[0], admin.WebhookPatch{Active: &off}, nil); err != nil {
			t.Fatal(err)
		}
		extra := f.create(capped, "https://extra.example.com/")
		if _, err := f.repo.UpdateWebhook(f.ctx, "operator", capped, ids[0], admin.WebhookPatch{Active: &on}, nil); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("enable beyond the cap", err)
		}
		if err := f.repo.DeleteWebhook(f.ctx, "operator", capped, extra.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.repo.UpdateWebhook(f.ctx, "operator", capped, ids[0], admin.WebhookPatch{Active: &on}, nil); err != nil {
			t.Fatal("a deleted endpoint kept its slot", err)
		}
	})

	t.Run("update", func(t *testing.T) {
		item := f.create(app, "https://update.example.com/a", "accepted")
		endpoint, description, empty, off, on := "https://update.example.com/b?x=1", "renamed", []string{}, false, true
		updated, err := f.repo.UpdateWebhook(f.ctx, "operator", app, item.ID, admin.WebhookPatch{Endpoint: &endpoint, Description: &description}, nil)
		if err != nil || updated.Endpoint != endpoint || updated.Description != description || len(updated.Events) != 1 || !updated.Active || !updated.SecretCreatedAt.Equal(*item.SecretCreatedAt) {
			t.Fatal("partial update", updated, err)
		}
		if updated, err = f.repo.UpdateWebhook(f.ctx, "operator", app, item.ID, admin.WebhookPatch{Events: &empty}, nil); err != nil || len(updated.Events) != 0 || updated.Endpoint != endpoint {
			t.Fatal("clearing the event filter", updated, err)
		}
		set := []string{"rejected", "clicked"}
		if updated, err = f.repo.UpdateWebhook(f.ctx, "operator", app, item.ID, admin.WebhookPatch{Events: &set}, nil); err != nil || len(updated.Events) != 2 {
			t.Fatal("setting the event filter", updated, err)
		}
		updatedBefore, disabledBefore, enabledBefore := audits("webhook.updated"), audits("webhook.disabled"), audits("webhook.enabled")
		if updated, err = f.repo.UpdateWebhook(f.ctx, "operator", app, item.ID, admin.WebhookPatch{Active: &off}, nil); err != nil || updated.Active || audits("webhook.updated") != updatedBefore || audits("webhook.disabled") != disabledBefore+1 {
			t.Fatal("disable", updated, err)
		}
		if updated, err = f.repo.UpdateWebhook(f.ctx, "operator", app, item.ID, admin.WebhookPatch{Active: &on}, nil); err != nil || !updated.Active || audits("webhook.enabled") != enabledBefore+1 {
			t.Fatal("enable", updated, err)
		}
		if _, err = f.repo.UpdateWebhook(f.ctx, "operator", app, item.ID, admin.WebhookPatch{Active: &on}, nil); err != nil || audits("webhook.enabled") != enabledBefore+1 {
			t.Fatal("repeating the active state is audited as a change", err)
		}
		bad := "http://update.example.com/"
		badEvents := []string{"bogus"}
		for name, patch := range map[string]admin.WebhookPatch{"empty": {}, "endpoint": {Endpoint: &bad}, "outcomes": {Events: &badEvents}} {
			if _, err = f.repo.UpdateWebhook(f.ctx, "operator", app, item.ID, patch, nil); !errors.Is(err, admin.ErrInvalidInput) {
				t.Errorf("%s patch accepted: %v", name, err)
			}
		}
		if _, err = f.repo.UpdateWebhook(f.ctx, "operator", other, item.ID, admin.WebhookPatch{Active: &off}, nil); !notFound(err) {
			t.Fatal("cross-app update", err)
		}
		f.exec(`UPDATE apps SET active=false WHERE id=$1`, app)
		if _, err = f.repo.UpdateWebhook(f.ctx, "operator", app, item.ID, admin.WebhookPatch{Active: &off}, nil); err != nil {
			t.Fatal("disabling under an inactive app", err)
		}
		if _, err = f.repo.UpdateWebhook(f.ctx, "operator", app, item.ID, admin.WebhookPatch{Active: &on}, nil); !notFound(err) {
			t.Fatal("enabling under an inactive app", err)
		}
		f.exec(`UPDATE apps SET active=true WHERE id=$1`, app)
		if err = f.repo.DeleteWebhook(f.ctx, "operator", app, item.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = f.repo.UpdateWebhook(f.ctx, "operator", app, item.ID, admin.WebhookPatch{Description: &description}, nil); !notFound(err) {
			t.Fatal("updating a deleted endpoint", err)
		}
	})

	t.Run("rotate and test target", func(t *testing.T) {
		item := f.create(app, "https://rotate.example.com/hook?k=v")
		rotated, err := f.repo.ReplaceWebhookSecret(f.ctx, "operator", app, item.ID, "thinpost", []byte("sealed-two"))
		if err != nil || !rotated.SecretCreatedAt.After(*item.SecretCreatedAt) {
			t.Fatal("rotate", rotated, err)
		}
		target, err := f.repo.WebhookTarget(f.ctx, "operator", app, item.ID)
		endpoint, sealed := target.Endpoint, target.SealedSecret
		if err != nil || endpoint != item.Endpoint || target.Format != "thinpost" || string(sealed) != "sealed-two" || audits("webhook.secret_rotated") != 1 || audits("webhook.tested") != 1 {
			t.Fatal("target", endpoint, string(sealed), err)
		}
		if _, err = f.repo.ReplaceWebhookSecret(f.ctx, "operator", other, item.ID, "thinpost", []byte("x")); !notFound(err) {
			t.Fatal("cross-app rotate", err)
		}
		if _, err = f.repo.WebhookTarget(f.ctx, "operator", other, item.ID); !notFound(err) {
			t.Fatal("cross-app target", err)
		}
		if err = f.repo.DeleteWebhook(f.ctx, "operator", app, item.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = f.repo.ReplaceWebhookSecret(f.ctx, "operator", app, item.ID, "thinpost", []byte("x")); !notFound(err) {
			t.Fatal("rotating a deleted endpoint", err)
		}
		if _, err = f.repo.WebhookTarget(f.ctx, "operator", app, item.ID); !notFound(err) {
			t.Fatal("test target for a deleted endpoint", err)
		}
	})

	t.Run("fan-out honors state and event filter and the claim carries the secret", func(t *testing.T) {
		fan := f.app("fan")
		all := f.create(fan, "https://all.example.com/")
		accepted := f.create(fan, "https://accepted.example.com/", "accepted")
		rejected := f.create(fan, "https://rejected.example.com/", "rejected")
		off := f.create(fan, "https://off.example.com/")
		gone := f.create(fan, "https://gone.example.com/")
		disable := false
		if _, err := f.repo.UpdateWebhook(f.ctx, "operator", fan, off.ID, admin.WebhookPatch{Active: &disable}, nil); err != nil {
			t.Fatal(err)
		}
		if err := f.repo.DeleteWebhook(f.ctx, "operator", fan, gone.ID); err != nil {
			t.Fatal(err)
		}
		event := f.acceptedEvent(fan)
		f.fanout(event, fan)
		rows, err := f.owner.QueryContext(f.ctx, `SELECT subscription_id FROM webhook_deliveries WHERE event_id=$1 ORDER BY subscription_id`, event)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got[id] = true
		}
		rows.Close()
		if len(got) != 2 || !got[all.ID] || !got[accepted.ID] || got[rejected.ID] {
			t.Fatal("fan-out reached the wrong endpoints", got)
		}
		// The unfiltered endpoint is claimed first only by queue order, so claim until
		// the filtered one is seen and check each carries the sealed secret.
		for range 2 {
			claim, claimErr := f.repo.ClaimWebhook(f.ctx, f.id(), time.Minute)
			if claimErr != nil || string(claim.SealedSecret) != "sealed" || claim.AppID != fan || !got[claim.SubscriptionID] {
				t.Fatal("claim", claim, claimErr)
			}
		}
	})

	t.Run("delete cancels pending work and keeps history", func(t *testing.T) {
		del := f.app("delete")
		item := f.create(del, "https://delete.example.com/")
		queued, inFlight, delivered, failed := f.event(del), f.event(del), f.event(del), f.event(del)
		token := f.id()
		for _, event := range []string{queued, inFlight, delivered, failed} {
			f.exec(`INSERT INTO webhook_deliveries(event_id,subscription_id,app_id) VALUES ($1,$2,$3)`, event, item.ID, del)
		}
		f.exec(`UPDATE webhook_deliveries SET state='in_flight',attempts=1,claim_token=$3,claim_until=clock_timestamp()+interval '1 minute' WHERE event_id=$1 AND subscription_id=$2`, inFlight, item.ID, token)
		f.exec(`INSERT INTO webhook_attempts(id,event_id,subscription_id) VALUES ($1,$2,$3)`, token, inFlight, item.ID)
		f.exec(`UPDATE webhook_deliveries SET state='delivered',attempts=1 WHERE event_id=$1`, delivered)
		f.exec(`UPDATE webhook_deliveries SET state='failed',attempts=12 WHERE event_id=$1`, failed)
		if err := f.repo.DeleteWebhook(f.ctx, "operator", other, item.ID); !notFound(err) {
			t.Fatal("cross-app delete", err)
		}
		if err := f.repo.DeleteWebhook(f.ctx, "operator", del, item.ID); err != nil {
			t.Fatal(err)
		}
		states := map[string]string{}
		rows, err := f.owner.QueryContext(f.ctx, `SELECT event_id,state||':'||COALESCE(claim_token,'') FROM webhook_deliveries WHERE subscription_id=$1`, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var event, state string
			if err = rows.Scan(&event, &state); err != nil {
				t.Fatal(err)
			}
			states[event] = state
		}
		rows.Close()
		if states[queued] != "cancelled:" || states[inFlight] != "cancelled:" || states[delivered] != "delivered:" || states[failed] != "failed:" {
			t.Fatal("delivery states", states)
		}
		var attemptState string
		var finished bool
		if err = f.owner.QueryRowContext(f.ctx, `SELECT state,finished_at IS NOT NULL FROM webhook_attempts WHERE id=$1`, token).Scan(&attemptState, &finished); err != nil || attemptState != "unknown" || !finished {
			t.Fatal("in-flight attempt", attemptState, finished, err)
		}
		var active bool
		var deletedAt sql.NullTime
		var ciphertext []byte
		if err = f.owner.QueryRowContext(f.ctx, `SELECT active,deleted_at,secret_ciphertext FROM webhook_subscriptions WHERE id=$1`, item.ID).Scan(&active, &deletedAt, &ciphertext); err != nil || active || !deletedAt.Valid || ciphertext != nil {
			t.Fatal("tombstone", active, deletedAt, ciphertext, err)
		}
		if items, listErr := f.repo.ListWebhooks(f.ctx, del); listErr != nil || len(items) != 0 {
			t.Fatal("deleted endpoint listed", items, listErr)
		}
		claim := webhook.Claim{AppID: del, SubscriptionID: item.ID, Token: token, Event: webhook.Event{ID: inFlight}}
		if err = f.repo.CommitWebhook(f.ctx, claim, 200); !errors.Is(err, webhook.ErrLostClaim) {
			t.Fatal("commit after delete", err)
		}
		if _, err = f.repo.ClaimWebhook(f.ctx, f.id(), time.Minute); !errors.Is(err, webhook.ErrNoWork) {
			t.Fatal("deleted endpoint claimed", err)
		}
		if err = f.repo.DeleteWebhook(f.ctx, "operator", del, item.ID); !notFound(err) {
			t.Fatal("second delete", err)
		}
		if err = f.repo.ReplayWebhookDelivery(f.ctx, "operator", del, failed, item.ID); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("replay to a deleted endpoint", err)
		}
		if audits("webhook.deleted") < 1 {
			t.Fatal("delete audit")
		}
	})

	t.Run("fan-out and delete race", func(t *testing.T) {
		race := f.app("race")
		item := f.create(race, "https://race.example.com/")
		event := f.event(race)
		// Connection A: an event transaction fans out and stays open.
		tx, err := f.repo.beginRuntime(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err = enqueueWebhookDeliveries(f.ctx, tx, event, race); err != nil {
			t.Fatal(err)
		}
		// Connection B: the delete must wait for A's share lock on the endpoint.
		done := make(chan error, 1)
		go func() { done <- f.repo.DeleteWebhook(f.ctx, "operator", race, item.ID) }()
		select {
		case err = <-done:
			t.Fatal("delete did not wait for the in-progress event transaction", err)
		case <-time.After(500 * time.Millisecond):
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		if n := f.count(`SELECT count(*) FROM webhook_deliveries WHERE subscription_id=$1 AND state IN ('queued','in_flight')`, item.ID); n != 0 {
			t.Fatal("deleted endpoint kept pending deliveries", n)
		}
		if n := f.count(`SELECT count(*) FROM webhook_deliveries WHERE subscription_id=$1 AND state='cancelled'`, item.ID); n != 1 {
			t.Fatal("racing delivery was not cancelled", n)
		}
	})
}

func TestWebhookEndpointsMigration(t *testing.T) {
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
	schema := "webhook_migration_" + hex.EncodeToString(random[:])
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
	for _, item := range migrations[:28] {
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
	// Five deliveries on one catalog-era endpoint; recovery_hold has an
	// in-flight attempt that the migration must leave alone.
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('app','app');
		INSERT INTO messages(id,app_id,object_key,enhanced_protection) VALUES ('m','app','k',true);
		INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ref,active) VALUES ('live','app','https://hooks.example.com/','ref',true),('off','app','https://off.example.com/','ref',false);
		INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) SELECT 'e'||n,'app','m','preparation_failed',0 FROM generate_series(1,5) n;
		INSERT INTO webhook_deliveries(event_id,subscription_id,app_id,state,claim_token,claim_until) VALUES
			('e1','live','app','queued',NULL,NULL),
			('e2','live','app','in_flight','tok2',clock_timestamp()+interval '1 minute'),
			('e3','live','app','failed',NULL,NULL),
			('e4','live','app','delivered',NULL,NULL),
			('e5','live','app','recovery_hold','tok5',clock_timestamp()+interval '1 minute');
		INSERT INTO webhook_attempts(id,event_id,subscription_id,state) VALUES ('tok2','e2','live','in_flight'),('tok5','e5','live','in_flight')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE runtime_gate SET mode='closed' WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var retired int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_subscriptions WHERE deleted_at IS NOT NULL AND NOT active AND secret_ciphertext IS NULL AND description='' AND events IS NULL`).Scan(&retired); err != nil || retired != 2 {
		t.Fatal("catalog-era endpoints were not retired", retired, err)
	}
	states := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT event_id,state||':'||COALESCE(claim_token,'') FROM webhook_deliveries`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var event, state string
		if err = rows.Scan(&event, &state); err != nil {
			t.Fatal(err)
		}
		states[event] = state
	}
	rows.Close()
	if states["e1"] != "cancelled:" || states["e2"] != "cancelled:" || states["e3"] != "failed:" || states["e4"] != "delivered:" || states["e5"] != "recovery_hold:tok5" {
		t.Fatal("delivery states", states)
	}
	attempts := map[string]string{}
	rows, err = db.QueryContext(ctx, `SELECT id,state||':'||(finished_at IS NOT NULL) FROM webhook_attempts`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, state string
		if err = rows.Scan(&id, &state); err != nil {
			t.Fatal(err)
		}
		attempts[id] = state
	}
	rows.Close()
	if attempts["tok2"] != "unknown:true" || attempts["tok5"] != "in_flight:false" {
		t.Fatal("attempt states", attempts)
	}
	var column bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='webhook_subscriptions' AND column_name='secret_ref')`).Scan(&column); err != nil || column {
		t.Fatal("secret_ref was not dropped", err)
	}
	for name, statement := range map[string]string{
		"live endpoint without a secret": `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url) VALUES ('x1','app','https://x.example.com/')`,
		"active tombstone":               `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,deleted_at,active) VALUES ('x2','app','https://x.example.com/',now(),true)`,
		"long description":               `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext,description) VALUES ('x3','app','https://x.example.com/','\x01',repeat('d',257))`,
		"empty events":                   `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext,events) VALUES ('x4','app','https://x.example.com/','\x01','{}')`,
		"oversized secret":               `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext) VALUES ('x5','app','https://x.example.com/',decode(repeat('00',1025),'hex'))`,
	} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
