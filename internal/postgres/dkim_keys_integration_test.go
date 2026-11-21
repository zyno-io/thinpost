package postgres

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/dkimkey"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/preparation"
)

// testKeyring is a one-key object keyring for sealing DKIM keys.
func testKeyring(t *testing.T) *objectstore.Keyring {
	t.Helper()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := os.WriteFile(path, []byte(`{"active":"k1","keys":{"k1":"`+key+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ring, err := objectstore.LoadKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

type dkimFixture struct {
	t     *testing.T
	ctx   context.Context
	repo  *Repository
	exec  func(string, ...any)
	count func(string, ...any) int
}

func newDKIMFixture(t *testing.T) *dkimFixture {
	ctx, repo, exec, count := smtpOwnerFixture(t)
	repo.DKIMKeys = testKeyring(t)
	return &dkimFixture{t: t, ctx: ctx, repo: repo, exec: exec, count: count}
}

func (f *dkimFixture) app(id string) string {
	f.t.Helper()
	app := strings.Repeat(id, 32)
	f.exec(`INSERT INTO apps(id,name) VALUES ($1,$2)`, app, "app "+id)
	return app
}

// verified makes a domain active the way an install from before DKIM keys had it.
func (f *dkimFixture) verified(app, domain string) {
	f.t.Helper()
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,$2,true,now())`, app, domain)
}

// request starts a domain and returns its pending key's selector.
func (f *dkimFixture) request(app, domain string) string {
	f.t.Helper()
	challenge, err := f.repo.RequestDomain(f.ctx, "operator", app, domain)
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.TrimSuffix(challenge.DNSName, "._domainkey."+domain)
}

// activate verifies the pending key as the handler would after finding its record.
func (f *dkimFixture) activate(app, domain string, switchActive bool) admin.Domain {
	f.t.Helper()
	ref, err := f.repo.PendingDKIMKey(f.ctx, app, domain, "")
	if err != nil {
		f.t.Fatal(err)
	}
	d, err := f.repo.ActivateDKIMKey(f.ctx, "operator", app, domain, ref, switchActive)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func TestDKIMKeyFirstKeyVerifiesTheDomain(t *testing.T) {
	f := newDKIMFixture(t)
	app := f.app("a")
	if _, err := f.repo.RequestDomain(f.ctx, "operator", strings.Repeat("9", 32), "example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("unknown app", err)
	}
	if _, err := f.repo.CreateDKIMKey(f.ctx, "operator", app, "example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("a key for a domain that was never requested", err)
	}
	challenge, err := f.repo.RequestDomain(f.ctx, "operator", app, "example.com")
	if err != nil || challenge.Domain.Active || challenge.DNSName != "thinpost1._domainkey.example.com" || challenge.Domain.PendingKey == nil || challenge.Domain.ActiveKey != nil {
		t.Fatal("request", challenge, err)
	}
	// The record is the sealed key's public half; the private half is sealed to the row.
	var public, sealed []byte
	var id string
	if err = f.repo.DB.QueryRowContext(f.ctx, `SELECT id,public_key,sealed_private_key FROM dkim_keys WHERE app_id=$1`, app).Scan(&id, &public, &sealed); err != nil {
		t.Fatal(err)
	}
	if challenge.DNSValue != dkimkey.RecordValue(public) || len(sealed) < 64 {
		t.Fatal("record is not the stored key")
	}
	if _, err = dkimkey.Open(f.repo.DKIMKeys, dkimkey.Stored{ID: id, AppID: app, Domain: "example.com", Selector: "thinpost1", PublicKey: public, Sealed: sealed}); err != nil {
		t.Fatal("sealed key does not open", err)
	}
	// Asking again returns the same record without a new key.
	again, err := f.repo.RequestDomain(f.ctx, "operator", app, "example.com")
	if err != nil || again.DNSValue != challenge.DNSValue || f.count(`SELECT count(*) FROM dkim_keys`) != 1 {
		t.Fatal("asking again made a new key", err)
	}
	// Nothing signs before the record is verified.
	if _, err = f.repo.ActiveSigningKey(f.ctx, app, "example.com"); !errors.Is(err, dkimkey.ErrNoKey) {
		t.Fatal("pending key signs", err)
	}
	// A key other than the pending one does not activate.
	wrong := admin.DKIMKeyRef{Selector: "thinpost1", PublicKey: []byte(strings.Repeat("x", 270))}
	if _, err = f.repo.ActivateDKIMKey(f.ctx, "operator", app, "example.com", wrong, false); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("activated a key with another public key", err)
	}
	d := f.activate(app, "example.com", false)
	if !d.Active || d.VerifiedAt == nil || d.ActiveKey == nil || d.ActiveKey.Selector != "thinpost1" || d.PendingKey != nil || d.ActiveKey.ActivatedAt == nil {
		t.Fatal("domain not verified", d)
	}
	k, err := f.repo.ActiveSigningKey(f.ctx, app, "example.com")
	if err != nil || k.Selector != "thinpost1" || k.ID != id {
		t.Fatal("verified key does not sign", err)
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE app_id=$1 AND action IN ('domain.dkim_key.created','domain.verified','domain.dkim_key.activated')`, app) != 3 {
		t.Fatal("audit")
	}
	if _, err = f.repo.RequestDomain(f.ctx, "operator", app, "example.com"); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("a domain with an active key was asked for a first key again", err)
	}
	if _, err = f.repo.PendingDKIMKey(f.ctx, app, "example.com", ""); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("pending key reported when there is none", err)
	}
	if _, err = f.repo.PendingDKIMKey(f.ctx, app, "example.com", "thinpost1"); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("an active key reported as pending", err)
	}
	if _, err = f.repo.PendingDKIMKey(f.ctx, app, "example.com", "thinpost7"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("unknown selector", err)
	}
}

