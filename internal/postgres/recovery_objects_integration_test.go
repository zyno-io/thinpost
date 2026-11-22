package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/prepared"
)

type recoveryRaw map[string][]byte

func (r recoveryRaw) Put(_ context.Context, key string, body []byte) error {
	if _, exists := r[key]; exists {
		return errors.New("duplicate")
	}
	r[key] = append([]byte(nil), body...)
	return nil
}
func (r recoveryRaw) Get(_ context.Context, key string, max int64) ([]byte, error) {
	body, exists := r[key]
	if !exists || int64(len(body)) > max {
		return nil, errors.New("unavailable")
	}
	return append([]byte(nil), body...), nil
}
func (r recoveryRaw) VerifyPresent(_ context.Context, key string) error {
	if _, exists := r[key]; !exists {
		return errors.New("unavailable")
	}
	return nil
}

func TestRecoveryObjectInventory(t *testing.T) {
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
	schema := "recovery_objects_" + hex.EncodeToString(random[:])
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
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	const gen = "11111111111111111111111111111111"
	const run = "22222222222222222222222222222222"
	if err = RecoveryBegin(ctx, db, 1, gen, run, "operator"); err != nil {
		t.Fatal(err)
	}
	if complete, qerr := RecoveryQuarantineBatch(ctx, db, gen, run, "operator"); qerr != nil || !complete {
		t.Fatal(complete, qerr)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('app','app');
		INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('m1','app','source/1',true,'done'),('m2','app','source/2',true,'done');
		INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,prepared_format) VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','m1',0,'final/1','accepted',1),('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','m2',0,'final/2','accepted',2);
		INSERT INTO bounce_receipts(id,delivery_id,object_key,fingerprint) VALUES ('b1','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','bounce/1',decode(repeat('00',32),'hex'));
		INSERT INTO inbound_messages(id,app_id,domain,object_key,size_bytes,fingerprint,content_expires_at) VALUES
			('11111111111111111111111111111111','app','example.com','inbound/v1/0000000000/11111111111111111111111111111111',10,decode(repeat('01',32),'hex'),now()+interval '30 days'),
			('22222222222222222222222222222222','app','example.com','inbound/v1/0000000000/22222222222222222222222222222222',10,decode(repeat('02',32),'hex'),now()-interval '1 day');
		INSERT INTO tracking_objects(delivery_id,object_key,expires_at,event_limit) VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','tracking/v1/0000000000/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',now(),1);
		INSERT INTO tracking_objects(delivery_id,object_key,retired_at,expires_at,event_limit) VALUES ('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',NULL,now(),now(),1);`); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyFile := filepath.Join(t.TempDir(), "keyring.json")
	if err = os.WriteFile(keyFile, []byte(fmt.Sprintf(`{"active":"v1","keys":{"v1":%q}}`, key)), 0600); err != nil {
		t.Fatal(err)
	}
	ring, err := objectstore.LoadKeyring(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	raw := recoveryRaw{}
	store, err := objectstore.NewEncrypted(raw, ring)
	if err != nil {
		t.Fatal(err)
	}
	// final/2 is a recipe object (delivery bbbb... is prepared_format 2): the
	// inventory reads it like any other authenticated object.
	recipe, err := prepared.New("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", strings.Repeat("0", 64), 0, []byte("DKIM-Signature: v=1\r\n\r\nbody\r\n"), len("DKIM-Signature: v=1\r\n\r\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	recipeObject, err := recipe.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Put(ctx, "final/2", recipeObject); err != nil {
		t.Fatal(err)
	}
	for _, objectKey := range []string{"source/1", "source/2", "final/1", "bounce/1", "inbound/v1/0000000000/11111111111111111111111111111111", "tracking/v1/0000000000/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if err = store.Put(ctx, objectKey, []byte("synthetic message")); err != nil {
			t.Fatal(err)
		}
	}
	verify := func(ctx context.Context, key string) error {
		body, readErr := store.Get(ctx, key, 16<<20)
		clear(body)
		return readErr
	}
	if _, err = RecoveryObjectInventoryPage(ctx, db, gen, run, 2, RecoveryObjectCursor{}, 2, verify); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("accepted wrong revision", err)
	}
	for _, cursor := range []RecoveryObjectCursor{{Kind: "message"}, {ID: "m1"}, {Kind: "bogus", ID: "x"}} {
		if _, err = RecoveryObjectInventoryPage(ctx, db, gen, run, 3, cursor, 2, verify); err == nil {
			t.Fatal("accepted invalid cursor", cursor)
		}
	}
	var cursor RecoveryObjectCursor
	checked := 0
	for {
		page, pageErr := RecoveryObjectInventoryPage(ctx, db, gen, run, 3, cursor, 2, verify)
		if pageErr != nil || len(page.Failed) != 0 {
			t.Fatal("inventory failed", pageErr, page.Failed)
		}
		checked += page.Checked
		if page.Next == nil {
			break
		}
		cursor = *page.Next
	}
	// The expired inbound message's object was never stored; it is skipped, not failed.
	if checked != 7 {
		t.Fatal("inventory omitted or repeated references", checked)
	}
	delete(raw, "bounce/1")
	raw["source/1"][len(raw["source/1"])-1] ^= 1
	page, err := RecoveryObjectInventoryPage(ctx, db, gen, run, 3, RecoveryObjectCursor{}, 100, verify)
	if err != nil || page.Checked != 7 || len(page.Failed) != 2 || page.Failed[0].Kind != "bounce" || page.Failed[1].Kind != "message" {
		t.Fatal("missing or corrupt objects not detected", page, err)
	}
	wrongKey := base64.StdEncoding.EncodeToString(append([]byte{1}, make([]byte, 31)...))
	if err = os.WriteFile(keyFile, []byte(fmt.Sprintf(`{"active":"v1","keys":{"v1":%q}}`, wrongKey)), 0600); err != nil {
		t.Fatal(err)
	}
	wrongRing, err := objectstore.LoadKeyring(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	wrongStore, err := objectstore.NewEncrypted(raw, wrongRing)
	if err != nil {
		t.Fatal(err)
	}
	page, err = RecoveryObjectInventoryPage(ctx, db, gen, run, 3, RecoveryObjectCursor{}, 100, func(ctx context.Context, key string) error {
		_, readErr := wrongStore.Get(ctx, key, 16<<20)
		return readErr
	})
	if err != nil || len(page.Failed) != 7 {
		t.Fatal("wrong key accepted", page, err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('','app','source/invalid',true,'done')`); err != nil {
		t.Fatal(err)
	}
	if _, err = RecoveryObjectInventoryPage(ctx, db, gen, run, 3, RecoveryObjectCursor{}, 100, verify); err == nil {
		t.Fatal("invalid empty reference ID was silently omitted")
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM messages WHERE id=''`); err != nil {
		t.Fatal(err)
	}
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, pageErr := RecoveryObjectInventoryPage(ctx, db, gen, run, 3, RecoveryObjectCursor{}, 1, func(context.Context, string) error { close(started); <-release; return nil })
		finished <- pageErr
	}()
	<-started
	opened := make(chan error, 1)
	go func() { opened <- RecoveryOpen(ctx, db, gen, run, "operator", 3) }()
	select {
	case err = <-opened:
		t.Fatal("gate opened during inventory", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	if err = <-opened; err != nil {
		t.Fatal(err)
	}
	if _, err = RecoveryObjectInventoryPage(ctx, db, gen, run, 3, RecoveryObjectCursor{}, 2, verify); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatal("continued after gate opened", err)
	}
}
