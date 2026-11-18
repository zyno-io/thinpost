package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/submission"
)

type memoryObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (o *memoryObjectStore) Put(_ context.Context, key string, data []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.objects == nil {
		o.objects = map[string][]byte{}
	}
	o.objects[key] = append([]byte(nil), data...)
	return nil
}

type allowAllRecipients struct{}

func (allowAllRecipients) Allows(string) bool { return true }

func TestTrackingDefaultsPostgres(t *testing.T) {
	f := newWebhookFixture(t, "trackdef_")
	ctx := f.ctx

	t.Run("defaults are off, audited, idempotent, gated and under the open gate", func(t *testing.T) {
		app := f.app("defaults")
		if got, err := f.repo.GetTrackingDefaults(ctx, app); err != nil || got != (admin.TrackingDefaults{}) {
			t.Fatal("default", got, err)
		}
		if _, err := f.repo.GetTrackingDefaults(ctx, f.id()); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("unknown app", err)
		}
		if _, err := f.repo.GetTrackingDefaults(ctx, "bad"); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatal("malformed app", err)
		}
		if _, err := f.repo.SetTrackingDefaults(ctx, "operator", f.id(), admin.TrackingDefaults{Opens: true}, true); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("set on unknown app", err)
		}
		if _, err := f.repo.SetTrackingDefaults(ctx, "operator", "bad", admin.TrackingDefaults{}, true); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatal("malformed app", err)
		}
		audits := func() int {
			return f.count(`SELECT count(*) FROM audit_events WHERE action='app.tracking_defaults.updated' AND app_id=$1`, app)
		}
		if got, err := f.repo.SetTrackingDefaults(ctx, "operator", app, admin.TrackingDefaults{Opens: true}, true); err != nil || got != (admin.TrackingDefaults{Opens: true}) || audits() != 1 {
			t.Fatal("enable", got, err)
		}
		if _, err := f.repo.SetTrackingDefaults(ctx, "operator", app, admin.TrackingDefaults{Opens: true}, true); err != nil || audits() != 1 {
			t.Fatal("identical value audited", err)
		}
		var details string
		if err := f.owner.QueryRowContext(ctx, `SELECT details::text FROM audit_events WHERE action='app.tracking_defaults.updated' AND app_id=$1`, app).Scan(&details); err != nil || details != `{"opens": true, "clicks": false, "previous": {"opens": false, "clicks": false}}` {
			t.Fatal("audit details", details, err)
		}
		// Without the runtime: turning one on is a conflict (no change, no audit);
		// an unchanged "on" and any turn-off are allowed.
		if _, err := f.repo.SetTrackingDefaults(ctx, "operator", app, admin.TrackingDefaults{Opens: true, Clicks: true}, false); !errors.Is(err, admin.ErrConflict) || audits() != 1 {
			t.Fatal("enable without runtime", err)
		}
		if got, _ := f.repo.GetTrackingDefaults(ctx, app); got != (admin.TrackingDefaults{Opens: true}) {
			t.Fatal("conflict changed the stored value", got)
		}
		if _, err := f.repo.SetTrackingDefaults(ctx, "operator", app, admin.TrackingDefaults{Opens: true}, false); err != nil || audits() != 1 {
			t.Fatal("unchanged on refused without runtime", err)
		}
		f.exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
		_, err := f.repo.SetTrackingDefaults(ctx, "operator", app, admin.TrackingDefaults{}, true)
		f.exec(`UPDATE runtime_gate SET mode='open' WHERE singleton`)
		if !errors.Is(err, ErrRuntimeClosed) {
			t.Fatal("changed under a closed gate", err)
		}
		if got, err := f.repo.SetTrackingDefaults(ctx, "operator", app, admin.TrackingDefaults{}, false); err != nil || got != (admin.TrackingDefaults{}) || audits() != 2 {
			t.Fatal("disable without runtime", got, err)
		}
	})

	t.Run("authentication carries the defaults and acceptance stores the resolved choice", func(t *testing.T) {
		app := f.app("accepting")
		f.exec(`UPDATE apps SET default_track_opens=true,default_track_clicks=true,submission_rate_per_minute=60000,submission_burst=100,submission_tokens=6000000000,recipient_daily_limit=1000,pending_message_limit=1000 WHERE id=$1`, app)
		digest := sha256.Sum256([]byte("key-" + app))
		f.exec(`INSERT INTO api_keys(id,digest,app_id) VALUES ($1,$2,$3)`, f.id(), digest[:], app)
		got, err := f.repo.LookupKey(ctx, digest)
		if err != nil || got.ID != app || !got.DefaultTrackOpens || !got.DefaultTrackClicks {
			t.Fatal("lookup", got, err)
		}
		f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, app)
		objects := &memoryObjectStore{}
		repo := &Repository{DB: f.repo.DB, Generation: f.repo.Generation, Objects: objectVerifierFunc(func(context.Context, string) error { return nil })}
		service, err := acceptance.New(repo, objects, []byte("0123456789abcdef0123456789abcdef"), allowAllRecipients{})
		if err != nil {
			t.Fatal(err)
		}
		service.ApplyTrackingDefaults = true
		req := submission.Request{Adapter: "resend", Message: submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "t", HTML: `<a href="https://example.net">x</a>`}}
		receipt, err := service.Accept(ctx, got, req)
		if err != nil {
			t.Fatal(err)
		}
		var key string
		if err = f.owner.QueryRowContext(ctx, `SELECT object_key FROM messages WHERE id=$1`, receipt.ID).Scan(&key); err != nil {
			t.Fatal(err)
		}
		check := func(raw []byte) {
			t.Helper()
			var source struct{ Message submission.Message }
			if err := json.Unmarshal(raw, &source); err != nil || source.Message.Tracking == nil || !source.Message.Tracking.Opens || !source.Message.Tracking.Clicks || strings.Contains(string(raw), "TrackingOff") {
				t.Fatal("stored source", err)
			}
		}
		stored := append([]byte(nil), objects.objects[key]...)
		check(stored)
		// A later default change never touches the stored source.
		if _, err = f.repo.SetTrackingDefaults(ctx, "operator", app, admin.TrackingDefaults{}, false); err != nil {
			t.Fatal(err)
		}
		if string(objects.objects[key]) != string(stored) {
			t.Fatal("stored source changed")
		}
		check(objects.objects[key])
	})
}

// TestMigration037 applies migrations through 36 over an existing app, then
// migrates: both defaults are false.
func TestMigration037(t *testing.T) {
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
	schema := "migration37_" + hex.EncodeToString(random[:])
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
	if migrations[36].version != 37 {
		t.Fatal("this test targets migration 37")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:36] {
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('app','test')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var opens, clicks bool
	if err = db.QueryRowContext(ctx, `SELECT default_track_opens,default_track_clicks FROM apps WHERE id='app'`).Scan(&opens, &clicks); err != nil || opens || clicks {
		t.Fatal("existing app defaults", opens, clicks, err)
	}
}
