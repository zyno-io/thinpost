package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/acceptance"
)

// migratedSchemaForTest returns a fresh, fully migrated schema in the
// disposable database. The connection's search_path is that schema.
func migratedSchemaForTest(t *testing.T, ctx context.Context, prefix string) *sql.DB {
	t.Helper()
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL database")
	}
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
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMessageMetadataSchemaConstraints(t *testing.T) {
	ctx := context.Background()
	db := migratedSchemaForTest(t, ctx, "metadata_schema_")
	if version, err := SchemaVersion(ctx, db); err != nil || version != len(migrations) || version < MessageMetadataSchema {
		t.Fatal("schema version", version, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('a','A'),('b','B')`); err != nil {
		t.Fatal(err)
	}
	// A legacy row has no metadata at all.
	if _, err := db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection) VALUES ('legacy','a','k0',false)`); err != nil {
		t.Fatal("unrecorded metadata rejected", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev) VALUES ('full','a','k1',false,'hi',1,'example.com','com.example.')`); err != nil {
		t.Fatal("recorded metadata rejected", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE messages SET sender_address='U@Example.com', sender_name='U' WHERE id='full'`); err != nil {
		t.Fatal("sender rejected", err)
	}
	for name, statement := range map[string]string{
		"sender address too short":           `UPDATE messages SET sender_address='a@' WHERE id='full'`,
		"sender address too long":            `UPDATE messages SET sender_address=repeat('a',999) WHERE id='full'`,
		"empty sender name":                  `UPDATE messages SET sender_name='' WHERE id='full'`,
		"sender name too long":               `UPDATE messages SET sender_name=repeat('n',999) WHERE id='full'`,
		"sender name without address":        `UPDATE messages SET sender_address=NULL WHERE id='full'`,
		"sender address without domain":      `INSERT INTO messages(id,app_id,object_key,enhanced_protection,sender_address) VALUES ('x0','a','k',false,'u@example.com')`,
		"subject without count":              `INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,sender_domain,sender_domain_rev) VALUES ('x1','a','k',false,'hi','example.com','com.example.')`,
		"sender domain without reversed":     `INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain) VALUES ('x2','a','k',false,'hi',1,'example.com')`,
		"sender domain without subject":      `INSERT INTO messages(id,app_id,object_key,enhanced_protection,sender_domain,sender_domain_rev) VALUES ('x3','a','k',false,'example.com','com.example.')`,
		"reversed length mismatch":           `INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev) VALUES ('x4','a','k',false,'hi',1,'example.com','com.example')`,
		"empty subject":                      `INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev) VALUES ('x5','a','k',false,'',1,'example.com','com.example.')`,
		"recipient count zero":               `INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev) VALUES ('x6','a','k',false,'hi',0,'example.com','com.example.')`,
		"recipient count above 100":          `INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev) VALUES ('x7','a','k',false,'hi',101,'example.com','com.example.')`,
		"recipient with mismatched app":      `INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ('full','b',0,'u@example.com','u@example.com','example.com','com.example.','to')`,
		"recipient reversed length mismatch": `INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ('full','a',0,'u@example.com','u@example.com','example.com','com.example','to')`,
		"recipient index above 99":           `INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ('full','a',100,'u@example.com','u@example.com','example.com','com.example.','to')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err == nil {
			t.Fatal("constraint did not reject:", name)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ('full','a',0,'U@Example.com','u@example.com','example.com','com.example.','to')`); err != nil {
		t.Fatal(err)
	}
}

// Extension routines are PUBLIC-executable and fail the collector and
// retirement privilege audits, so migrations must not install any routine that
// is not a trigger function.
func TestMigrationsInstallNoExecutableRoutines(t *testing.T) {
	ctx := context.Background()
	db := migratedSchemaForTest(t, ctx, "metadata_routines_")
	var count int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname=current_schema() AND p.prorettype<>'pg_catalog.trigger'::regtype AND has_function_privilege('public',p.oid,'EXECUTE')`).Scan(&count)
	if err != nil || count != 0 {
		t.Fatal("non-trigger routine executable by PUBLIC", count, err)
	}
	var extensions int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_extension WHERE extname<>'plpgsql'`).Scan(&extensions); err != nil || extensions != 0 {
		t.Fatal("unexpected extension", extensions, err)
	}
}

// withMetadata fills the recorded-metadata fields that acceptance derives from
// a validated message, using synthetic values.
func withMetadata(record acceptance.Record) acceptance.Record {
	item := withItemMetadata(acceptance.BatchItem{From: record.From, RecipientCount: record.RecipientCount})
	record.Subject, record.Recipients, record.RecipientKinds, record.SenderDomain = item.Subject, item.Recipients, item.RecipientKinds, item.SenderDomain
	record.SenderAddress, record.SenderName = item.SenderAddress, item.SenderName
	return record
}

func withItemMetadata(item acceptance.BatchItem) acceptance.BatchItem {
	item.Subject = "Fixture subject"
	item.Recipients = make([]string, item.RecipientCount)
	item.RecipientKinds = toKinds(item.RecipientCount)
	for i := range item.Recipients {
		item.Recipients[i] = fmt.Sprintf("recipient%d@Recipient.example", i)
	}
	if at := strings.LastIndexByte(item.From, '@'); at >= 0 {
		item.SenderDomain = strings.ToLower(item.From[at+1:])
		item.SenderAddress, item.SenderName = item.From, "Fixture Sender"
	}
	return item
}
