package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/smtpcred"
)

const smtpTestGeneration = "00000000000000000000000000000dde"

// smtpOwnerFixture is a migrated schema with an open gate, used through the
// schema owner: it tests behavior, and TestSMTPRuntimeGrants tests privileges.
func smtpOwnerFixture(t *testing.T) (context.Context, *Repository, func(string, ...any), func(string, ...any) int) {
	t.Helper()
	ctx := context.Background()
	db := migratedSchemaForTest(t, ctx, "smtp_store_")
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(query, err)
		}
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(query, err)
		}
		return n
	}
	exec(`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, smtpTestGeneration)
	return ctx, &Repository{DB: db, Generation: smtpTestGeneration}, exec, count
}

func TestSMTPCredentialsPostgres(t *testing.T) {
	ctx, repo, exec, count := smtpOwnerFixture(t)
	app, other := strings.Repeat("a", 32), strings.Repeat("b", 32)
	exec(`INSERT INTO apps(id,name) VALUES ($1,'A'),($2,'B')`, app, other)
	exec(`INSERT INTO apps(id,name,active) VALUES ($1,'Off',false)`, strings.Repeat("c", 32))
	audits := func(action string) int {
		return count(`SELECT count(*) FROM audit_events WHERE action=$1 AND app_id=$2`, action, app)
	}

	if _, err := repo.IssueSMTPCredential(ctx, "operator", strings.Repeat("9", 32), "x"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("unknown app", err)
	}
	if _, err := repo.IssueSMTPCredential(ctx, "operator", strings.Repeat("c", 32), "x"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("inactive app", err)
	}
	if _, err := repo.IssueSMTPCredential(ctx, "operator", app, ""); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("empty name", err)
	}
	if _, err := repo.ListSMTPCredentials(ctx, strings.Repeat("9", 32)); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("list unknown app", err)
	}
	if list, err := repo.ListSMTPCredentials(ctx, app); err != nil || list == nil || len(list) != 0 {
		t.Fatal("empty list", list, err)
	}

	issued, err := repo.IssueSMTPCredential(ctx, "operator", app, "Front desk printer")
	if err != nil || !strings.HasPrefix(issued.Password, smtpcred.SecretPrefix) || issued.Username != issued.Credential.ID || issued.Credential.Username != issued.Credential.ID ||
		!issued.Credential.Active || issued.Credential.Name != "Front desk printer" || audits("smtp_credential.created") != 1 {
		t.Fatal("issue", issued, err)
	}
	var stored []byte
	if err = repo.DB.QueryRowContext(ctx, `SELECT digest FROM smtp_credentials WHERE id=$1`, issued.Username).Scan(&stored); err != nil || len(stored) != 32 || strings.Contains(string(stored), issued.Password) {
		t.Fatal("digest", err)
	}
	if count(`SELECT count(*) FROM audit_events WHERE details::text LIKE '%'||$1||'%' OR resource_id=$1`, issued.Password) != 0 {
		t.Fatal("password reached the audit log")
	}

	// A credential never leaks across apps.
	if err = repo.RevokeSMTPCredential(ctx, "operator", other, issued.Username); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("revoke through another app", err)
	}
	if list, err := repo.ListSMTPCredentials(ctx, other); err != nil || len(list) != 0 {
		t.Fatal("other app list", list, err)
	}

	// The tenth active credential is the last; revoking frees a slot.
	for i := 1; i < smtpcred.MaxActivePerApp; i++ {
		if _, err = repo.IssueSMTPCredential(ctx, "operator", app, fmt.Sprintf("device %d", i)); err != nil {
			t.Fatal("issue", i, err)
		}
	}
	if _, err = repo.IssueSMTPCredential(ctx, "operator", app, "one too many"); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("eleventh credential", err)
	}
	if err = repo.RevokeSMTPCredential(ctx, "operator", app, issued.Username); err != nil || audits("smtp_credential.revoked") != 1 {
		t.Fatal("revoke", err)
	}
	if err = repo.RevokeSMTPCredential(ctx, "operator", app, issued.Username); !errors.Is(err, admin.ErrNotFound) || audits("smtp_credential.revoked") != 1 {
		t.Fatal("second revoke", err)
	}
	if err = repo.RevokeSMTPCredential(ctx, "operator", app, "bad"); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("malformed id", err)
	}
	if _, err = repo.IssueSMTPCredential(ctx, "operator", app, "replacement"); err != nil {
		t.Fatal("slot not freed", err)
	}
	list, err := repo.ListSMTPCredentials(ctx, app)
	if err != nil || len(list) != 11 || list[0].ID != issued.Username || list[0].Active || list[0].RevokedAt == nil || !list[10].Active {
		t.Fatal("list", len(list), err)
	}

	// Concurrent creates cannot pass the limit.
	racer := strings.Repeat("d", 32)
	exec(`INSERT INTO apps(id,name) VALUES ($1,'Racer')`, racer)
	var wg sync.WaitGroup
	var mu sync.Mutex
	created, conflicts := 0, 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repo.IssueSMTPCredential(ctx, "operator", racer, "r")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, admin.ErrConflict):
				conflicts++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if created != smtpcred.MaxActivePerApp || conflicts != 6 {
		t.Fatal("concurrent creates", created, conflicts)
	}

	// A closed gate refuses mutations.
	exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
	if _, err = repo.IssueSMTPCredential(ctx, "operator", other, "x"); err == nil {
		t.Fatal("issued through a closed gate")
	}
	exec(`UPDATE runtime_gate SET mode='open' WHERE singleton`)
}

func TestSMTPSettingsPostgres(t *testing.T) {
	ctx, repo, exec, count := smtpOwnerFixture(t)
	app := strings.Repeat("a", 32)
	exec(`INSERT INTO apps(id,name) VALUES ($1,'A')`, app)
	exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now()),($1,'pending.example',false,NULL)`, app)
	audits := func() int {
		return count(`SELECT count(*) FROM audit_events WHERE action='smtp_settings.updated' AND app_id=$1`, app)
	}
	str := func(s string) *string { return &s }

	if got, err := repo.GetSMTPSettings(ctx, app); err != nil || got != (admin.SMTPFromPolicy{Mode: "off", OnlyUnauthorized: true}) {
		t.Fatal("default", got, err)
	}
	if _, err := repo.GetSMTPSettings(ctx, strings.Repeat("9", 32)); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("unknown app", err)
	}
	if _, err := repo.GetSMTPSettings(ctx, "bad"); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("malformed app", err)
	}
	// An unchanged policy is not audited.
	if _, err := repo.SetSMTPSettings(ctx, "operator", app, admin.SMTPFromPolicy{Mode: "off", OnlyUnauthorized: true}); err != nil || audits() != 0 {
		t.Fatal("unchanged policy audited", err)
	}
	domain := admin.SMTPFromPolicy{Mode: "domain", Domain: str("example.com"), KeepOrigin: true, OnlyUnauthorized: false}
	got, err := repo.SetSMTPSettings(ctx, "operator", app, domain)
	if err != nil || got.Mode != "domain" || *got.Domain != "example.com" || !got.KeepOrigin || audits() != 1 {
		t.Fatal("domain mode", got, err)
	}
	if again, err := repo.SetSMTPSettings(ctx, "operator", app, domain); err != nil || !samePolicy(again, got) || audits() != 1 {
		t.Fatal("PUT is not idempotent", again, err, audits())
	}
	if stored, err := repo.GetSMTPSettings(ctx, app); err != nil || !samePolicy(stored, got) {
		t.Fatal("stored", stored, err)
	}
	address := admin.SMTPFromPolicy{Mode: "address", Address: str("no-reply@example.com"), OnlyUnauthorized: true}
	if got, err = repo.SetSMTPSettings(ctx, "operator", app, address); err != nil || got.Domain != nil || got.KeepOrigin || audits() != 2 {
		t.Fatal("address mode", got, err)
	}
	// Domain mode keeps an optional fallback address, stored and idempotent.
	fallback := admin.SMTPFromPolicy{Mode: "domain", Domain: str("example.com"), Address: str("no-reply@example.com"), KeepOrigin: true, OnlyUnauthorized: true}
	if got, err = repo.SetSMTPSettings(ctx, "operator", app, fallback); err != nil || got.Address == nil || *got.Address != "no-reply@example.com" || *got.Domain != "example.com" || audits() != 3 {
		t.Fatal("domain mode with fallback", got, err)
	}
	if stored, err := repo.GetSMTPSettings(ctx, app); err != nil || !samePolicy(stored, fallback) {
		t.Fatal("stored fallback", stored, err)
	}
	if again, err := repo.SetSMTPSettings(ctx, "operator", app, fallback); err != nil || !samePolicy(again, fallback) || audits() != 3 {
		t.Fatal("fallback PUT is not idempotent", again, err, audits())
	}
	// 409: a domain that is not an active verified sender domain of the app.
	for _, p := range []admin.SMTPFromPolicy{
		{Mode: "domain", Domain: str("pending.example"), OnlyUnauthorized: true},
		{Mode: "domain", Domain: str("elsewhere.example"), OnlyUnauthorized: true},
		{Mode: "address", Address: str("a@pending.example"), OnlyUnauthorized: true},
		{Mode: "address", Address: str("a@elsewhere.example"), OnlyUnauthorized: true},
		{Mode: "domain", Domain: str("example.com"), Address: str("a@pending.example"), OnlyUnauthorized: true},
		{Mode: "domain", Domain: str("example.com"), Address: str("a@elsewhere.example"), OnlyUnauthorized: true},
	} {
		if _, err = repo.SetSMTPSettings(ctx, "operator", app, p); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("unverified domain accepted", p, err)
		}
	}
	// 400: bad shapes.
	for _, p := range []admin.SMTPFromPolicy{
		{Mode: "rewrite"},
		{Mode: "domain"},
		{Mode: "domain", Domain: str("Example.com")},
		{Mode: "domain", Domain: str("example.com"), Address: str(`"a b"@example.com`)},
		{Mode: "domain", Domain: str("example.com"), Address: str("a..b@example.com")},
		{Mode: "address"},
		{Mode: "address", Address: str(`"a b"@example.com`)},
		{Mode: "address", Address: str("a..b@example.com")},
		{Mode: "address", Address: str("a@example.com"), KeepOrigin: true},
		{Mode: "off", Domain: str("example.com")},
		{Mode: "off", KeepOrigin: true},
	} {
		if _, err = repo.SetSMTPSettings(ctx, "operator", app, p); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatal("bad shape accepted", p, err)
		}
	}
	if got, err = repo.SetSMTPSettings(ctx, "operator", app, admin.SMTPFromPolicy{Mode: "off", OnlyUnauthorized: true}); err != nil || got.Domain != nil || got.Address != nil || audits() != 4 {
		t.Fatal("back to off", got, err)
	}
	if _, err = repo.SetSMTPSettings(ctx, "operator", strings.Repeat("9", 32), address); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("unknown app", err)
	}
	if audits() != 4 {
		t.Fatal("a refused change was audited")
	}
}

