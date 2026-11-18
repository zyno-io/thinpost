package dispatch

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/submission"
)

type sourceObjects struct{ source []byte }

func (o sourceObjects) Get(_ context.Context, key string, _ int64) ([]byte, error) {
	if strings.HasPrefix(key, "source-") {
		return o.source, nil
	}
	return []byte("signed MIME"), nil
}

// A suppressed delivery followed by a clean one must come back from Next with
// the attempt ID the dispatcher generated, against the real repository.
func TestNextAfterSuppressedDeliveryKeepsCallerAttemptID(t *testing.T) {
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
	schema := "dispatch_supp_" + hex.EncodeToString(random[:])
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
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = postgres.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	const generation = "00000000000000000000000000000ccc"
	n := 0
	id := func() string { n++; return fmt.Sprintf("%032x", n+5000) }
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	app := id()
	exec(`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation)
	exec(`INSERT INTO apps(id,name) VALUES ($1,'One')`, app)
	exec(`INSERT INTO suppressions(id,app_id,address,address_lower,domain_lower,reason,origin) VALUES ($1,$2,'blocked@example.net','blocked@example.net','example.net','manual','console')`, id(), app)
	add := func(address string, age int) string {
		m, d := id(), id()
		exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,$3,false,'prepared')`, m, app, "source-"+m)
		exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,retry_after) VALUES ($1,$2,0,$3,now()-$4*interval '1 second')`, d, m, "signed-"+d, age)
		exec(`INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ($1,$2,0,$3,$3,'example.net','net.example.','to')`, m, app, address)
		return d
	}
	add("blocked@example.net", 20)
	clean := add("clean@example.net", 10)
	source, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{1, submission.Message{From: "a@example.com", To: []string{"clean@example.net"}, Subject: "hello", Text: "body"}})
	if err != nil {
		t.Fatal(err)
	}
	repo := &postgres.Repository{DB: db, Generation: generation}
	lease, err := repo.AcquireControllerLease(ctx, "worker", "controller", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{Repository: repo, Objects: sourceObjects{source}, Routes: routes{}, Envelopes: envelopes{}, Recipients: allowRecipients(true)}
	start, mime, err := p.Next(ctx, lease, delivery.EdgeCapabilities{})
	if err != nil || start == nil || start.DeliveryId != clean || len(mime) == 0 {
		t.Fatal("clean delivery after a suppressed one was not dispatched", start, err)
	}
	var state string
	if err = db.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE id=$1`, clean).Scan(&state); err != nil || state != "in_flight" {
		t.Fatal("clean delivery state", state, err)
	}
	var attempts int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM delivery_attempts WHERE id=$1 AND delivery_id=$2`, start.AttemptId, clean).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatal("dispatched attempt is not the recorded one", attempts, err)
	}
}
