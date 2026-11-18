package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/bounceingest"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/tracking"
)

type trackingObjects struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (o *trackingObjects) Put(_ context.Context, key string, value []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.values[key]; exists {
		return errors.New("object exists")
	}
	o.values[key] = bytes.Clone(value)
	return nil
}
func (o *trackingObjects) Get(_ context.Context, key string, max int64) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	value, exists := o.values[key]
	if !exists || int64(len(value)) > max {
		return nil, errors.New("object unavailable")
	}
	return bytes.Clone(value), nil
}
func (o *trackingObjects) VerifyPresent(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.values[key]; !exists {
		return errors.New("object unavailable")
	}
	return nil
}

func TestTrackingPostgresAtomicReferencesAndBoundedEvents(t *testing.T) {
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
	schema := "tracking_test_" + hex.EncodeToString(random[:])
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
	if _, err = owner.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:17] {
		body, checksum, readErr := migrationBody(migration)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, err = owner.ExecContext(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
		if _, err = owner.ExecContext(ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, migration.version, checksum); err != nil {
			t.Fatal(err)
		}
	}
	if err = CheckSchema(ctx, owner); err == nil {
		t.Fatal("mail runtime accepted schema 17")
	}
	// The new binary must still validate the staged schema-17 operator role
	// before migration 18. All fixture role changes roll back with this tx.
	pre18, err := owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	preRole := "tracking_pre18_" + hex.EncodeToString(random[:])
	quotedPreRole := pgx.Identifier{preRole}.Sanitize()
	var database string
	if err = pre18.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE ROLE " + quotedPreRole + " LOGIN",
		"REVOKE TEMPORARY ON DATABASE " + pgx.Identifier{database}.Sanitize() + " FROM PUBLIC",
		"GRANT USAGE ON SCHEMA " + pgx.Identifier{schema}.Sanitize() + " TO " + quotedPreRole,
		"GRANT SELECT, UPDATE (lock_guard) ON runtime_gate TO " + quotedPreRole,
	} {
		if _, err = pre18.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err = checkRuntimeRolePrivileges(ctx, pre18, preRole, true); err != nil {
		t.Fatal("schema 17 operator role rejected", err)
	}
	if err = pre18.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('legacy-app','Legacy');
		INSERT INTO messages(id,app_id,object_key,require_tls,state) VALUES ('legacy-message','legacy-app','legacy-source',false,'prepared');
		INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ('legacy-delivery','legacy-message',0,'legacy-final');
		INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch) VALUES ('legacy-attempt','legacy-delivery','legacy-worker',1),('legacy-attempt-2','legacy-delivery','legacy-worker',1),('legacy-attempt-3','legacy-delivery','legacy-worker',1);
		INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ('legacy-event','legacy-app','legacy-message','preparation_failed',0);
		INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code) VALUES
		('legacy-delivery-event','legacy-app','legacy-message','legacy-delivery','legacy-attempt','rejected',450),
		('legacy-arbitrary-event','legacy-app','legacy-message','legacy-delivery','legacy-attempt-2','opened',250),
		('legacy-collision','legacy-app','legacy-message','legacy-delivery','legacy-attempt-3','tracking_abandoned',0)`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, owner); err == nil {
		t.Fatal("migration accepted a historical outcome collision")
	}
	if count, countErr := appliedCount(ctx, owner); countErr != nil || count != 17 {
		t.Fatal("failed migration did not roll back", count, countErr)
	}
	if _, err = owner.ExecContext(ctx, `DELETE FROM webhook_events WHERE id='legacy-collision'`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, owner); err != nil {
		t.Fatal("tracking migrations", err)
	}
	if err = CheckSchema(ctx, owner); err != nil {
		t.Fatal("current schema", err)
	}
	var secureDefiner, fixedPath, trustedOwner, privateFunction bool
	err = owner.QueryRowContext(ctx, `SELECT p.prosecdef,p.proconfig @> ARRAY['search_path=pg_catalog'],p.proowner=n.nspowner,NOT has_function_privilege('public',p.oid,'EXECUTE')
		FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE p.oid='prevent_tracking_mapping_revival()'::regprocedure`).Scan(&secureDefiner, &fixedPath, &trustedOwner, &privateFunction)
	if err != nil || !secureDefiner || !fixedPath || !trustedOwner || !privateFunction {
		t.Fatal("retirement audit function ownership or permissions are unsafe", err)
	}
	var legacyCount int
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM webhook_events WHERE tracking_event_id IS NULL AND ((id='legacy-event' AND outcome='preparation_failed') OR (id='legacy-delivery-event' AND outcome='rejected') OR (id='legacy-arbitrary-event' AND outcome='opened'))`).Scan(&legacyCount); err != nil || legacyCount != 3 {
		t.Fatal("migration changed historical events", legacyCount, err)
	}
	const generation = "00000000000000000000000000000aaa"
	id := func(n int) string { return fmt.Sprintf("%032x", n) }
	app, other, message, deliveryID, sub, otherSub := id(1), id(2), id(3), id(4), id(5), id(6)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, []any{generation}},
		{`INSERT INTO apps(id,name) VALUES ($1,'One'),($2,'Other')`, []any{app, other}},
		{`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, []any{app}},
		{`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state,sender_domain,sender_domain_rev,subject,recipient_count) VALUES ($1,$2,'source',true,'queued','example.com','com.example.','s',1)`, []any{message, app}},
		{`INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext) VALUES ($1,$3,'https://example.net/events','\x01'::bytea),($2,$4,'https://example.net/events','\x01'::bytea)`, []any{sub, otherSub, app, other}},
	} {
		if _, err = owner.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	db, cleanup := restrictedRuntimeForTest(t, ctx, owner, u.String())
	defer cleanup()
	for _, statement := range []string{
		`UPDATE tracking_objects SET expires_at=clock_timestamp()`,
		`UPDATE tracking_objects SET revoked_at=clock_timestamp()`,
		`UPDATE tracking_objects SET retired_at=clock_timestamp()`,
		`UPDATE tracking_links SET token_hash=decode(repeat('aa',32),'hex')`,
		`DELETE FROM tracking_events`,
	} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Fatal("runtime obtained unsafe tracking privilege", statement)
		}
	}
	var runtimeRole string
	if err = db.QueryRowContext(ctx, `SELECT current_user`).Scan(&runtimeRole); err != nil {
		t.Fatal(err)
	}
	quotedRole := pgx.Identifier{runtimeRole}.Sanitize()
	for _, grant := range []struct{ give, take string }{
		{"GRANT UPDATE (object_key) ON tracking_objects TO ", "REVOKE UPDATE (object_key) ON tracking_objects FROM "},
		{"GRANT UPDATE (retired_at) ON tracking_objects TO ", "REVOKE UPDATE (retired_at) ON tracking_objects FROM "},
		{"GRANT DELETE ON tracking_events TO ", "REVOKE DELETE ON tracking_events FROM "},
	} {
		if _, err = owner.ExecContext(ctx, grant.give+quotedRole); err != nil {
			t.Fatal(err)
		}
		if err = CheckRuntimeGatePrivileges(ctx, db); err == nil {
			t.Fatal("unsafe tracking grant accepted")
		}
		if _, err = owner.ExecContext(ctx, grant.take+quotedRole); err != nil {
			t.Fatal(err)
		}
	}
	if err = CheckRuntimeGatePrivileges(ctx, db); err != nil {
		t.Fatal("safe tracking grants rejected", err)
	}
	raw := &trackingObjects{values: make(map[string][]byte)}
	keyFile := filepath.Join(t.TempDir(), "keyring.json")
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	if err = os.WriteFile(keyFile, []byte(fmt.Sprintf(`{"active":"fixture","keys":{"fixture":%q}}`, encoded)), 0600); err != nil {
		t.Fatal(err)
	}
	ring, err := objectstore.LoadKeyring(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := objectstore.NewEncrypted(raw, ring)
	if err != nil {
		t.Fatal(err)
	}
	protected, err := tracking.NewProtectedStore(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	repo := &Repository{DB: db, Generation: generation, Objects: encrypted}
	quota, err := repo.GetTrackingQuota(ctx, app)
	if err != nil || quota.DailyLimit != 0 || quota.Used != 0 || quota.Suppressed != 0 {
		t.Fatal("new tracking quota not closed", quota, err)
	}
	quota, err = repo.SetTrackingQuota(ctx, "operator", app, 4)
	if err != nil || quota.DailyLimit != 4 {
		t.Fatal("configure tracking quota", quota, err)
	}
	claim, err := repo.ClaimPreparation(ctx, id(7), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	claim.SigningDomain = "example.com"
	finalKey, err := objectkey.New("final", deliveryID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = encrypted.PutMIME(ctx, finalKey, []byte("fixture signed MIME")); err != nil {
		t.Fatal(err)
	}
	trackingKey, err := tracking.NewMappingKey(deliveryID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	links := []tracking.Mapping{{Token: "000102030405060708090a0b0c0d0e0f", Kind: "open"}, {Token: "101112131415161718191a1b1c1d1e1f", Kind: "click", Destination: "https://example.net/private?patient=fixture"}, {Token: "202122232425262728292a2b2c2d2e2f", Kind: "click", Destination: "https://example.net/other"}}
	placeholder := tracking.Reference{ID: id(8), Hash: [32]byte{1}, Kind: "open"}
	deliveries := []preparation.Delivery{{ID: deliveryID, RecipientIndex: 0, ObjectKey: finalKey, Tracking: &preparation.Tracking{ObjectKey: trackingKey, ExpiresAt: time.Now().Add(30 * 24 * time.Hour), PostSendValiditySeconds: 86400, EventLimit: 3, Links: []tracking.Reference{placeholder}}}}
	if err = repo.CommitPreparation(ctx, claim, deliveries); err == nil {
		t.Fatal("missing encrypted mapping was committed")
	}
	var count int
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE id=$1`, deliveryID).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed object check left delivery", count, err)
	}
	refs, err := protected.Put(ctx, trackingKey, links)
	if err != nil {
		t.Fatal(err)
	}
	deliveries[0].Tracking.Links = refs
	deliveries[0].Tracking.ExpiresAt = time.Now().Add(24*time.Hour + 5*time.Minute)
	if err = repo.CommitPreparation(ctx, claim, deliveries); err == nil {
		t.Fatal("committed mapping without its post-send validity and safety margin")
	}
	deliveries[0].Tracking.ExpiresAt = time.Now().Add(30 * 24 * time.Hour)
	if err = repo.CommitPreparation(ctx, claim, deliveries); err != nil {
		t.Fatal("atomic publication", err)
	}
	service := &tracking.Service{Repository: repo, Objects: protected}
	const linkOrigin = "https://tracking.example.com"
	linkKeys, err := tracking.NewLinkKeys([]byte("0123456789abcdef0123456789abcdef"), nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := &tracking.Handler{Origin: linkOrigin, Links: linkKeys, Visitor: service}
	linkPath := func(m tracking.Mapping) string {
		link, linkErr := linkKeys.Link(linkOrigin, m.Kind, m.Token, m.Destination)
		if linkErr != nil {
			t.Fatal(linkErr)
		}
		return strings.TrimPrefix(link, linkOrigin)
	}
	request := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	openPath := linkPath(links[0])
	clickPath := linkPath(links[1])
	if w := request("HEAD", openPath); w.Code != 200 {
		t.Fatal("HEAD lookup", w.Code)
	}
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM tracking_events`).Scan(&count); err != nil || count != 0 {
		t.Fatal("HEAD recorded an event", count, err)
	}
	if w := request("GET", openPath); w.Code != 200 {
		t.Fatal("open lookup", w.Code)
	}
	// The handler records best effort, so exercise the bounded recording
	// directly, where a failed visit is visible: 1 above plus 11 here.
	openHash, err := tracking.HashToken(links[0].Token)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 11
	results := make(chan error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- service.Visit(context.Background(), openHash, "open", true)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for visitErr := range results {
		if visitErr != nil {
			t.Fatal("open visit failed", visitErr)
		}
	}
	var events, ownHooks, foreignHooks int
	if err = owner.QueryRowContext(ctx, `SELECT (SELECT event_count FROM tracking_links WHERE id=$1),(SELECT count(*) FROM tracking_events WHERE link_id=$1),(SELECT count(*) FROM webhook_deliveries WHERE subscription_id=$2),(SELECT count(*) FROM webhook_deliveries WHERE subscription_id=$3)`, refs[0].ID, sub, otherSub).Scan(&count, &events, &ownHooks, &foreignHooks); err != nil || count != 3 || events != 3 || ownHooks != 3 || foreignHooks != 0 {
		t.Fatal("bounded event fanout", count, events, ownHooks, foreignHooks, err)
	}
	quota, err = repo.GetTrackingQuota(ctx, app)
	if err != nil || quota.Used != 3 || quota.Suppressed != 9 {
		t.Fatal("per-link suppression was not counted", quota, err)
	}
	if _, err = owner.ExecContext(ctx, `CREATE FUNCTION fail_tracking_webhook() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.tracking_event_id IS NOT NULL THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_tracking_webhook BEFORE INSERT ON webhook_events FOR EACH ROW EXECUTE FUNCTION fail_tracking_webhook()`); err != nil {
		t.Fatal(err)
	}
	// Analytics are best effort for encrypted links: the redirect survives a
	// failed event transaction, but nothing may be recorded.
	if w := request("GET", clickPath); w.Code != 302 || w.Header().Get("Location") != links[1].Destination {
		t.Fatal("failed event transaction broke the redirect", w.Code)
	}
	if err = owner.QueryRowContext(ctx, `SELECT event_count,(SELECT count(*) FROM tracking_events WHERE link_id=$1) FROM tracking_links WHERE id=$1`, refs[1].ID).Scan(&count, &events); err != nil || count != 0 || events != 0 {
		t.Fatal("event failure did not roll back", count, events, err)
	}
	if _, err = owner.ExecContext(ctx, `DROP TRIGGER fail_tracking_webhook ON webhook_events; DROP FUNCTION fail_tracking_webhook()`); err != nil {
		t.Fatal(err)
	}
	// Independent links contend for the same single remaining app event slot.
	// As above, recording is exercised directly so a failed visit is visible.
	var clickHashes [2][32]byte
	for i := range clickHashes {
		if clickHashes[i], err = tracking.HashToken(links[1+i].Token); err != nil {
			t.Fatal(err)
		}
	}
	const clickWorkers = 12
	clickResults := make(chan error, clickWorkers)
	clickStart := make(chan struct{})
	for i := 0; i < clickWorkers; i++ {
		hash := clickHashes[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-clickStart
			clickResults <- service.Visit(context.Background(), hash, "click", true)
		}()
	}
	close(clickStart)
	wg.Wait()
	close(clickResults)
	for visitErr := range clickResults {
		if visitErr != nil {
			t.Fatal("click visit failed under shared app budget", visitErr)
		}
	}
	quota, err = repo.GetTrackingQuota(ctx, app)
	if err != nil || quota.Used != 4 || quota.Suppressed != 20 {
		t.Fatal("app quota was not shared across links or failure spent budget", quota, err)
	}
	otherQuota, err := repo.GetTrackingQuota(ctx, other)
	if err != nil || otherQuota.DailyLimit != 0 || otherQuota.Used != 0 || otherQuota.Suppressed != 0 {
		t.Fatal("tracking quota affected another app", otherQuota, err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM tracking_events WHERE link_id IN ($1,$2)`, refs[1].ID, refs[2].ID).Scan(&events); err != nil || events != 1 {
		t.Fatal("app limit permitted excess click events", events, err)
	}
	quota, err = repo.SetTrackingQuota(ctx, "operator", app, 2)
	if err != nil || quota.Used != 4 || quota.Suppressed != 20 {
		t.Fatal("limit change reset usage", quota, err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='app.tracking_quota.updated'`, app).Scan(&count); err != nil || count != 2 {
		t.Fatal("tracking quota audit missing", count, err)
	}
	if _, err = repo.SetTrackingQuota(ctx, "operator", app, 0); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", clickPath); w.Code != 302 {
		t.Fatal("zero budget prevented link resolution", w.Code)
	}
	quota, err = repo.GetTrackingQuota(ctx, app)
	if err != nil || quota.Used != 4 || quota.Suppressed != 21 {
		t.Fatal("zero budget recorded an event", quota, err)
	}
	if _, err = owner.ExecContext(ctx, `CREATE FUNCTION fail_tracking_quota_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='app.tracking_quota.updated' THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_tracking_quota_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_tracking_quota_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.SetTrackingQuota(ctx, "operator", app, 9); err == nil {
		t.Fatal("quota changed without audit")
	}
	if _, err = owner.ExecContext(ctx, `DROP TRIGGER fail_tracking_quota_audit ON audit_events; DROP FUNCTION fail_tracking_quota_audit()`); err != nil {
		t.Fatal(err)
	}
	quota, err = repo.GetTrackingQuota(ctx, app)
	if err != nil || quota.DailyLimit != 0 || quota.Used != 4 {
		t.Fatal("failed audit did not roll back quota", quota, err)
	}
	if _, err = repo.SetTrackingQuota(ctx, "operator", app, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE apps SET tracking_quota_day=(clock_timestamp() AT TIME ZONE 'UTC')::date-1 WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", clickPath); w.Code != 302 {
		t.Fatal("quota did not reopen on a new UTC day", w.Code)
	}
	quota, err = repo.GetTrackingQuota(ctx, app)
	if err != nil || quota.Used != 1 || quota.Suppressed != 0 {
		t.Fatal("new day did not reset tracking budget", quota, err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE apps SET tracking_quota_day=(clock_timestamp() AT TIME ZONE 'UTC')::date+1,tracking_quota_used=2,tracking_quota_suppressed=1000000000 WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", clickPath); w.Code != 302 {
		t.Fatal("link failed with backward clock and saturated counter", w.Code)
	}
	quota, err = repo.GetTrackingQuota(ctx, app)
	if err != nil || quota.Used != 2 || quota.Suppressed != 1000000000 || quota.Day <= time.Now().UTC().Format("2006-01-02") {
		t.Fatal("backward clock or saturation reopened budget", quota, err)
	}
	// An older release may have allowed more active subscriptions. Suppress
	// tracking events until an operator resolves the excess; never drop an
	// arbitrary subset of webhook deliveries.
	if _, err = owner.ExecContext(ctx, `UPDATE apps SET tracking_quota_day=(clock_timestamp() AT TIME ZONE 'UTC')::date-1 WHERE id=$1`, app); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext) SELECT md5('legacy-hook-'||n),$1,'https://example.net/events','\x01'::bytea FROM generate_series(1,10) n`, app); err != nil {
		t.Fatal(err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM tracking_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", clickPath); w.Code != 302 {
		t.Fatal("legacy subscription overflow prevented link resolution", w.Code)
	}
	var afterOverflow int
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM tracking_events`).Scan(&afterOverflow); err != nil || events != afterOverflow {
		t.Fatal("tracking event exceeded subscription fanout limit", events, afterOverflow, err)
	}
	quota, err = repo.GetTrackingQuota(ctx, app)
	if err != nil || quota.Used != 0 || quota.Suppressed != 1 {
		t.Fatal("legacy fanout suppression not visible", quota, err)
	}
	// Non-tracking events fail their whole transaction on a legacy excess;
	// neither an event nor a partial set of deliveries may be retained.
	failedMessage, failedToken := id(500), id(501)
	if _, err = owner.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state,prepare_token,prepare_attempts) VALUES ($1,$2,'overflow-source',false,'preparing',$3,12)`, failedMessage, app, failedToken); err != nil {
		t.Fatal(err)
	}
	failureTx, err := owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = finishPreparationFailure(ctx, failureTx, failedMessage, failedToken, 12, "source_read"); !errors.Is(err, errWebhookFanoutExceeded) {
		failureTx.Rollback()
		t.Fatal("over-limit preparation failure was not rejected", err)
	}
	if err = failureTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = owner.QueryRowContext(ctx, `SELECT state,(SELECT count(*) FROM webhook_events WHERE message_id=$1) FROM messages WHERE id=$1`, failedMessage).Scan(&state, &count); err != nil || state != "preparing" || count != 0 {
		t.Fatal("over-limit preparation transaction was not rolled back", state, count, err)
	}
	bounceID := id(503)
	bounceKey, err := objectkey.New("bounces", bounceID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bounce := bounceingest.Record{ID: bounceID, ObjectKey: bounceKey, TokenHash: [32]byte{1, 2}, Fingerprint: [32]byte{3, 4}}
	if _, err = owner.ExecContext(ctx, `UPDATE deliveries SET bounce_key_version='fixture',bounce_token_hash=$2 WHERE id=$1`, deliveryID, bounce.TokenHash[:]); err != nil {
		t.Fatal(err)
	}
	if err = encrypted.PutMIME(ctx, bounceKey, []byte("synthetic bounce")); err != nil {
		t.Fatal(err)
	}
	if err = repo.CommitBounce(ctx, bounce); !errors.Is(err, errWebhookFanoutExceeded) {
		t.Fatal("over-limit bounce was not rejected", err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM bounce_receipts WHERE id=$1)+(SELECT count(*) FROM webhook_events WHERE id=$1)`, bounceID).Scan(&count); err != nil || count != 0 {
		t.Fatal("over-limit bounce transaction was not rolled back", count, err)
	}
	attemptID := id(504)
	if _, err = owner.ExecContext(ctx, `INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized) VALUES ($1,$2,'fixture-worker',1,false)`, attemptID, deliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE deliveries SET state='in_flight',current_attempt_id=$2 WHERE id=$1`, deliveryID, attemptID); err != nil {
		t.Fatal(err)
	}
	deliveryResult := delivery.Result{AttemptID: attemptID, DeliveryID: deliveryID, WorkerID: "fixture-worker", Epoch: 1, Outcome: "uncertain"}
	if err = repo.CommitDeliveryResult(ctx, deliveryResult); !errors.Is(err, errWebhookFanoutExceeded) {
		t.Fatal("over-limit delivery result was not rejected", err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT state,(SELECT count(*) FROM webhook_events WHERE id=$2) FROM deliveries WHERE id=$1`, deliveryID, attemptID).Scan(&state, &count); err != nil || state != "in_flight" || count != 0 {
		t.Fatal("over-limit delivery transaction was not rolled back", state, count, err)
	}
	if _, err = owner.ExecContext(ctx, `DELETE FROM webhook_subscriptions WHERE id=md5('legacy-hook-10')`); err != nil {
		t.Fatal(err)
	}
	failureTx, err = owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = finishPreparationFailure(ctx, failureTx, failedMessage, failedToken, 12, "source_read"); err != nil {
		failureTx.Rollback()
		t.Fatal("exactly ten subscriptions were rejected", err)
	}
	if err = failureTx.QueryRowContext(ctx, `SELECT count(*) FROM webhook_deliveries w JOIN webhook_events e ON e.id=w.event_id WHERE e.message_id=$1`, failedMessage).Scan(&count); err != nil || count != maxActiveWebhooks {
		failureTx.Rollback()
		t.Fatal("at-limit event did not fan out to every subscription", count, err)
	}
	if err = failureTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `DELETE FROM webhook_subscriptions WHERE app_id=$1 AND id<>$2`, app, sub); err != nil {
		t.Fatal(err)
	}
	if err = repo.CommitBounce(ctx, bounce); err != nil {
		t.Fatal("bounce did not recover after reducing subscriptions", err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM bounce_receipts WHERE id=$1)+(SELECT count(*) FROM webhook_events WHERE id=$1)+(SELECT count(*) FROM webhook_deliveries WHERE event_id=$1)`, bounceID).Scan(&count); err != nil || count != 3 {
		t.Fatal("bounce recovery did not commit the full event", count, err)
	}
	if err = repo.CommitDeliveryResult(ctx, deliveryResult); err != nil {
		t.Fatal("delivery result did not recover after reducing subscriptions", err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT state,(SELECT count(*) FROM webhook_events WHERE id=$2)+(SELECT count(*) FROM webhook_deliveries WHERE event_id=$2) FROM deliveries WHERE id=$1`, deliveryID, attemptID).Scan(&state, &count); err != nil || state != "uncertain" || count != 2 {
		t.Fatal("delivery recovery did not commit the full event", state, count, err)
	}
	failureTx, err = owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = finishPreparationFailure(ctx, failureTx, failedMessage, failedToken, 12, "source_read"); err != nil {
		failureTx.Rollback()
		t.Fatal("preparation did not recover after reducing subscriptions", err)
	}
	if err = failureTx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT state,(SELECT count(*) FROM webhook_deliveries w JOIN webhook_events e ON e.id=w.event_id WHERE e.message_id=$1) FROM messages WHERE id=$1`, failedMessage).Scan(&state, &count); err != nil || state != "preparation_failed" || count != 1 {
		t.Fatal("preparation recovery did not commit the event", state, count, err)
	}
	// An open token presented as a click is a valid encrypted link, but the
	// store must not record a click for it.
	var beforeConfusion, afterConfusion int
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM tracking_events`).Scan(&beforeConfusion); err != nil {
		t.Fatal(err)
	}
	confused := linkPath(tracking.Mapping{Token: links[0].Token, Kind: "click", Destination: "https://example.net/other"})
	if w := request("GET", confused); w.Code != 302 {
		t.Fatal("encrypted link did not resolve", w.Code)
	}
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM tracking_events`).Scan(&afterConfusion); err != nil || afterConfusion != beforeConfusion {
		t.Fatal("tracking kind confusion recorded an event", err)
	}
	claimHook, err := repo.ClaimWebhook(ctx, id(9), 45*time.Second)
	if err != nil || claimHook.AppID != app || claimHook.Event.TrackingLinkID != refs[0].ID || claimHook.Event.Outcome != "opened" {
		t.Fatal("tracking webhook not scoped", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE tracking_objects SET revoked_at=clock_timestamp() WHERE delivery_id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	if err = repo.RecordTrackingVisit(ctx, refs[1].Hash, "click", trackingKey, true); !errors.Is(err, tracking.ErrNotFound) {
		t.Fatal("revocation was not rechecked", err)
	}
	// Revocation stops analytics, not the redirect (links are self-contained).
	var beforeRevoked, afterRevoked int
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM tracking_events`).Scan(&beforeRevoked); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", clickPath); w.Code != 302 {
		t.Fatal("revoked mapping broke the redirect", w.Code)
	}
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM tracking_events`).Scan(&afterRevoked); err != nil || afterRevoked != beforeRevoked {
		t.Fatal("revoked link recorded an event", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE tracking_objects SET revoked_at=NULL,expires_at=clock_timestamp()+interval '500 milliseconds' WHERE delivery_id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	lock, err := owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	var locked bool
	if err = lock.QueryRowContext(ctx, `SELECT true FROM tracking_objects WHERE delivery_id=$1 FOR UPDATE`, deliveryID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan error, 1)
	go func() { waiting <- repo.RecordTrackingVisit(ctx, refs[1].Hash, "click", trackingKey, true) }()
	select {
	case err = <-waiting:
		t.Fatal("visit did not wait for reference lock", err)
	case <-time.After(100 * time.Millisecond):
	}
	time.Sleep(500 * time.Millisecond)
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-waiting; !errors.Is(err, tracking.ErrNotFound) {
		t.Fatal("link resolved after expiry during lock wait", err)
	}
	// A visit can also expire after it takes the object lock while waiting on
	// the app's shared budget. It must not consume budget or return a redirect.
	if _, err = owner.ExecContext(ctx, `UPDATE tracking_objects SET expires_at=clock_timestamp()+interval '500 milliseconds' WHERE delivery_id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	budgetLock, err := owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer budgetLock.Rollback()
	if err = budgetLock.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 FOR UPDATE`, app).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	go func() { waiting <- repo.RecordTrackingVisit(ctx, refs[1].Hash, "click", trackingKey, true) }()
	select {
	case err = <-waiting:
		t.Fatal("visit did not wait for app budget lock", err)
	case <-time.After(100 * time.Millisecond):
	}
	time.Sleep(500 * time.Millisecond)
	if err = budgetLock.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-waiting; !errors.Is(err, tracking.ErrNotFound) {
		t.Fatal("link resolved after expiry during budget lock wait", err)
	}
	// Even an expired mapping reference protects its encrypted object until a
	// separately reviewed retention process removes the reference.
	oldKey, err := tracking.NewMappingKey(deliveryID, time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE tracking_objects SET object_key=$2 WHERE delivery_id=$1`, deliveryID, oldKey); err != nil {
		t.Fatal(err)
	}
	horizon, err := AdvanceObjectGCHorizon(ctx, owner, generation, "operator", 24*time.Hour)
	if err != nil {
		t.Fatal("advance fixture horizon", err)
	}
	removed := false
	deleted, err := repo.CollectUnreferenced(ctx, oldKey, horizon, func(context.Context, string, time.Time) (bool, error) { removed = true; return true, nil })
	if err != nil || deleted || removed {
		t.Fatal("referenced tracking object eligible for collection", deleted, err)
	}
	retireRole := "tracking_retire_" + hex.EncodeToString(random[:])
	quotedRetireRole := pgx.Identifier{retireRole}.Sanitize()
	badRole := "tracking_bad_" + hex.EncodeToString(random[:])
	quotedBadRole := pgx.Identifier{badRole}.Sanitize()
	if _, err = owner.ExecContext(ctx, "CREATE ROLE "+quotedBadRole+" LOGIN"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = owner.ExecContext(context.Background(), "DROP OWNED BY "+quotedBadRole)
		_, _ = owner.ExecContext(context.Background(), "DROP ROLE "+quotedBadRole)
	}()
	if _, err = owner.ExecContext(ctx, "GRANT SELECT ON messages TO "+quotedBadRole); err != nil {
		t.Fatal(err)
	}
	if err = GrantTrackingRetirementRole(ctx, owner, badRole); err == nil {
		t.Fatal("unsafe pre-existing role was granted retirement privileges")
	}
	var leakedGrant bool
	if err = owner.QueryRowContext(ctx, `SELECT has_column_privilege($1,'tracking_objects','object_key','UPDATE')`, badRole).Scan(&leakedGrant); err != nil || leakedGrant {
		t.Fatal("failed retirement grant leaked privileges", err)
	}
	if _, err = owner.ExecContext(ctx, "CREATE ROLE "+quotedRetireRole+" LOGIN PASSWORD 'disposable-retirement-password'"); err != nil {
		t.Fatal(err)
	}
	if err = GrantTrackingRetirementRole(ctx, owner, retireRole); err != nil {
		t.Fatal("grant tracking retirement role", err)
	}
	if err = GrantTrackingRetirementRole(ctx, owner, retireRole); err != nil {
		t.Fatal("repeat tracking retirement grants", err)
	}
	retireURL := *u
	retireURL.User = url.UserPassword(retireRole, "disposable-retirement-password")
	retireDB, err := sql.Open("pgx", retireURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		retireDB.Close()
		_, _ = owner.ExecContext(context.Background(), "DROP OWNED BY "+quotedRetireRole)
		_, _ = owner.ExecContext(context.Background(), "DROP ROLE "+quotedRetireRole)
	}()
	if err = CheckTrackingRetirementRolePrivileges(ctx, retireDB); err != nil {
		t.Fatal("tracking retirement role rejected", err)
	}
	for _, statement := range []string{`SELECT 1 FROM message_recipients`, `SELECT subject FROM messages`, `SELECT sender_domain FROM messages`} {
		if _, err = retireDB.ExecContext(ctx, statement); err == nil {
			t.Fatal("retirement role can read recorded message metadata", statement)
		}
	}
	for _, tamper := range []struct{ apply, restore string }{
		{"ALTER TABLE tracking_objects DISABLE TRIGGER tracking_object_retirement_monotonic", "ALTER TABLE tracking_objects ENABLE TRIGGER tracking_object_retirement_monotonic"},
		{"GRANT EXECUTE ON FUNCTION prevent_tracking_mapping_revival() TO " + quotedBadRole, "REVOKE EXECUTE ON FUNCTION prevent_tracking_mapping_revival() FROM " + quotedBadRole},
		{"ALTER FUNCTION prevent_tracking_mapping_revival() SECURITY INVOKER", "ALTER FUNCTION prevent_tracking_mapping_revival() SECURITY DEFINER"},
	} {
		if _, err = owner.ExecContext(ctx, tamper.apply); err != nil {
			t.Fatal(err)
		}
		if err = CheckTrackingRetirementRolePrivileges(ctx, retireDB); err == nil {
			t.Fatal("retirement accepted a tampered audit boundary", tamper.apply)
		}
		if err = GrantTrackingRetirementRole(ctx, owner, retireRole); err == nil {
			t.Fatal("retirement grant accepted a tampered audit boundary", tamper.apply)
		}
		if _, err = owner.ExecContext(ctx, tamper.restore); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = owner.ExecContext(ctx, `CREATE OR REPLACE FUNCTION prevent_tracking_mapping_revival() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$ BEGIN RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if err = CheckTrackingRetirementRolePrivileges(ctx, retireDB); err == nil {
		t.Fatal("retirement accepted a changed audit function body")
	}
	auditMigration, _, err := migrationBody(migrations[20])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, string(auditMigration)); err != nil {
		t.Fatal(err)
	}
	if err = CheckTrackingRetirementRolePrivileges(ctx, retireDB); err != nil {
		t.Fatal("restored retirement audit boundary rejected", err)
	}
	for _, grant := range []struct{ give, take string }{
		{"GRANT INSERT ON audit_events TO ", "REVOKE INSERT ON audit_events FROM "},
		{"GRANT SELECT ON messages TO ", "REVOKE SELECT ON messages FROM "},
		{"GRANT SELECT (id) ON messages TO ", "REVOKE SELECT (id) ON messages FROM "},
		{"GRANT UPDATE (expires_at) ON tracking_objects TO ", "REVOKE UPDATE (expires_at) ON tracking_objects FROM "},
		{"GRANT EXECUTE ON FUNCTION prevent_tracking_mapping_revival() TO ", "REVOKE EXECUTE ON FUNCTION prevent_tracking_mapping_revival() FROM "},
	} {
		if _, err = owner.ExecContext(ctx, grant.give+quotedRetireRole); err != nil {
			t.Fatal(err)
		}
		if err = CheckTrackingRetirementRolePrivileges(ctx, retireDB); err == nil {
			t.Fatal("unsafe retirement role privilege accepted")
		}
		if err = GrantTrackingRetirementRole(ctx, owner, retireRole); err == nil {
			t.Fatal("grant command accepted unsafe retirement role")
		}
		if _, err = owner.ExecContext(ctx, grant.take+quotedRetireRole); err != nil {
			t.Fatal(err)
		}
	}
	if err = CheckTrackingRetirementRolePrivileges(ctx, retireDB); err != nil {
		t.Fatal("safe retirement role rejected after revocation", err)
	}
	if _, err = owner.ExecContext(ctx, "REVOKE SELECT (object_key) ON tracking_objects FROM "+quotedRetireRole); err != nil {
		t.Fatal(err)
	}
	if err = CheckTrackingRetirementRolePrivileges(ctx, retireDB); err == nil {
		t.Fatal("retirement role accepted without required object-key read")
	}
	if err = GrantTrackingRetirementRole(ctx, owner, retireRole); err != nil {
		t.Fatal("could not restore missing retirement privilege", err)
	}
	parentRole := "tracking_retire_parent_" + hex.EncodeToString(random[:])
	quotedParentRole := pgx.Identifier{parentRole}.Sanitize()
	if _, err = owner.ExecContext(ctx, "CREATE ROLE "+quotedParentRole); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = owner.ExecContext(context.Background(), "DROP OWNED BY "+quotedParentRole)
		_, _ = owner.ExecContext(context.Background(), "DROP ROLE "+quotedParentRole)
	}()
	if _, err = owner.ExecContext(ctx, "GRANT SELECT ON messages TO "+quotedParentRole); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, "GRANT "+quotedParentRole+" TO "+quotedRetireRole); err != nil {
		t.Fatal(err)
	}
	if err = CheckTrackingRetirementRolePrivileges(ctx, retireDB); err == nil {
		t.Fatal("inherited unsafe retirement privilege accepted")
	}
	if _, err = owner.ExecContext(ctx, "REVOKE "+quotedParentRole+" FROM "+quotedRetireRole); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`UPDATE deliveries SET state='queued'`, `UPDATE tracking_objects SET expires_at=clock_timestamp()`, `DELETE FROM audit_events`, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id) VALUES ('fake','fake','fake','fake','fake')`, `SELECT final_object_key FROM deliveries`, `SELECT message_id FROM deliveries`, `SELECT id FROM messages`, `SELECT token_hash FROM tracking_links`, `SELECT * FROM recovery_holds`, `SELECT * FROM recovery_events`} {
		if _, err = retireDB.ExecContext(ctx, statement); err == nil {
			t.Fatal("retirement role has an unsafe privilege", statement)
		}
	}
	if err = CheckRuntimeGatePrivileges(ctx, retireDB); err == nil {
		t.Fatal("retirement role was accepted as a runtime identity")
	}
	maintenance := &Repository{DB: retireDB, Generation: generation}
	directRetire := func(target, actor, expectedGeneration string, commit bool) error {
		direct, beginErr := retireDB.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer direct.Rollback()
		if _, beginErr = direct.ExecContext(ctx, `SELECT set_config('thinpost.retirement_actor',$1,true),set_config('thinpost.retirement_generation',$2,true)`, actor, expectedGeneration); beginErr != nil {
			return beginErr
		}
		if _, beginErr = direct.ExecContext(ctx, `UPDATE tracking_objects SET object_key=NULL,retired_at=clock_timestamp() WHERE delivery_id=$1`, target); beginErr != nil {
			return beginErr
		}
		if commit {
			return direct.Commit()
		}
		return nil
	}
	for _, protectedState := range []string{"queued", "in_flight", "uncertain", "recipient_policy_held"} {
		if _, err = owner.ExecContext(ctx, `UPDATE deliveries SET state=$2 WHERE id=$1`, deliveryID, protectedState); err != nil {
			t.Fatal(err)
		}
		retired, retireErr := maintenance.RetireExpiredTrackingMappings(ctx, "operator", 10)
		if retireErr != nil || retired != 0 {
			t.Fatal("nonterminal tracking mapping retired", protectedState, retired, retireErr)
		}
		if err = directRetire(deliveryID, "operator", generation, false); err == nil {
			t.Fatal("database allowed retirement of a nonterminal mapping", protectedState)
		}
	}
	if _, err = owner.ExecContext(ctx, `UPDATE deliveries SET state='accepted' WHERE id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE tracking_objects SET expires_at=clock_timestamp()+interval '1 hour' WHERE delivery_id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	if err = directRetire(deliveryID, "operator", generation, false); err == nil {
		t.Fatal("database allowed retirement before expiry")
	}
	if _, err = owner.ExecContext(ctx, `UPDATE tracking_objects SET expires_at=clock_timestamp()-interval '1 second' WHERE delivery_id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err = retireDB.ExecContext(ctx, `UPDATE tracking_objects SET object_key=NULL,retired_at=clock_timestamp() WHERE delivery_id=$1`, deliveryID); err == nil {
		t.Fatal("direct retirement succeeded without audit context")
	}
	if err = directRetire(deliveryID, " ", generation, false); err == nil {
		t.Fatal("direct retirement accepted an invalid audit actor")
	}
	if err = directRetire(deliveryID, "operator", id(998), false); err == nil {
		t.Fatal("direct retirement ignored the generation fence")
	}
	if _, err = repo.RetireExpiredTrackingMappings(ctx, "operator", 10); err == nil {
		t.Fatal("runtime role retired protected object reference")
	}
	if _, err = owner.ExecContext(ctx, `CREATE FUNCTION fail_tracking_retirement_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='tracking.mapping.retired' THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_tracking_retirement_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_tracking_retirement_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err = maintenance.RetireExpiredTrackingMappings(ctx, "operator", 10); err == nil {
		t.Fatal("retirement succeeded without its audit")
	}
	if _, err = owner.ExecContext(ctx, `DROP TRIGGER fail_tracking_retirement_audit ON audit_events; DROP FUNCTION fail_tracking_retirement_audit()`); err != nil {
		t.Fatal(err)
	}
	var stillReferenced bool
	if err = owner.QueryRowContext(ctx, `SELECT object_key IS NOT NULL AND retired_at IS NULL FROM tracking_objects WHERE delivery_id=$1`, deliveryID).Scan(&stillReferenced); err != nil || !stillReferenced {
		t.Fatal("failed retirement changed reference", err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE runtime_gate SET mode='closed' WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if _, err = maintenance.RetireExpiredTrackingMappings(ctx, "operator", 10); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("retirement ignored closed gate", err)
	}
	if err = directRetire(deliveryID, "operator", generation, false); err == nil {
		t.Fatal("direct retirement ignored the closed gate")
	}
	if _, err = owner.ExecContext(ctx, `UPDATE runtime_gate SET mode='open' WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	retireResults := make(chan int, workers)
	retireErrors := make(chan error, workers)
	retireStart := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-retireStart
			n, retireErr := maintenance.RetireExpiredTrackingMappings(ctx, "operator", 10)
			retireResults <- n
			retireErrors <- retireErr
		}()
	}
	close(retireStart)
	wg.Wait()
	close(retireResults)
	close(retireErrors)
	retiredTotal := 0
	for n := range retireResults {
		retiredTotal += n
	}
	for retireErr := range retireErrors {
		if retireErr != nil {
			t.Fatal("concurrent retirement failed", retireErr)
		}
	}
	if retiredTotal != 1 {
		t.Fatal("retirement was not exactly once", retiredTotal)
	}
	if _, err = retireDB.ExecContext(ctx, `UPDATE tracking_objects SET object_key=$2,retired_at=NULL WHERE delivery_id=$1`, deliveryID, oldKey); err == nil {
		t.Fatal("retirement identity revived an already retired mapping")
	}
	var auditCount, retainedEvents, retainedHooks int
	if err = owner.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM audit_events WHERE app_id=$1 AND resource_id=$2 AND action='tracking.mapping.retired'),(SELECT count(*) FROM tracking_events WHERE delivery_id=$2),(SELECT count(*) FROM webhook_events WHERE delivery_id=$2)`, app, deliveryID).Scan(&auditCount, &retainedEvents, &retainedHooks); err != nil || auditCount != 1 || retainedEvents == 0 || retainedHooks == 0 {
		t.Fatal("retirement audit or history", auditCount, retainedEvents, retainedHooks, err)
	}
	// Verify the batch bound and SKIP LOCKED against two terminal deliveries.
	for i, deliveryState := range []string{"accepted", "failed"} {
		candidateID := id(600 + i)
		candidateKey, keyErr := tracking.NewMappingKey(candidateID, time.Now().Add(-48*time.Hour))
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		if _, err = owner.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ($1,$2,$3,$4,$5)`, candidateID, message, i+1, "terminal-final-"+candidateID, deliveryState); err != nil {
			t.Fatal(err)
		}
		if _, err = owner.ExecContext(ctx, `INSERT INTO tracking_objects(delivery_id,object_key,expires_at,event_limit) VALUES ($1,$2,clock_timestamp()-interval '1 hour',3)`, candidateID, candidateKey); err != nil {
			t.Fatal(err)
		}
	}
	if n, retireErr := maintenance.RetireExpiredTrackingMappings(ctx, "operator", 1); retireErr != nil || n != 1 {
		t.Fatal("retirement batch bound", n, retireErr)
	}
	var remainingID string
	if err = owner.QueryRowContext(ctx, `SELECT delivery_id FROM tracking_objects WHERE delivery_id IN ($1,$2) AND object_key IS NOT NULL`, id(600), id(601)).Scan(&remainingID); err != nil {
		t.Fatal(err)
	}
	retireLock, err := owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer retireLock.Rollback()
	if err = retireLock.QueryRowContext(ctx, `SELECT true FROM tracking_objects WHERE delivery_id=$1 FOR UPDATE`, remainingID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if n, retireErr := maintenance.RetireExpiredTrackingMappings(ctx, "operator", 1); retireErr != nil || n != 0 {
		t.Fatal("retirement failed to skip a locked mapping", n, retireErr)
	}
	if err = retireLock.Commit(); err != nil {
		t.Fatal(err)
	}
	if n, retireErr := maintenance.RetireExpiredTrackingMappings(ctx, "operator", 1); retireErr != nil || n != 1 {
		t.Fatal("retirement did not resume after lock release", n, retireErr)
	}
	directID := id(602)
	directKey, err := tracking.NewMappingKey(directID, time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ($1,$2,3,$3,'accepted')`, directID, message, "terminal-final-"+directID); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO tracking_objects(delivery_id,object_key,expires_at,event_limit) VALUES ($1,$2,clock_timestamp()-interval '1 hour',3)`, directID, directKey); err != nil {
		t.Fatal(err)
	}
	if err = directRetire(directID, "direct-operator", generation, true); err != nil {
		t.Fatal("audited direct retirement failed", err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id=$1 AND resource_id=$2 AND action='tracking.mapping.retired' AND actor_id='direct-operator' AND details->>'database_user'=$3`, app, directID, retireRole).Scan(&count); err != nil || count != 1 {
		t.Fatal("direct retirement lacked database-enforced audit", count, err)
	}
	deleted, err = repo.CollectUnreferenced(ctx, oldKey, horizon, func(context.Context, string, time.Time) (bool, error) { removed = true; return true, nil })
	if err != nil || !deleted || !removed {
		t.Fatal("retired object did not become collectable", deleted, err)
	}
	// A backwards clock change or accidental expiry extension must not revive
	// a capability once its ciphertext reference was retired.
	if _, err = owner.ExecContext(ctx, `UPDATE tracking_objects SET expires_at=clock_timestamp()+interval '1 hour' WHERE delivery_id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.LookupTrackingObject(ctx, refs[1].Hash, "click"); !errors.Is(err, tracking.ErrNotFound) {
		t.Fatal("retired mapping was revived", err)
	}
	t.Run("indefinite mappings and stateless redirect", func(t *testing.T) {
		const origin = "https://tracking.example.com"
		keys, keyErr := tracking.NewLinkKeys([]byte("0123456789abcdef0123456789abcdef"), nil)
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		html, mappings, rewriteErr := tracking.Rewrite(`<a href="https://example.net/forever?case=fixture">link</a>`, tracking.Options{Origin: origin, Links: keys, Clicks: true})
		if rewriteErr != nil || len(mappings) != 1 {
			t.Fatal("rewrite", rewriteErr)
		}
		indefiniteID := id(900)
		key, keyErr := tracking.NewMappingKey(indefiniteID, time.Now())
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		newRefs, putErr := protected.Put(ctx, key, mappings)
		if putErr != nil {
			t.Fatal(putErr)
		}
		if _, execErr := owner.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ($1,$2,99,'final/indefinite','accepted');`, indefiniteID, message); execErr != nil {
			t.Fatal(execErr)
		}
		if _, execErr := owner.ExecContext(ctx, `INSERT INTO tracking_objects(delivery_id,object_key,expires_at,event_limit) VALUES ($1,$2,NULL,3)`, indefiniteID, key); execErr != nil {
			t.Fatal(execErr)
		}
		ref := newRefs[0]
		if _, execErr := owner.ExecContext(ctx, `INSERT INTO tracking_links(id,token_hash,delivery_id,kind) VALUES ($1,$2,$3,'click')`, ref.ID, ref.Hash[:], indefiniteID); execErr != nil {
			t.Fatal(execErr)
		}
		if got, lookupErr := repo.LookupTrackingObject(ctx, ref.Hash, "click"); lookupErr != nil || got != key {
			t.Fatal("indefinite mapping did not resolve", lookupErr)
		}
		if visitErr := repo.RecordTrackingVisit(ctx, ref.Hash, "click", key, false); visitErr != nil {
			t.Fatal("null expiry rejected", visitErr)
		}
		if n, retireErr := maintenance.RetireExpiredTrackingMappings(ctx, "operator", 10); retireErr != nil || n != 0 {
			t.Fatal("indefinite mapping expired", n, retireErr)
		}
		if retireErr := directRetire(indefiniteID, "operator", generation, true); retireErr == nil {
			t.Fatal("expiry retirement accepted an indefinite mapping")
		}
		if _, execErr := owner.ExecContext(ctx, `DELETE FROM tracking_links WHERE delivery_id=$1`, indefiniteID); execErr != nil {
			t.Fatal(execErr)
		}
		if _, lookupErr := repo.LookupTrackingObject(ctx, ref.Hash, "click"); !errors.Is(lookupErr, tracking.ErrNotFound) {
			t.Fatal("fixture record still exists", lookupErr)
		}
		// Extract the complete issued URL rather than reconstructing it from
		// the mapping. The public router must need neither row nor ciphertext.
		_, after, found := strings.Cut(html, `href="`)
		link, _, _ := strings.Cut(after, `"`)
		if !found {
			t.Fatal("issued link missing")
		}
		h := &tracking.Handler{Origin: origin, Links: keys, Visitor: &tracking.Service{Repository: repo, Objects: protected}}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", link, nil))
		if w.Code != 302 || w.Header().Get("Location") != "https://example.net/forever?case=fixture" {
			t.Fatal("deleted record broke redirect", w.Code)
		}
	})
	enableHook, disableHook := true, false
	// Activation must enforce the limit even when independent replicas race.
	const candidates = 20
	type activation struct {
		id  string
		err error
	}
	activations := make(chan activation, candidates)
	activationStart := make(chan struct{})
	for i := 0; i < candidates; i++ {
		hookID := id(200 + i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-activationStart
			_, activateErr := repo.InsertWebhook(ctx, "operator", other, hookID, admin.WebhookInput{Endpoint: "https://example.net/events"}, []byte{1}, nil)
			activations <- activation{id: hookID, err: activateErr}
		}()
	}
	close(activationStart)
	wg.Wait()
	close(activations)
	var enabled, rejected int
	var activeID, rejectedID string
	for result := range activations {
		switch {
		case result.err == nil:
			enabled++
			activeID = result.id
		case errors.Is(result.err, admin.ErrConflict):
			rejected++
			rejectedID = result.id
		default:
			t.Fatal("unexpected subscription activation result", result.err)
		}
	}
	if enabled != maxActiveWebhooks-1 || rejected != candidates-enabled {
		t.Fatal("concurrent webhook fanout limit", enabled, rejected)
	}
	if _, err = repo.UpdateWebhook(ctx, "operator", other, activeID, admin.WebhookPatch{Active: &enableHook}, nil); err != nil {
		t.Fatal("re-enabling an active subscription at the limit failed", err)
	}
	if _, err = repo.UpdateWebhook(ctx, "operator", other, activeID, admin.WebhookPatch{Active: &disableHook}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.InsertWebhook(ctx, "operator", other, rejectedID, admin.WebhookInput{Endpoint: "https://example.net/events"}, []byte{1}, nil); err != nil {
		t.Fatal("subscription slot was not released", err)
	}
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM webhook_subscriptions WHERE app_id=$1 AND active`, other).Scan(&count); err != nil || count != maxActiveWebhooks {
		t.Fatal("active subscription count", count, err)
	}
	// Enable and disable acquire the app before the subscription, so racing
	// operator actions cannot deadlock on a reverse audit/FK lock order.
	raceCtx, raceCancel := context.WithTimeout(ctx, 5*time.Second)
	defer raceCancel()
	raceResults := make(chan error, candidates)
	for i := 0; i < candidates; i++ {
		enable := i%2 == 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			if enable {
				_, actionErr := repo.UpdateWebhook(raceCtx, "operator", other, activeID, admin.WebhookPatch{Active: &enableHook}, nil)
				raceResults <- actionErr
				return
			}
			_, actionErr := repo.UpdateWebhook(raceCtx, "operator", other, activeID, admin.WebhookPatch{Active: &disableHook}, nil)
			raceResults <- actionErr
		}()
	}
	wg.Wait()
	close(raceResults)
	for actionErr := range raceResults {
		if actionErr != nil && !errors.Is(actionErr, admin.ErrConflict) {
			t.Fatal("concurrent subscription action failed", actionErr)
		}
	}
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM webhook_subscriptions WHERE app_id=$1 AND active`, other).Scan(&count); err != nil || count > maxActiveWebhooks {
		t.Fatal("subscription race exceeded cap", count, err)
	}
}