func TestSMTPConnectionPostgres(t *testing.T) {
	ctx, repo, exec, _ := smtpOwnerFixture(t)
	assigned, unassigned := strings.Repeat("a", 32), strings.Repeat("b", 32)
	ipOne, ipTwo := strings.Repeat("1", 32), strings.Repeat("2", 32)
	exec(`INSERT INTO apps(id,name) VALUES ($1,'Pinned'),($2,'Default')`, assigned, unassigned)
	primary := netip.MustParseAddr("45.0.1.22")

	connect := func(app string, p netip.Addr) admin.SMTPConnection {
		t.Helper()
		c, err := repo.SMTPConnection(ctx, app, p)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Ports) != 2 || c.Ports[0] != (admin.SMTPPort{Port: 465, Security: "tls"}) || c.Ports[1] != (admin.SMTPPort{Port: 587, Security: "starttls"}) || c.ObservedPorts == nil {
			t.Fatal("ports", c)
		}
		return c
	}
	host := func(c admin.SMTPConnection) string {
		if c.Host == nil {
			return ""
		}
		return *c.Host
	}
	source := func(c admin.SMTPConnection) string {
		if c.HostSource == nil {
			return ""
		}
		return *c.HostSource
	}
	if _, err := repo.SMTPConnection(ctx, strings.Repeat("9", 32), primary); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("unknown app", err)
	}

	// No row for the primary: no host and the prompt flag.
	if c := connect(unassigned, primary); c.Host != nil || c.HostSource != nil || !c.PrimaryUnnamed {
		t.Fatal("unnamed primary", c)
	}
	// An unset primary (no bridge) is neither a host nor a prompt.
	if c := connect(unassigned, netip.Addr{}); c.Host != nil || c.PrimaryUnnamed {
		t.Fatal("unset primary", c)
	}

	exec(`INSERT INTO sending_ips(id,name,address,helo_name,bounce_domain) VALUES
		($1,'main','45.0.1.22','smtp-main.example.org','mail.example.org'),
		($2,'extra','45.0.0.10','smtp-extra.example.org','mail.example.org')`, ipOne, ipTwo)
	if c := connect(unassigned, primary); host(c) != "smtp-main.example.org" || source(c) != "primary" || c.PrimaryUnnamed {
		t.Fatal("primary row", c)
	}
	exec(`UPDATE apps SET sending_ip_id=$2 WHERE id=$1`, assigned, ipTwo)
	if c := connect(assigned, primary); host(c) != "smtp-extra.example.org" || source(c) != "app_sending_ip" || c.PrimaryUnnamed {
		t.Fatal("assigned row", c)
	}
	// An assigned app does not need the primary row at all.
	if c := connect(assigned, netip.Addr{}); host(c) != "smtp-extra.example.org" {
		t.Fatal("assigned row without primary", c)
	}

	// An edge report whose first address differs from the configured primary
	// does not change the host: the Edge never steers clients.
	exec(`INSERT INTO sending_ips(id,name,address,helo_name,bounce_domain) VALUES ($1,'third','45.0.0.11','smtp-third.example.org','mail.example.org')`, strings.Repeat("3", 32))
	if _, err := repo.DB.ExecContext(ctx, `INSERT INTO edge_source_reports(worker_id,epoch,addresses,bounce_domains,reported_at,submission_ports)
		VALUES ('w1',1,'{45.0.0.11,45.0.1.22}'::inet[],'{mail.example.org}',clock_timestamp(),'{587,465}')`); err != nil {
		t.Fatal(err)
	}
	c := connect(unassigned, primary)
	if host(c) != "smtp-main.example.org" || len(c.ObservedPorts) != 2 || c.ObservedPorts[0] != 587 || c.ObservedAt == nil {
		t.Fatal("edge report steered the host", c)
	}
	// A report that lists no ports means unknown, not none.
	if _, err := repo.DB.ExecContext(ctx, `UPDATE edge_source_reports SET submission_ports=NULL,reported_at=reported_at+interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if c = connect(unassigned, primary); len(c.ObservedPorts) != 0 || c.ObservedAt != nil {
		t.Fatal("unreported ports", c)
	}
}

func TestRecordEdgeSourcesWritesSubmissionPorts(t *testing.T) {
	ctx, repo, exec, _ := smtpOwnerFixture(t)
	worker, controller := strings.Repeat("e", 32), strings.Repeat("f", 32)
	exec(`INSERT INTO controller_leases(worker_id,controller_id,epoch,expires_at) VALUES ($1,$2,1,clock_timestamp()+interval '1 hour')`, worker, controller)
	lease := ControllerLease{WorkerID: worker, ControllerID: controller, Epoch: 1}
	caps := delivery.EdgeCapabilities{Explicit: true, Addresses: []netip.Addr{netip.MustParseAddr("45.0.1.22")}, BounceDomains: []string{"mail.example.org"}, SubmissionPorts: []int{465, 587}}
	if err := repo.RecordEdgeSources(ctx, lease, caps); err != nil {
		t.Fatal(err)
	}
	var ports string
	if err := repo.DB.QueryRowContext(ctx, `SELECT submission_ports::text FROM edge_source_reports WHERE worker_id=$1`, worker).Scan(&ports); err != nil || ports != "{465,587}" {
		t.Fatal(ports, err)
	}
	// An untrusted report with invalid, duplicate and too many ports is cleaned,
	// never refused, so the rest of the source report still lands.
	caps.SubmissionPorts = []int{0, -1, 70000, 587, 587, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	if err := repo.RecordEdgeSources(ctx, lease, caps); err != nil {
		t.Fatal(err)
	}
	if err := repo.DB.QueryRowContext(ctx, `SELECT submission_ports::text FROM edge_source_reports WHERE worker_id=$1`, worker).Scan(&ports); err != nil || ports != "{587,1,2,3,4,5,6,7}" {
		t.Fatal(ports, err)
	}
	caps.SubmissionPorts = nil
	if err := repo.RecordEdgeSources(ctx, lease, caps); err != nil {
		t.Fatal(err)
	}
	var cleared bool
	if err := repo.DB.QueryRowContext(ctx, `SELECT submission_ports IS NULL FROM edge_source_reports WHERE worker_id=$1`, worker).Scan(&cleared); err != nil || !cleared {
		t.Fatal("ports not cleared", err)
	}
}

// The runtime role may create credentials and revoke them (and record use) but
// may not edit a digest, delete a row, or rewrite the replay secret.
func TestSMTPRuntimeGrants(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	schema := "smtp_grants_" + strings.Repeat("0", 4) + fmt.Sprint(time.Now().UnixNano())
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
	owner, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err = Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	db, cleanup := restrictedRuntimeForTest(t, ctx, owner, u.String())
	defer cleanup()
	app, cred := strings.Repeat("a", 32), strings.Repeat("c", 32)
	for _, statement := range []string{
		`INSERT INTO apps(id,name) VALUES ('` + app + `','A')`,
		`INSERT INTO smtp_credentials(id,app_id,name,digest) VALUES ('` + cred + `','` + app + `','n',` + smtpTestDigest + `)`,
		`UPDATE smtp_credentials SET revoked_at=clock_timestamp(),last_used_at=clock_timestamp() WHERE id='` + cred + `'`,
		`INSERT INTO smtp_replay_secret(sealed) VALUES (decode(repeat('ab',40),'hex'))`,
		`SELECT sealed FROM smtp_replay_secret`,
		`UPDATE apps SET smtp_from_mode='off' WHERE id='` + app + `'`,
		`UPDATE edge_source_reports SET submission_ports=NULL WHERE false`,
	} {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for _, statement := range []string{
		`UPDATE smtp_credentials SET digest=` + smtpTestDigest + ` WHERE id='` + cred + `'`,
		`UPDATE smtp_credentials SET app_id=app_id`,
		`DELETE FROM smtp_credentials`,
		`UPDATE smtp_replay_secret SET sealed=sealed`,
		`DELETE FROM smtp_replay_secret`,
		`TRUNCATE smtp_credentials`,
	} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("runtime role may run %s", statement)
		}
	}
}

// Message summaries read the new columns: a message submitted over SMTP names
// its credential and the From the client originally sent. Nothing writes these
// columns yet, so the rows are inserted directly.
func TestSMTPMessageSummaryPostgres(t *testing.T) {
	ctx, repo, exec, _ := smtpOwnerFixture(t)
	app, cred := strings.Repeat("a", 32), strings.Repeat("c", 32)
	exec(`INSERT INTO apps(id,name) VALUES ($1,'A')`, app)
	exec(`INSERT INTO smtp_credentials(id,app_id,name,digest) VALUES ($1,$2,'Printer',decode(repeat('ab',32),'hex'))`, cred, app)
	exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev,smtp_credential_id,original_from_address,accepted_at)
		VALUES ($1,$2,'k1',false,'',1,'example.com','com.example.',$3,'device@printer.example.net',now()),
		       ($4,$2,'k2',false,'hello',1,'example.com','com.example.',NULL,NULL,now()-interval '1 minute')`,
		strings.Repeat("1", 32), app, cred, strings.Repeat("2", 32))
	page, err := repo.ListMessages(ctx, "operator", app, "", 10)
	if err != nil || len(page.Messages) != 2 {
		t.Fatal(page, err)
	}
	smtpMessage, apiMessage := page.Messages[0], page.Messages[1]
	if smtpMessage.SMTP == nil || smtpMessage.SMTP.CredentialID != cred || smtpMessage.SMTP.CredentialName != "Printer" ||
		smtpMessage.SMTP.OriginalFrom == nil || *smtpMessage.SMTP.OriginalFrom != "device@printer.example.net" {
		t.Fatal("smtp message", smtpMessage)
	}
	if apiMessage.SMTP != nil {
		t.Fatal("api message", apiMessage)
	}
	detail, err := repo.GetMessage(ctx, "operator", app, strings.Repeat("1", 32))
	if err != nil || detail.Message.SMTP == nil || detail.Message.SMTP.CredentialName != "Printer" {
		t.Fatal("detail", detail, err)
	}
	found, err := repo.SearchMessages(ctx, "operator", app, admin.MessageQuery{Field: "sender_domain", Text: "example.com", Limit: 10})
	if err != nil || len(found.Messages) != 2 || found.Messages[0].SMTP == nil {
		t.Fatal("search", found, err)
	}
}

