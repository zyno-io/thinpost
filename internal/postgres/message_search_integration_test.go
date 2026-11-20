package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
)

type searchFixture struct {
	t     *testing.T
	ctx   context.Context
	db    *sql.DB
	repo  *Repository
	clock time.Time
}

func (f *searchFixture) message(app, id, subject string, protected bool, sender string, recipients []string, age time.Duration) {
	f.t.Helper()
	tx, err := f.db.BeginTx(f.ctx, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	at := f.clock.Add(-age)
	if subject == "" {
		_, err = tx.ExecContext(f.ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,accepted_at) VALUES ($1,$2,$3,$4,$5)`, id, app, "submissions/"+id, protected, at)
	} else {
		_, err = tx.ExecContext(f.ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,accepted_at,subject,recipient_count,sender_domain,sender_domain_rev,sender_address,sender_name) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			id, app, "submissions/"+id, protected, at, subject, len(recipients), sender, reverseDomainLabels(sender), "Sender@"+sender, "Sender Name")
	}
	if err != nil {
		f.t.Fatal(err)
	}
	if subject != "" {
		var rows recipientRows
		if !rows.add(id, recipients, toKinds(len(recipients)), len(recipients), false) {
			f.t.Fatal("fixture recipients invalid")
		}
		if err = insertRecipients(f.ctx, tx, app, &rows); err != nil {
			f.t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
}

// toKinds returns n "to" kinds for fixtures that record only To recipients.
func toKinds(n int) []string {
	kinds := make([]string, n)
	for i := range kinds {
		kinds[i] = "to"
	}
	return kinds
}

func (f *searchFixture) search(app, field, q string, limit int) ([]string, error) {
	page, err := f.repo.SearchMessages(f.ctx, "operator", app, admin.MessageQuery{Text: q, Field: field, Limit: limit})
	return pageIDs(page), err
}

func pageIDs(page admin.MessagePage) []string {
	var ids []string
	for _, m := range page.Messages {
		ids = append(ids, m.ID)
	}
	return ids
}

// hexID spreads the leading digits so that short ID prefixes are distinctive.
func hexID(n int) string { return fmt.Sprintf("%08x%024x", uint32(n)*2654435761, n) }

func (f *searchFixture) expect(app, field, q string, want ...string) {
	f.t.Helper()
	got, err := f.search(app, field, q, 100)
	if err != nil {
		f.t.Fatal("search failed:", field, err)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		f.t.Fatalf("field %s: got %d results, want %d: %v vs %v", field, len(got), len(want), got, want)
	}
}

func (f *searchFixture) reject(app, field, q string) {
	f.t.Helper()
	if _, err := f.search(app, field, q, 10); !errors.Is(err, admin.ErrInvalidInput) {
		f.t.Fatal("unsearchable term accepted:", field, err)
	}
}

func TestMessageSearchAndMasking(t *testing.T) {
	ctx := context.Background()
	db := migratedSchemaForTest(t, ctx, "message_search_")
	const generation = "00000000000000000000000000000bbb"
	if _, err := db.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation); err != nil {
		t.Fatal(err)
	}
	appA, appB := hexID(0xa1), hexID(0xb1)
	if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'A'),($2,'B')`, appA, appB); err != nil {
		t.Fatal(err)
	}
	f := &searchFixture{t: t, ctx: ctx, db: db, repo: &Repository{DB: db, Generation: generation}, clock: time.Now().UTC().Truncate(time.Second)}
	m1, m2, m3, m4, m5, m6 := hexID(0x1001), hexID(0x1002), hexID(0x1003), hexID(0x1004), hexID(0x1005), hexID(0x1006)
	f.message(appA, m1, "INV-2024-001 for caf\u00e9", false, "mail.example.com", []string{"Alice@Example.COM", "bob@sub.example.net"}, 1*time.Minute)
	f.message(appA, m2, "Secret 2024 plan", true, "example.com", []string{"carol@example.com"}, 2*time.Minute)
	f.message(appA, m3, "ex_ample test", false, "badexample.com", []string{"dave@badexample.com", "under_score@foo.test", "100%@foo.test"}, 3*time.Minute)
	f.message(appA, m4, "", false, "", nil, 4*time.Minute) // accepted before recipients were recorded
	f.message(appB, m5, "INV-2024-001", false, "example.com", []string{"alice@example.com"}, 5*time.Minute)
	var bulk []string
	for i := 0; i < 10; i++ {
		bulk = append(bulk, fmt.Sprintf("user%d@bulk.example.net", i))
	}
	f.message(appA, m6, "bulk", false, "example.org", bulk, 6*time.Minute)
	d1, d2 := "deadbeef"+hexID(1)[8:], "cafebabe"+hexID(2)[8:] // delivery IDs
	for i, d := range []string{d1, d2} {
		if _, err := db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ($1,$2,$3,'final/'||$1,'queued')`, d, m1, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ($1,$2,0,'final/legacy','queued')`, hexID(3), m4); err != nil {
		t.Fatal(err)
	}

	t.Run("history masks Enhanced Protection subjects and lists recipients", func(t *testing.T) {
		page, err := f.repo.ListMessages(ctx, "operator", appA, "", 100)
		if err != nil || len(page.Messages) != 5 {
			t.Fatal("list failed", err)
		}
		raw, _ := json.Marshal(page)
		if strings.Contains(string(raw), "Secret") || strings.Contains(string(raw), "2024 plan") {
			t.Fatal("EP subject appeared in list JSON")
		}
		byID := map[string]admin.MessageSummary{}
		for _, m := range page.Messages {
			byID[m.ID] = m
		}
		ep := byID[m2]
		if ep.Subject != nil || !ep.SubjectMasked || !ep.EnhancedProtection || ep.SenderDomain == nil || *ep.SenderDomain != "example.com" || len(ep.Recipients) != 1 || ep.Sender == nil || ep.Sender.Address != "Sender@example.com" || ep.Sender.Name == nil || *ep.Sender.Name != "Sender Name" {
			t.Fatalf("EP summary wrong: %+v", ep)
		}
		open := byID[m1]
		if open.Subject == nil || *open.Subject != "INV-2024-001 for caf\u00e9" || open.SubjectMasked || open.EnhancedProtection || *open.SenderDomain != "mail.example.com" || *open.RecipientCount != 2 || strings.Join(open.Recipients, ",") != "Alice@Example.COM,bob@sub.example.net" {
			t.Fatalf("open summary wrong: %+v", open)
		}
		if len(byID[m6].Recipients) != 3 || *byID[m6].RecipientCount != 10 {
			t.Fatalf("list should show the first 3 recipients: %+v", byID[m6])
		}
		legacy := byID[m4]
		if legacy.Subject != nil || legacy.SubjectMasked || legacy.SenderDomain != nil || legacy.Sender != nil || legacy.RecipientCount != nil || legacy.Recipients != nil {
			t.Fatalf("unrecorded message invented metadata: %+v", legacy)
		}
		detail, err := f.repo.GetMessage(ctx, "operator", appA, m6)
		if err != nil || len(detail.Message.Recipients) != 10 || detail.Message.Recipients[9] != "user9@bulk.example.net" {
			t.Fatal("detail should list every recipient", err)
		}
		detail, err = f.repo.GetMessage(ctx, "operator", appA, m1)
		if err != nil || len(detail.Deliveries) != 2 || detail.Deliveries[0].Recipient == nil || *detail.Deliveries[0].Recipient != "Alice@Example.COM" || *detail.Deliveries[1].Recipient != "bob@sub.example.net" {
			t.Fatalf("delivery recipient join wrong: %+v %v", detail.Deliveries, err)
		}
		detail, err = f.repo.GetMessage(ctx, "operator", appA, m2)
		raw, _ = json.Marshal(detail)
		if err != nil || strings.Contains(string(raw), "Secret") || detail.Message.Subject != nil {
			t.Fatal("EP subject appeared in detail", err)
		}
		detail, err = f.repo.GetMessage(ctx, "operator", appA, m4)
		if err != nil || len(detail.Deliveries) != 1 || detail.Deliveries[0].Recipient != nil {
			t.Fatalf("unrecorded delivery gained a recipient: %+v %v", detail.Deliveries, err)
		}
		var audits int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events`).Scan(&audits); err != nil || audits != 0 {
			t.Fatal("history reads must not be audited", audits, err)
		}
	})

	t.Run("id search matches message and delivery ID prefixes", func(t *testing.T) {
		f.expect(appA, "id", m1[:8], m1)
		f.expect(appA, "id", strings.ToUpper(d1[:10]), m1)
		f.expect(appA, "id", d1[:10], m1)
		f.expect(appA, "id", d2, m1)
		f.expect(appB, "id", d1[:10])
		f.expect(appB, "id", m5[:12], m5)
		f.expect(appA, "id", m5[:12])
		f.reject(appA, "id", m1[:7])
		f.reject(appA, "id", "zzzzzzzz")
		f.reject(appA, "id", m1+"0")
		f.expect(appA, "any", d1[:12], m1)
	})

	t.Run("recipient search is a case-folded, escaped prefix", func(t *testing.T) {
		f.expect(appA, "recipient", "alice", m1)
		f.expect(appA, "recipient", "ALICE@EXAMPLE.COM", m1)
		f.expect(appA, "recipient", "alice@ex", m1)
		f.expect(appA, "recipient", "@example.com", m1, m2)
		f.expect(appA, "recipient", "@EXAMPLE.CO", m1, m2)
		f.expect(appA, "recipient", "example", m1, m2)
		f.expect(appA, "recipient", "bad", m3)
		f.expect(appA, "recipient", "carol", m2) // EP messages are searchable by recipient
		f.expect(appA, "recipient", "under_score", m3)
		f.expect(appA, "recipient", "_nder")
		f.expect(appA, "recipient", "under%")
		f.expect(appA, "recipient", "100%", m3)
		f.expect(appA, "recipient", "%%")
		f.expect(appA, "recipient", "ice@example.com")
		f.reject(appA, "recipient", "a")
		f.reject(appA, "recipient", "two words")
	})

	t.Run("domain search matches the domain and its subdomains", func(t *testing.T) {
		f.expect(appA, "recipient_domain", "example.com", m1, m2)
		f.expect(appA, "recipient_domain", "@Example.COM.", m1, m2)
		f.expect(appA, "recipient_domain", "badexample.com", m3)
		f.expect(appA, "recipient_domain", "ex_mple.com")
		f.expect(appA, "recipient_domain", "example.net", m1, m6)
		f.expect(appA, "recipient_domain", "net", m1, m6)
		f.expect(appA, "recipient_domain", "bulk.example.net", m6)
		f.expect(appA, "recipient_domain", "mple.com")
		f.expect(appA, "sender_domain", "example.com", m1, m2)
		f.expect(appA, "sender_domain", "MAIL.example.com", m1)
		f.expect(appA, "sender_domain", "badexample.com", m3)
		f.expect(appA, "sender_domain", "org", m6)
		f.expect(appA, "sender_domain", "ex_mple.com")
		f.expect(appB, "sender_domain", "example.com", m5)
		f.reject(appA, "recipient_domain", "a..b")
		f.reject(appA, "recipient_domain", "has space.example")
		f.reject(appA, "sender_domain", "user@example.com")
	})

	t.Run("subject search matches word prefixes and never EP subjects", func(t *testing.T) {
		f.expect(appA, "subject", "2024", m1)
		f.expect(appA, "subject", "caf", m1)
		f.expect(appA, "subject", "CAF\u00c9", m1)
		f.expect(appA, "subject", "inv 2024", m1)
		f.expect(appA, "subject", "INV-2024-001", m1)
		f.expect(appA, "subject", "secret")
		f.expect(appA, "subject", "plan")
		f.expect(appA, "subject", "mple")
		f.expect(appA, "subject", "ex ample", m3)
		f.expect(appA, "subject", "bulk", m6)
		f.expect(appB, "subject", "2024", m5)
		f.reject(appA, "subject", "---")
		f.expect(appA, "subject", `it's a \ test`)
		f.expect(appA, "subject", "a:* | b & !c")
	})

	t.Run("any combines the qualifying branches", func(t *testing.T) {
		f.expect(appA, "any", "example.com", m1, m2)
		f.expect(appA, "any", "example.net", m1, m6) // dotted: the domain branches match subdomains
		f.expect(appA, "any", "net")                 // not dotted: no domain branch, no prefix match
		f.expect(appA, "any", "2024", m1)
		f.expect(appA, "any", "carol", m2)
		f.expect(appA, "any", "bulk", m6)
		f.expect(appA, "any", "alice@example.com", m1)
		f.expect(appA, "any", "nothing matches this")
		f.reject(appA, "any", "!")
	})

	t.Run("cross-app isolation and unknown app", func(t *testing.T) {
		f.expect(appB, "recipient", "alice", m5)
		f.expect(appB, "recipient", "carol")
		f.expect(appA, "any", "alice@example.com", m1)
		if _, err := f.search(hexID(0xdead), "any", "alice", 10); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("unknown app searched", err)
		}
	})

	t.Run("pages across the union without duplicates", func(t *testing.T) {
		for i := 0; i < 7; i++ {
			f.message(appA, hexID(0x2000+i), fmt.Sprintf("paged %d", i), i%2 == 0, "page.example.org", []string{fmt.Sprintf("a%d@page.example.net", i), fmt.Sprintf("b%d@page.example.net", i), fmt.Sprintf("c%d@page.example.net", i)}, time.Duration(i)*time.Second)
		}
		var seen []string
		cursor := ""
		for pages := 0; pages < 10; pages++ {
			page, err := f.repo.SearchMessages(ctx, "operator", appA, admin.MessageQuery{Text: "page.example.net", Field: "any", Cursor: cursor, Limit: 3})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Messages) > 3 {
				t.Fatal("page larger than limit")
			}
			seen = append(seen, pageIDs(page)...)
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		if len(seen) != 7 {
			t.Fatal("paging lost or duplicated rows", seen)
		}
		for i, id := range seen {
			if id != hexID(0x2000+i) {
				t.Fatal("paging order wrong", seen)
			}
		}
		// A cursor from one filter is simply "older than this row" for another.
		page, err := f.repo.SearchMessages(ctx, "operator", appA, admin.MessageQuery{Text: "paged", Field: "subject", Cursor: cursor, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		_ = page
		// Messages matching through many recipient rows still count once.
		page, err = f.repo.SearchMessages(ctx, "operator", appA, admin.MessageQuery{Text: "bulk.example.net", Field: "recipient_domain", Limit: 1})
		if err != nil || len(page.Messages) != 1 || page.NextCursor != "" {
			t.Fatal("many matching recipients displaced the page", err)
		}
	})

	t.Run("subject reveal audits only Enhanced Protection", func(t *testing.T) {
		subject, err := f.repo.RevealSubject(ctx, "operator", appA, m1)
		if err != nil || subject != "INV-2024-001 for caf\u00e9" {
			t.Fatal("open subject not revealed", err)
		}
		var audits int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events`).Scan(&audits); err != nil || audits != 0 {
			t.Fatal("non-EP reveal audited", audits, err)
		}
		subject, err = f.repo.RevealSubject(ctx, "operator", appA, m2)
		if err != nil || subject != "Secret 2024 plan" {
			t.Fatal("EP subject not revealed", err)
		}
		var action, resource, actor string
		if err = db.QueryRowContext(ctx, `SELECT action,resource_id,actor_id FROM audit_events`).Scan(&action, &resource, &actor); err != nil || action != "message.subject.reveal" || resource != m2 || actor != "operator" {
			t.Fatal("EP reveal audit wrong", action, resource, err)
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE (actor_id||app_id||action||resource_id||details::text) ILIKE '%secret%' OR (actor_id||app_id||action||resource_id||details::text) LIKE '%2024%'`).Scan(&audits); err != nil || audits != 0 {
			t.Fatal("subject text leaked into audit rows", audits, err)
		}
		if _, err = f.repo.RevealSubject(ctx, "operator", appA, m4); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("unrecorded subject not a conflict", err)
		}
		if _, err = f.repo.RevealSubject(ctx, "operator", appB, m1); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("cross-app reveal", err)
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events`).Scan(&audits); err != nil || audits != 1 {
			t.Fatal("failed reveals were audited", audits, err)
		}
		if _, err = db.ExecContext(ctx, `CREATE FUNCTION fail_reveal_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$;
			CREATE TRIGGER fail_reveal_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_reveal_audit()`); err != nil {
			t.Fatal(err)
		}
		subject, err = f.repo.RevealSubject(ctx, "operator", appA, m2)
		if err == nil || subject != "" {
			t.Fatal("EP subject returned without an audit row")
		}
		if _, err = db.ExecContext(ctx, `DROP TRIGGER fail_reveal_audit ON audit_events; DROP FUNCTION fail_reveal_audit()`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("content reads audit only Enhanced Protection", func(t *testing.T) {
		audits := func(action string) (n int, resource string) {
			if err := db.QueryRowContext(ctx, `SELECT count(*),COALESCE(min(resource_id),'') FROM audit_events WHERE action=$1`, action).Scan(&n, &resource); err != nil {
				t.Fatal(err)
			}
			return
		}
		// m2 is Enhanced Protection; m1 is not.
		if _, err := db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ($1,$2,0,'final/ep','queued')`, hexID(0x3001), m2); err != nil {
			t.Fatal(err)
		}
		key, protected, err := f.repo.AuthorizeBodyRead(ctx, "operator", appA, m1)
		if err != nil || key != "submissions/"+m1 || protected {
			t.Fatal("non-EP body authorization", err)
		}
		if _, _, err = f.repo.AuthorizeAttachmentRead(ctx, "operator", appA, m1, 0); err != nil {
			t.Fatal(err)
		}
		if key, protected, err = f.repo.AuthorizeDeliveryHeadersRead(ctx, "operator", appA, m1, d1); err != nil || key != "final/"+d1 || protected {
			t.Fatal("non-EP headers authorization", err)
		}
		for _, action := range []string{"message.body.read", "message.attachment.read", "message.delivery.headers.read"} {
			if n, _ := audits(action); n != 0 {
				t.Fatal("non-EP content read audited", action)
			}
		}
		if key, protected, err = f.repo.AuthorizeBodyRead(ctx, "operator", appA, m2); err != nil || key != "submissions/"+m2 || !protected {
			t.Fatal("EP body authorization", err)
		}
		if _, protected, err = f.repo.AuthorizeAttachmentRead(ctx, "operator", appA, m2, 7); err != nil || !protected {
			t.Fatal("EP attachment authorization", err)
		}
		if key, protected, err = f.repo.AuthorizeDeliveryHeadersRead(ctx, "operator", appA, m2, hexID(0x3001)); err != nil || key != "final/ep" || !protected {
			t.Fatal("EP headers authorization", err)
		}
		for action, resource := range map[string]string{
			"message.body.read":             m2,
			"message.attachment.read":       m2 + ":7",
			"message.delivery.headers.read": m2 + ":" + hexID(0x3001),
		} {
			if n, got := audits(action); n != 1 || got != resource {
				t.Fatal("EP audit missing or malformed", action, n, got)
			}
		}
		// App scoping: another app's ID, another message's delivery, and unknown IDs.
		for _, call := range []func() error{
			func() error { _, _, e := f.repo.AuthorizeBodyRead(ctx, "operator", appB, m2); return e },
			func() error { _, _, e := f.repo.AuthorizeAttachmentRead(ctx, "operator", appB, m2, 0); return e },
			func() error {
				_, _, e := f.repo.AuthorizeDeliveryHeadersRead(ctx, "operator", appB, m2, hexID(0x3001))
				return e
			},
			func() error {
				_, _, e := f.repo.AuthorizeDeliveryHeadersRead(ctx, "operator", appA, m1, hexID(0x3001))
				return e
			},
			func() error { _, _, e := f.repo.AuthorizeDeliveryHeadersRead(ctx, "operator", appA, m3, d1); return e },
			func() error { _, _, e := f.repo.AuthorizeAttachmentRead(ctx, "operator", appA, m2, 20); return e },
		} {
			if err := call(); !errors.Is(err, admin.ErrNotFound) {
				t.Fatal("scoping failure not 404", err)
			}
		}
		for _, action := range []string{"message.body.read", "message.attachment.read", "message.delivery.headers.read"} {
			if n, _ := audits(action); n != 1 {
				t.Fatal("rejected reads were audited", action, n)
			}
		}
		// The audit must commit before the key is returned: a failing audit insert yields no key.
		if _, err = db.ExecContext(ctx, `CREATE FUNCTION fail_content_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$;
			CREATE TRIGGER fail_content_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_content_audit()`); err != nil {
			t.Fatal(err)
		}
		if key, _, err = f.repo.AuthorizeBodyRead(ctx, "operator", appA, m2); err == nil || key != "" {
			t.Fatal("EP object reference returned without an audit row")
		}
		if _, _, err = f.repo.AuthorizeBodyRead(ctx, "operator", appA, m1); err != nil {
			t.Fatal("non-EP read depends on the audit table", err)
		}
		if _, err = db.ExecContext(ctx, `DROP TRIGGER fail_content_audit ON audit_events; DROP FUNCTION fail_content_audit()`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("invalid requests are rejected before the database", func(t *testing.T) {
		for _, q := range []admin.MessageQuery{
			{Text: "", Field: "any", Limit: 10},
			{Text: " padded", Field: "any", Limit: 10},
			{Text: "bad\x00", Field: "any", Limit: 10},
			{Text: strings.Repeat("a", 255), Field: "any", Limit: 10},
			{Text: "abc", Field: "body", Limit: 10},
			{Text: "abc", Field: "any", Limit: 0},
			{Text: "abc", Field: "any", Limit: 101},
			{Text: "abc", Field: "any", Limit: 10, Cursor: "not-a-cursor"},
		} {
			if _, err := f.repo.SearchMessages(ctx, "operator", appA, q); !errors.Is(err, admin.ErrInvalidInput) {
				t.Fatal("invalid query accepted", err)
			}
		}
	})

	t.Run("indexes serve the search predicates", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		for _, setting := range []string{`SET LOCAL enable_seqscan=off`, `SET LOCAL enable_bitmapscan=off`, `SET LOCAL plan_cache_mode=force_custom_plan`} {
			if _, err = tx.ExecContext(ctx, setting); err != nil {
				t.Fatal(err)
			}
		}
		for index, query := range map[string]string{
			"message_recipients_address_idx":    `SELECT 1 FROM message_recipients WHERE app_id='x' AND address_lower LIKE 'ali%' ESCAPE '\'`,
			"message_recipients_domain_idx":     `SELECT 1 FROM message_recipients WHERE app_id='x' AND domain_lower LIKE 'exa%' ESCAPE '\'`,
			"message_recipients_domain_rev_idx": `SELECT 1 FROM message_recipients WHERE app_id='x' AND domain_rev LIKE 'com.example.%' ESCAPE '\'`,
			"messages_sender_domain_idx":        `SELECT 1 FROM messages WHERE app_id='x' AND sender_domain_rev IS NOT NULL AND sender_domain_rev LIKE 'com.example.%' ESCAPE '\'`,
			"deliveries_id_prefix_idx":          `SELECT 1 FROM deliveries WHERE id LIKE 'abcdef01%'`,
		} {
			assertIndexUsed(t, ctx, tx, index, query)
		}
		bitmap := `EXPLAIN SELECT 1 FROM messages WHERE subject IS NOT NULL AND NOT enhanced_protection AND to_tsvector('simple'::regconfig, regexp_replace(subject, '[^[:alnum:]]+', ' ', 'g')) @@ to_tsquery('simple'::regconfig,'inv:*')`
		if _, err = tx.ExecContext(ctx, `SET LOCAL enable_bitmapscan=on`); err != nil {
			t.Fatal(err)
		}
		assertIndexUsed(t, ctx, tx, "messages_subject_search_idx", bitmap[len("EXPLAIN "):])
	})
}

func assertIndexUsed(t *testing.T, ctx context.Context, tx *sql.Tx, index, query string) {
	t.Helper()
	rows, err := tx.QueryContext(ctx, "EXPLAIN "+query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if !strings.Contains(plan.String(), index) {
		t.Fatalf("plan does not use %s:\n%s", index, plan.String())
	}
}

func TestMessageStatusDerivedFromDeliveries(t *testing.T) {
	ctx := context.Background()
	db := migratedSchemaForTest(t, ctx, "message_status_")
	const generation = "00000000000000000000000000000ccc"
	if _, err := db.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation); err != nil {
		t.Fatal(err)
	}
	app := hexID(0xc1)
	if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'C')`, app); err != nil {
		t.Fatal(err)
	}
	f := &searchFixture{t: t, ctx: ctx, db: db, repo: &Repository{DB: db, Generation: generation}, clock: time.Now().UTC().Truncate(time.Second)}
	cases := []struct {
		name   string
		msg    string
		states []string
		retry  int  // number of leading deliveries given a completed attempt
		denied bool // those attempts were recipient policy denials, not SMTP attempts
		want   string
	}{
		{"delivered", hexID(0x2001), []string{"accepted", "accepted"}, 0, false, "delivered"},
		{"partial", hexID(0x2002), []string{"accepted", "failed"}, 0, false, "partially_delivered"},
		{"sending", hexID(0x2003), []string{"accepted", "queued"}, 0, false, "sending"},
		{"retrying", hexID(0x2004), []string{"in_flight", "accepted"}, 1, false, "retrying"},
		{"attention", hexID(0x2005), []string{"accepted", "tracking_held"}, 0, false, "attention"},
		{"uncertain", hexID(0x2006), []string{"accepted", "uncertain"}, 0, false, "attention"},
		{"failed", hexID(0x2007), []string{"failed"}, 0, false, "failed"},
		{"recovery held", hexID(0x2008), []string{"accepted", "recovery_hold"}, 0, false, "held"},
		{"recovery held only", hexID(0x2009), []string{"recovery_hold"}, 0, false, "held"},
		{"released policy hold", hexID(0x200a), []string{"queued"}, 1, true, "sending"},
	}
	n := 0x20000
	for i, c := range cases {
		f.message(app, c.msg, "s", false, "example.com", []string{"a@example.com"}, time.Duration(i+1)*time.Minute)
		if _, err := db.ExecContext(ctx, `UPDATE messages SET state='prepared' WHERE id=$1`, c.msg); err != nil {
			t.Fatal(err)
		}
		for j, st := range c.states {
			n++
			d := hexID(n)
			if _, err := db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ($1,$2,$3,'final/'||$1,$4)`, d, c.msg, j, st); err != nil {
				t.Fatal(err)
			}
			if j < c.retry {
				n++
				outcome := "rejected"
				if c.denied {
					outcome = "recipient_policy_denied"
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,state,outcome) VALUES ($1,$2,'w',1,'completed',$3)`, hexID(n), d, outcome); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	page, err := f.repo.ListMessages(ctx, "operator", app, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range page.Messages {
		got[m.ID] = m.Status
	}
	for _, c := range cases {
		if got[c.msg] != c.want {
			t.Errorf("%s: list status %q, want %q", c.name, got[c.msg], c.want)
		}
		detail, err := f.repo.GetMessage(ctx, "operator", app, c.msg)
		if err != nil || detail.Message.Status != c.want {
			t.Errorf("%s: detail status %q (%v), want %q", c.name, detail.Message.Status, err, c.want)
		}
	}
}