func TestDKIMKeyForDomainVerifiedBeforeKeysExisted(t *testing.T) {
	f := newDKIMFixture(t)
	app := f.app("a")
	f.verified(app, "old.example")
	domains, err := f.repo.ListDomains(f.ctx, app)
	if err != nil || len(domains) != 1 || !domains[0].Active || domains[0].ActiveKey != nil || domains[0].PendingKey != nil {
		t.Fatal("a domain with no key should list as active without keys", domains, err)
	}
	if _, err = f.repo.ActiveSigningKey(f.ctx, app, "old.example"); !errors.Is(err, dkimkey.ErrNoKey) {
		t.Fatal("domain without a key signs", err)
	}
	challenge, err := f.repo.CreateDKIMKey(f.ctx, "operator", app, "old.example")
	if err != nil || challenge.DNSName != "thinpost1._domainkey.old.example" {
		t.Fatal("create", err)
	}
	d := f.activate(app, "old.example", false)
	if d.ActiveKey == nil || d.VerifiedAt == nil {
		t.Fatal("not activated", d)
	}
	// Already verified: no second verification audit.
	if f.count(`SELECT count(*) FROM audit_events WHERE action='domain.verified'`) != 0 {
		t.Fatal("an already verified domain was audited as newly verified")
	}
	if _, err = f.repo.ActiveSigningKey(f.ctx, app, "old.example"); err != nil {
		t.Fatal(err)
	}
}