// A submission locks the app and then shares the credential. Revocation must
// take the same order, or it deadlocks against a submission in flight and the
// credential stays live.
func TestSMTPRevocationDoesNotDeadlockWithASubmission(t *testing.T) {
	ctx, repo, exec, _ := smtpOwnerFixture(t)
	app := strings.Repeat("a", 32)
	exec(`INSERT INTO apps(id,name) VALUES ($1,'A')`, app)
	issued, err := repo.IssueSMTPCredential(ctx, "operator", app, "printer")
	if err != nil {
		t.Fatal(err)
	}
	// Stand-in for a submission: app FOR UPDATE now, credential FOR SHARE later.
	sub, err := repo.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Rollback()
	if _, err = sub.ExecContext(ctx, `SELECT 1 FROM apps WHERE id=$1 FOR UPDATE`, app); err != nil {
		t.Fatal(err)
	}
	revoked := make(chan error, 1)
	go func() { revoked <- repo.RevokeSMTPCredential(ctx, "operator", app, issued.Username) }()
	time.Sleep(300 * time.Millisecond) // let the revocation start waiting
	if _, err = sub.ExecContext(ctx, `SELECT 1 FROM smtp_credentials WHERE id=$1 FOR SHARE`, issued.Username); err != nil {
		t.Fatal("submission side:", err)
	}
	if err = sub.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-revoked; err != nil {
		t.Fatal("revocation:", err)
	}
}
