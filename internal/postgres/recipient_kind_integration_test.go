package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
)

// recipientRows accepts only a To entry first, then Cc, then Bcc, with a known
// kind on every entry.
func TestRecipientRowsKindOrder(t *testing.T) {
	addresses := []string{"a@example.net", "b@example.net", "c@example.net", "d@example.net"}
	for name, tc := range map[string]struct {
		kinds []string
		ok    bool
	}{
		"to cc bcc":           {[]string{"to", "to", "cc", "bcc"}, true},
		"all to":              {[]string{"to", "to", "to", "to"}, true},
		"to bcc bcc bcc":      {[]string{"to", "bcc", "bcc", "bcc"}, true},
		"cc first":            {[]string{"cc", "to", "to", "to"}, false},
		"bcc before cc":       {[]string{"to", "bcc", "cc", "cc"}, false},
		"cc after bcc":        {[]string{"to", "to", "bcc", "cc"}, false},
		"to after cc":         {[]string{"to", "cc", "to", "to"}, false},
		"unknown kind":        {[]string{"to", "to", "cc", "reply"}, false},
		"empty kind":          {[]string{"to", "to", "to", ""}, false},
		"length mismatch":     {[]string{"to", "to", "to"}, false},
		"upper case is wrong": {[]string{"to", "to", "TO", "to"}, false},
	} {
		var rows recipientRows
		if got := rows.add("m", addresses, tc.kinds, len(addresses)); got != tc.ok {
			t.Errorf("%s: add returned %v", name, got)
		}
	}
}

// migrationIndexOf finds a registered migration by file name suffix; a test can
// apply every migration before message_recipients
// gains its kind column and create rows as an older release did.
func migrationIndexOf(t *testing.T, suffix string) int {
	t.Helper()
	for i, item := range migrations {
		if strings.HasSuffix(item.file, suffix) {
			return i
		}
	}
	t.Fatal("migration not registered:", suffix)
	return -1
}

func TestMigrationRecipientKindKeepsExistingRowsAsTo(t *testing.T) {
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
	schema := "recipient_kind_mig_" + hex.EncodeToString(random[:])
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
	target := migrationIndexOf(t, "_recipient_kind.sql")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:target] {
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
		INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev) VALUES ('m1','app','k1',false,'hi',2,'example.com','com.example.');
		INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev) VALUES
			('m1','app',0,'a@example.net','a@example.net','example.net','net.example.'),
			('m1','app',1,'b@example.net','b@example.net','example.net','net.example.')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM message_recipients WHERE kind='to'`).Scan(&count); err != nil || count != 2 {
		t.Fatal("existing recipients must read as To", count, err)
	}
	for name, statement := range map[string]string{
		"insert without a kind": `INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev) VALUES ('m1','app',2,'c@example.net','c@example.net','example.net','net.example.')`,
		"unknown kind":          `INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ('m1','app',2,'c@example.net','c@example.net','example.net','net.example.','reply')`,
	} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Fatal("accepted:", name)
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ('m1','app',2,'c@example.net','c@example.net','example.net','net.example.','bcc')`); err != nil {
		t.Fatal(err)
	}
}

func TestRecipientKindsInHistorySearchAndWebhookContext(t *testing.T) {
	f := newWebhookFixture(t, "recipient_kind_")
	app, message := f.app("kinds"), f.id()
	addresses := []string{"to1@example.net", "to2@example.net", "cc@example.org", "bcc@example.com"}
	kinds := []string{"to", "to", "cc", "bcc"}
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev,sender_address,sender_name) VALUES ($1,$2,'k'||$1,false,'Kinds',4,'sender.example','example.sender.','s@sender.example','S')`, message, app)
	var rows recipientRows
	if !rows.add(message, addresses, kinds, 4) {
		t.Fatal("fixture recipients rejected")
	}
	tx, err := f.owner.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = insertRecipients(f.ctx, tx, app, &rows); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var deliveries, attempts []string
	for i := range addresses {
		delivery, attempt := f.id(), f.id()
		f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,$3,'f'||$1)`, delivery, message, i)
		f.exec(`INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch) VALUES ($1,$2,'w',1)`, attempt, delivery)
		deliveries, attempts = append(deliveries, delivery), append(attempts, attempt)
	}

	detail, err := f.repo.GetMessage(f.ctx, "operator", app, message)
	if err != nil || !reflect.DeepEqual(detail.Message.Recipients, addresses) || !reflect.DeepEqual(detail.Message.RecipientKinds, kinds) || len(detail.Deliveries) != 4 {
		t.Fatalf("detail recipients %v kinds %v: %v", detail.Message.Recipients, detail.Message.RecipientKinds, err)
	}
	for i, d := range detail.Deliveries {
		if d.Recipient == nil || *d.Recipient != addresses[i] || d.RecipientKind == nil || *d.RecipientKind != kinds[i] {
			t.Fatalf("delivery %d: recipient and kind not reported", i)
		}
	}
	page, err := f.repo.ListMessages(f.ctx, "operator", app, "", 10)
	if err != nil || len(page.Messages) != 1 || !reflect.DeepEqual(page.Messages[0].Recipients, addresses[:3]) || !reflect.DeepEqual(page.Messages[0].RecipientKinds, kinds[:3]) {
		t.Fatalf("list: %+v %v", page.Messages, err)
	}
	// Search by recipient matches Cc and Bcc addresses and domains too.
	for field, term := range map[string]string{admin.SearchFieldRecipient: "bcc@example.com", admin.SearchFieldRecipientDomain: "example.org"} {
		found, err := f.repo.SearchMessages(f.ctx, "operator", app, admin.MessageQuery{Text: term, Field: field, Limit: 10})
		if err != nil || len(found.Messages) != 1 {
			t.Fatalf("search %s: %v", field, err)
		}
	}

	// A message-level webhook event names the To recipients only; a delivery
	// event names that delivery's own recipient, Cc and Bcc included.
	failed, accepted := f.id(), f.id()
	f.exec(`INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ($1,$2,$3,'preparation_failed',0)`, failed, app, message)
	f.exec(`INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES ($1,$2,$3,$4,$5,'accepted',250)`, accepted, app, message, deliveries[3], attempts[3])
	read, err := f.owner.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback()
	context, err := loadWebhookContext(f.ctx, read, failed, app)
	if err != nil || !reflect.DeepEqual(context.Recipients, addresses[:2]) {
		t.Fatalf("message-level event recipients %v: %v", context.Recipients, err)
	}
	if context, err = loadWebhookContext(f.ctx, read, accepted, app); err != nil || !reflect.DeepEqual(context.Recipients, []string{"bcc@example.com"}) {
		t.Fatalf("delivery event recipients %v: %v", context.Recipients, err)
	}
}