func TestDKIMSelectorsAreUniquePerDomainAcrossAppsAndNeverReused(t *testing.T) {
	f := newDKIMFixture(t)
	a, b, c := f.app("a"), f.app("b"), f.app("c")
	if got := f.request(a, "shared.example"); got != "thinpost1" {
		t.Fatal(got)
	}
	if got := f.request(b, "shared.example"); got != "thinpost2" {
		t.Fatal("second app on the same domain got", got)
	}
	// A different domain has its own numbering.
	if got := f.request(a, "other.example"); got != "thinpost1" {
		t.Fatal(got)
	}
	f.activate(a, "shared.example", false)
	f.activate(b, "shared.example", false)
	// Rotating takes the next unused number, and the retired selector is not recycled.
	if _, err := f.repo.CreateDKIMKey(f.ctx, "operator", a, "shared.example"); err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM dkim_keys WHERE domain='shared.example' AND selector='thinpost3' AND app_id=$1`, a) != 1 {
		t.Fatal("rotation selector")
	}
	f.activate(a, "shared.example", true)
	if got := f.request(c, "shared.example"); got != "thinpost4" {
		t.Fatal("a retired selector was reused", got)
	}
	// The database enforces it too.
	if _, err := f.repo.DB.ExecContext(f.ctx, `INSERT INTO dkim_keys(id,app_id,domain,selector,public_key,sealed_private_key,state) VALUES ($1,$2,'shared.example','thinpost1',decode(repeat('ab',270),'hex'),decode(repeat('cd',100),'hex'),'pending')`, strings.Repeat("e", 32), b); !isUniqueViolation(err) {
		t.Fatal("same selector twice on a domain", err)
	}
}

func TestDKIMConcurrentRequestsForOneDomainGetDistinctSelectors(t *testing.T) {
	f := newDKIMFixture(t)
	apps := []string{f.app("a"), f.app("b"), f.app("c"), f.app("d")}
	selectors := make([]string, len(apps))
	var wg sync.WaitGroup
	for i, app := range apps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			challenge, err := f.repo.RequestDomain(f.ctx, "operator", app, "busy.example")
			if err != nil {
				t.Error(err)
				return
			}
			selectors[i] = challenge.Domain.PendingKey.Selector
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, s := range selectors {
		if s == "" || seen[s] {
			t.Fatal("selectors collided", selectors)
		}
		seen[s] = true
	}
}

func TestDKIMRotationSwitchIsAtomicAndShredsTheOldKey(t *testing.T) {
	f := newDKIMFixture(t)
	app := f.app("a")
	f.request(app, "example.com")
	f.activate(app, "example.com", false)
	var oldID string
	if err := f.repo.DB.QueryRowContext(f.ctx, `SELECT id FROM dkim_keys WHERE state='active'`).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.CreateDKIMKey(f.ctx, "operator", app, "example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.CreateDKIMKey(f.ctx, "operator", app, "example.com"); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("two pending keys", err)
	}
	// Seeing the new record only marks the key ready; the old key keeps signing.
	d := f.activate(app, "example.com", false)
	if d.ActiveKey.Selector != "thinpost1" || d.PendingKey == nil || d.PendingKey.PublishedAt == nil {
		t.Fatal("verify during a rotation changed the active key", d)
	}
	// A failure in the middle of the switch leaves the old key active and intact.
	f.exec(`CREATE FUNCTION fail_activation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$;
		CREATE TRIGGER fail_activation BEFORE UPDATE ON dkim_keys FOR EACH ROW WHEN (OLD.state='pending' AND NEW.state='active') EXECUTE FUNCTION fail_activation()`)
	ref, err := f.repo.PendingDKIMKey(f.ctx, app, "example.com", "thinpost2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.ActivateDKIMKey(f.ctx, "operator", app, "example.com", ref, true); err == nil {
		t.Fatal("switch succeeded with the activation failing")
	}
	if f.count(`SELECT count(*) FROM dkim_keys WHERE id=$1 AND state='active' AND sealed_private_key IS NOT NULL`, oldID) != 1 || f.count(`SELECT count(*) FROM dkim_keys WHERE state='active'`) != 1 {
		t.Fatal("a failed switch left the old key retired or shredded")
	}
	f.exec(`DROP TRIGGER fail_activation ON dkim_keys; DROP FUNCTION fail_activation()`)
	// The retry policy decides how long the old record stays: max age + 2 days.
	f.exec(`UPDATE delivery_retry_policy SET max_age_ms=2592000000 WHERE singleton`)
	before := time.Now()
	d, err = f.repo.ActivateDKIMKey(f.ctx, "operator", app, "example.com", ref, true)
	if err != nil || d.ActiveKey == nil || d.ActiveKey.Selector != "thinpost2" || d.PendingKey != nil || d.RetiredKey == nil || d.RetiredKey.Selector != "thinpost1" {
		t.Fatal("switch", d, err)
	}
	wantKeep := before.Add(32 * 24 * time.Hour)
	if d.RetiredKey.KeepRecordUntil.Before(wantKeep.Add(-time.Minute)) || d.RetiredKey.KeepRecordUntil.After(wantKeep.Add(time.Minute)) {
		t.Fatal("keep_record_until does not follow the retry policy", d.RetiredKey.KeepRecordUntil, wantKeep)
	}
	if f.count(`SELECT count(*) FROM dkim_keys WHERE id=$1 AND state='retired' AND sealed_private_key IS NULL AND retired_at IS NOT NULL`, oldID) != 1 {
		t.Fatal("old key not retired and shredded")
	}
	if _, err = f.repo.SealedSigningKey(f.ctx, oldID); !errors.Is(err, dkimkey.ErrNoKey) {
		t.Fatal("retired key can still be fetched for signing", err)
	}
	k, err := f.repo.ActiveSigningKey(f.ctx, app, "example.com")
	if err != nil || k.Selector != "thinpost2" {
		t.Fatal("new key does not sign", err)
	}
	// The default policy (5 days) keeps the record 7 days, the floor.
	if _, err = f.repo.CreateDKIMKey(f.ctx, "operator", app, "example.com"); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE delivery_retry_policy SET max_age_ms=432000000 WHERE singleton`)
	before = time.Now()
	f.activate(app, "example.com", true)
	var keep time.Time
	if err = f.repo.DB.QueryRowContext(f.ctx, `SELECT keep_record_until FROM dkim_keys WHERE selector='thinpost2'`).Scan(&keep); err != nil {
		t.Fatal(err)
	}
	if keep.Before(before.Add(7*24*time.Hour-time.Minute)) || keep.After(before.Add(7*24*time.Hour+time.Minute)) {
		t.Fatal("default keep period", keep.Sub(before))
	}
}

