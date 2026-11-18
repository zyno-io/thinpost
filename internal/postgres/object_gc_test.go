package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/objectkey"
)

func testObjectCollection(t *testing.T, db *sql.DB, repo, collector *Repository, generation string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const app = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeea0"
	const message = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeea1"
	const delivery = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeea2"
	const bounce = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeea3"
	old := time.Now().Add(-48 * time.Hour)
	key := func(kind, id string, at time.Time) string {
		result, err := objectkey.New(kind, id, at)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	sourceKey, finalKey, bounceKey := key("submissions", message, old), key("final", delivery, old), key("bounces", bounce, old)
	if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'gc test')`, app); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,$3,false,'prepared')`, message, app, sourceKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,$3)`, delivery, message, finalKey); err != nil {
		t.Fatal(err)
	}
	inboundKey := key("inbound", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeea7", old)
	if _, err := db.ExecContext(ctx, `INSERT INTO inbound_messages(id,app_id,domain,object_key,size_bytes,fingerprint,content_expires_at) VALUES ('eeeeeeeeeeeeeeeeeeeeeeeeeeeeeea7',$1,'example.com',$2,10,decode(repeat('00',32),'hex'),now()+interval '30 days')`, app, inboundKey); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("gc-bounce"))
	if _, err := db.ExecContext(ctx, `INSERT INTO bounce_receipts(id,delivery_id,object_key,fingerprint) VALUES ($1,$2,$3,$4)`, bounce, delivery, bounceKey, hash[:]); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		generation string
		retention  time.Duration
	}{{generation, time.Hour}, {"00000000000000000000000000000bbb", 24 * time.Hour}} {
		if _, err := AdvanceObjectGCHorizon(ctx, db, invalid.generation, "gc test", invalid.retention); err == nil {
			t.Fatal("invalid horizon request succeeded")
		}
	}
	if _, err := AdvanceObjectGCHorizon(ctx, collector.DB, generation, "gc test", 24*time.Hour); err == nil {
		t.Fatal("collector identity advanced collection horizon")
	}
	writer, err := repo.beginRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	blockedCtx, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err = AdvanceObjectGCHorizon(blockedCtx, db, generation, "gc test", 24*time.Hour)
	deadlineReached := errors.Is(blockedCtx.Err(), context.DeadlineExceeded)
	stop()
	if err == nil || !deadlineReached {
		t.Fatal("horizon did not wait for admitted writer", err)
	}
	if err = writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	horizon, err := AdvanceObjectGCHorizon(ctx, db, generation, "gc test", 24*time.Hour)
	if err != nil || !horizon.After(old) || horizon.After(time.Now().Add(-23*time.Hour)) {
		t.Fatal("maintenance could not establish safe horizon", err)
	}
	testObjectCollectionEndToEnd(t, collector, horizon, sourceKey, key("submissions", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeea5", old), key("submissions", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeea6", old))
	removed := 0
	remove := func(context.Context, string, time.Time) (bool, error) { removed++; return true, nil }
	for _, referenced := range []string{sourceKey, finalKey, bounceKey, inboundKey, key("submissions", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeea4", time.Now())} {
		deleted, err := collector.CollectUnreferenced(ctx, referenced, horizon, remove)
		if err != nil || deleted {
			t.Fatal("referenced or recent object was removed", err)
		}
	}
	if removed != 0 {
		t.Fatal("remove callback reached for a protected object")
	}
	orphan := key("submissions", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeea5", old)
	if _, err = collector.CollectUnreferenced(ctx, orphan, horizon.Add(-time.Second), remove); err == nil || removed != 0 {
		t.Fatal("collector accepted a stale horizon", err)
	}
	if deleted, err := collector.CollectUnreferenced(ctx, orphan, horizon, remove); err != nil || !deleted || removed != 1 {
		t.Fatal("orphan was not removed", err)
	}
	if _, err = collector.CollectUnreferenced(ctx, "submissions/legacy", horizon, remove); err == nil {
		t.Fatal("legacy object admitted to collection")
	}
	finishDelete, deletedLater := make(chan struct{}), make(chan struct{})
	if _, err = collector.CollectUnreferenced(ctx, orphan, horizon, func(context.Context, string, time.Time) (bool, error) {
		go func() { <-finishDelete; close(deletedLater) }()
		return false, errors.New("secret object details")
	}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("ambiguous deletion did not fail closed or leaked details", err)
	}
	record := withMetadata(acceptance.Record{AppID: "a", KeyID: "key-a", ObjectKey: orphan, From: "sender@example.com", Adapter: "resend", RecipientCount: 1})
	if _, err = repo.Commit(ctx, record); err == nil || err.Error() != "object reference window expired" {
		t.Fatal("writer referenced an object after an ambiguous deletion", err)
	}
	close(finishDelete)
	<-deletedLater
	var referenced bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE object_key=$1)`, orphan).Scan(&referenced); err != nil || referenced {
		t.Fatal("late deletion could affect a committed reference", err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := collector.CollectUnreferenced(ctx, orphan, horizon, func(ctx context.Context, _ string, _ time.Time) (bool, error) {
			close(entered)
			select {
			case <-release:
				return true, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("collector did not enter deletion")
	}
	closer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = closer.ExecContext(ctx, `SET LOCAL lock_timeout='150ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err = closer.ExecContext(ctx, `UPDATE runtime_gate SET mode='closed' WHERE singleton`); err == nil {
		t.Fatal("gate closed while collector held it")
	}
	_ = closer.Rollback()
	close(release)
	if err = <-done; err != nil {
		t.Fatal("collector did not release the gate", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE runtime_gate SET mode='closed' WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if _, err = collector.CollectUnreferenced(ctx, orphan, horizon, remove); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("collector was admitted during recovery", err)
	}
	if _, err = AdvanceObjectGCHorizon(ctx, db, generation, "gc test", 24*time.Hour); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("horizon advanced with closed gate", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE runtime_gate SET mode='open' WHERE singleton`); err != nil {
		t.Fatal(err)
	}
}