func TestDKIMCancelOnlyRemovesAPendingKey(t *testing.T) {
	f := newDKIMFixture(t)
	app := f.app("a")
	f.request(app, "example.com")
	f.activate(app, "example.com", false)
	if _, err := f.repo.CreateDKIMKey(f.ctx, "operator", app, "example.com"); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.CancelDKIMKey(f.ctx, "operator", app, "example.com", "thinpost1"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("cancelled the active key", err)
	}
	if err := f.repo.CancelDKIMKey(f.ctx, "operator", strings.Repeat("b", 32), "example.com", "thinpost2"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("cancelled another app's key", err)
	}
	if err := f.repo.CancelDKIMKey(f.ctx, "operator", app, "example.com", "thinpost2"); err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM dkim_keys WHERE state='pending'`) != 0 || f.count(`SELECT count(*) FROM dkim_keys WHERE state='active'`) != 1 {
		t.Fatal("cancel removed the wrong key")
	}
	// A new rotation can start again.
	if _, err := f.repo.CreateDKIMKey(f.ctx, "operator", app, "example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestDKIMTableInvariants(t *testing.T) {
	f := newDKIMFixture(t)
	app := f.app("a")
	f.request(app, "example.com")
	f.activate(app, "example.com", false)
	insert := func(id, selector, state string) error {
		_, err := f.repo.DB.ExecContext(f.ctx, `INSERT INTO dkim_keys(id,app_id,domain,selector,public_key,sealed_private_key,state,activated_at)
			VALUES ($1,$2,'example.com',$3,decode(repeat('ab',270),'hex'),decode(repeat('cd',100),'hex'),$4,CASE WHEN $4='active' THEN now() END)`, id, app, selector, state)
		return err
	}
	// One active key per app and domain.
	if err := insert(strings.Repeat("1", 32), "thinpost8", "active"); err == nil {
		t.Fatal("a second active key")
	}
	if err := insert(strings.Repeat("2", 32), "thinpost8", "pending"); err != nil {
		t.Fatal(err)
	}
	// One pending key per app and domain.
	if err := insert(strings.Repeat("3", 32), "thinpost9", "pending"); err == nil {
		t.Fatal("a second pending key")
	}
	// A retired key has no private key, and only a retired key lacks one.
	if _, err := f.repo.DB.ExecContext(f.ctx, `UPDATE dkim_keys SET state='retired',retired_at=now(),keep_record_until=now() WHERE state='pending'`); err == nil {
		t.Fatal("retired without shredding")
	}
	if _, err := f.repo.DB.ExecContext(f.ctx, `UPDATE dkim_keys SET sealed_private_key=NULL WHERE state='active'`); err == nil {
		t.Fatal("an active key lost its private key")
	}
	if _, err := f.repo.DB.ExecContext(f.ctx, `UPDATE dkim_keys SET state='retired',sealed_private_key=NULL WHERE state='pending'`); err == nil {
		t.Fatal("retired without a keep date")
	}
	if _, err := f.repo.DB.ExecContext(f.ctx, `UPDATE dkim_keys SET state='retired',sealed_private_key=NULL,activated_at=now(),retired_at=now(),keep_record_until=now() WHERE state='pending'`); err != nil {
		t.Fatal("a properly shredded key could not be retired", err)
	}
	// An active key needs its activation time; the domain must exist.
	if _, err := f.repo.DB.ExecContext(f.ctx, `UPDATE dkim_keys SET activated_at=NULL WHERE state='active'`); err == nil {
		t.Fatal("active without activated_at")
	}
	if _, err := f.repo.DB.ExecContext(f.ctx, `INSERT INTO dkim_keys(id,app_id,domain,selector,public_key,sealed_private_key,state) VALUES ($1,$2,'unknown.example','thinpost1',decode(repeat('ab',270),'hex'),decode(repeat('cd',100),'hex'),'pending')`, strings.Repeat("4", 32), app); err == nil {
		t.Fatal("a key for a domain the app does not have")
	}
}

func TestDKIMSealedKeyIsBoundToItsRow(t *testing.T) {
	f := newDKIMFixture(t)
	a, b := f.app("a"), f.app("b")
	f.request(a, "one.example")
	f.activate(a, "one.example", false)
	f.request(b, "two.example")
	f.activate(b, "two.example", false)
	ka, err := f.repo.ActiveSigningKey(f.ctx, a, "one.example")
	if err != nil {
		t.Fatal(err)
	}
	kb, err := f.repo.ActiveSigningKey(f.ctx, b, "two.example")
	if err != nil {
		t.Fatal(err)
	}
	sealedA, err := f.repo.SealedSigningKey(f.ctx, ka.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dkimkey.Open(f.repo.DKIMKeys, sealedA); err != nil {
		t.Fatal("own key does not open", err)
	}
	// Copy A's sealed bytes (and public key) onto B's row: it must not open.
	f.exec(`UPDATE dkim_keys SET sealed_private_key=$1,public_key=$2 WHERE id=$3`, sealedA.Sealed, sealedA.PublicKey, kb.ID)
	moved, err := f.repo.SealedSigningKey(f.ctx, kb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dkimkey.Open(f.repo.DKIMKeys, moved); err == nil {
		t.Fatal("ciphertext moved to another row opened")
	}
}

func TestDKIMKeysNeedTheKeyring(t *testing.T) {
	f := newDKIMFixture(t)
	app := f.app("a")
	f.repo.DKIMKeys = nil
	if _, err := f.repo.RequestDomain(f.ctx, "operator", app, "example.com"); err == nil {
		t.Fatal("a key was made without a keyring")
	}
	if f.count(`SELECT count(*) FROM sender_domains`) != 0 || f.count(`SELECT count(*) FROM dkim_keys`) != 0 {
		t.Fatal("a failed request left rows behind")
	}
}

func TestListDomainsShowsKeyStatus(t *testing.T) {
	f := newDKIMFixture(t)
	app := f.app("a")
	f.request(app, "active.example")
	f.activate(app, "active.example", false)
	f.request(app, "pending.example")
	f.verified(app, "nokey.example")
	domains, err := f.repo.ListDomains(f.ctx, app)
	if err != nil || len(domains) != 3 {
		t.Fatal(domains, err)
	}
	byName := map[string]admin.Domain{}
	for _, d := range domains {
		byName[d.Name] = d
	}
	if d := byName["active.example"]; d.ActiveKey == nil || d.PendingKey != nil || d.ActiveKey.DNSName != "thinpost1._domainkey.active.example" {
		t.Fatal("active", d)
	}
	if d := byName["pending.example"]; d.Active || d.ActiveKey != nil || d.PendingKey == nil || d.PendingKey.PublishedAt != nil {
		t.Fatal("pending", d)
	}
	if d := byName["nokey.example"]; !d.Active || d.ActiveKey != nil || d.PendingKey != nil {
		t.Fatal("nokey", d)
	}
}

// A message signed with a key that a rotation switched out meanwhile must not
// become visible: its commit fails (retryable) and succeeds with the new key.
func TestPreparationCommitRequiresTheSigningKeyToBeActive(t *testing.T) {
	f := newDKIMFixture(t)
	app := f.app("a")
	f.request(app, "example.com")
	f.activate(app, "example.com", false)
	oldKey, err := f.repo.ActiveSigningKey(f.ctx, app, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	message := strings.Repeat("7", 32)
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,sender_domain,sender_domain_rev,subject,recipient_count) VALUES ($1,$2,'submissions/rot',true,'example.com','com.example.','s',1)`, message, app)
	f.repo.Objects = objectVerifierFunc(func(context.Context, string) error { return nil })
	claim, err := f.repo.ClaimPreparation(f.ctx, strings.Repeat("8", 32), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claim.SigningDomain, claim.SigningKeyID = "example.com", oldKey.ID
	// Rotation completes while the message is being prepared.
	if _, err = f.repo.CreateDKIMKey(f.ctx, "operator", app, "example.com"); err != nil {
		t.Fatal(err)
	}
	f.activate(app, "example.com", true)
	id := strings.Repeat("9", 32)
	deliveries := []preparation.Delivery{{ID: id, RecipientIndex: 0, ObjectKey: fixtureObjectKey("final", id)}}
	err = f.repo.CommitPreparation(f.ctx, claim, deliveries)
	if err == nil || errors.Is(err, preparation.ErrLostLease) {
		t.Fatal("a message signed with a retired key was committed, or reported a lost lease instead of a retryable failure", err)
	}
	if f.count(`SELECT count(*) FROM messages WHERE id=$1 AND state='preparing'`, message) != 1 || f.count(`SELECT count(*) FROM deliveries`) != 0 {
		t.Fatal("the failed commit changed the message")
	}
	newKey, err := f.repo.ActiveSigningKey(f.ctx, app, "example.com")
	if err != nil || newKey.ID == oldKey.ID {
		t.Fatal(err)
	}
	claim.SigningKeyID = newKey.ID
	if err = f.repo.CommitPreparation(f.ctx, claim, deliveries); err != nil {
		t.Fatal("commit with the active key failed", err)
	}
}
