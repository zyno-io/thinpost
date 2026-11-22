package postgres

import (
	"crypto/sha256"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/submission"
)

func TestVerifySMTPCredential(t *testing.T) {
	f := newSMTPCommitFixture(t)
	app, active, err := f.repo.VerifySMTPCredential(f.ctx, f.cred, f.digest)
	if err != nil || app != f.app || !active {
		t.Fatal("valid", app, active, err)
	}
	for name, c := range map[string]struct {
		id     string
		digest [32]byte
	}{
		"wrong secret":   {f.cred, sha256.Sum256([]byte("nope"))},
		"unknown id":     {strings.Repeat("9", 32), f.digest},
		"malformed id":   {"x", f.digest},
		"the API key":    {f.key, f.digest},
		"empty id":       {"", f.digest},
		"zero digest":    {f.cred, [32]byte{}},
		"upper-cased id": {strings.ToUpper(f.cred), f.digest},
	} {
		if _, _, err = f.repo.VerifySMTPCredential(f.ctx, c.id, c.digest); !errors.Is(err, submission.ErrInvalidKey) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A paused app is reported, not hidden: the caller decides.
	f.exec(`UPDATE apps SET active=false WHERE id=$1`, f.app)
	if app, active, err = f.repo.VerifySMTPCredential(f.ctx, f.cred, f.digest); err != nil || app != f.app || active {
		t.Fatal("paused", app, active, err)
	}
	f.exec(`UPDATE smtp_credentials SET revoked_at=clock_timestamp() WHERE id=$1`, f.cred)
	if _, _, err = f.repo.VerifySMTPCredential(f.ctx, f.cred, f.digest); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("revoked", err)
	}
	// A closed gate is not an invalid credential.
	f.exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
	if _, _, err = f.repo.VerifySMTPCredential(f.ctx, f.cred, f.digest); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal("closed gate", err)
	}
}

func TestTouchSMTPCredentialIsRateLimited(t *testing.T) {
	f := newSMTPCommitFixture(t)
	used := func() (time.Time, bool) {
		var at *time.Time
		if err := f.repo.DB.QueryRowContext(f.ctx, `SELECT last_used_at FROM smtp_credentials WHERE id=$1`, f.cred).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if at == nil {
			return time.Time{}, false
		}
		return *at, true
	}
	if _, ok := used(); ok {
		t.Fatal("a new credential was already used")
	}
	if err := f.repo.TouchSMTPCredential(f.ctx, f.cred); err != nil {
		t.Fatal(err)
	}
	first, ok := used()
	if !ok {
		t.Fatal("not touched")
	}
	if err := f.repo.TouchSMTPCredential(f.ctx, f.cred); err != nil {
		t.Fatal(err)
	}
	if again, _ := used(); !again.Equal(first) {
		t.Fatal("touched twice within a minute")
	}
	f.exec(`UPDATE smtp_credentials SET last_used_at=clock_timestamp()-interval '61 seconds' WHERE id=$1`, f.cred)
	if err := f.repo.TouchSMTPCredential(f.ctx, f.cred); err != nil {
		t.Fatal(err)
	}
	if later, _ := used(); !later.After(first.Add(-time.Second)) || time.Since(later) > time.Minute {
		t.Fatal("not refreshed", later)
	}
	// A revoked credential is left alone, and a held row lock makes it give up.
	f.exec(`UPDATE smtp_credentials SET last_used_at=NULL WHERE id=$1`, f.cred)
	tx, err := f.repo.DB.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(f.ctx, `SELECT 1 FROM smtp_credentials WHERE id=$1 FOR UPDATE`, f.cred); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err = f.repo.TouchSMTPCredential(f.ctx, f.cred); err == nil || time.Since(start) > 3*time.Second {
		t.Fatal("touch waited on a locked row", err, time.Since(start))
	}
	_ = tx.Rollback()
	if _, ok := used(); ok {
		t.Fatal("a failed touch wrote")
	}
}

func TestSMTPPolicyReadsPolicyAndVerifiedDomains(t *testing.T) {
	f := newSMTPCommitFixture(t)
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'mail.example.com',true,now()),($1,'off.example.com',false,now())`, f.app)
	policy, verified, err := f.repo.SMTPPolicy(f.ctx, f.app, []string{"example.com", "off.example.com", "unknown.example.org"})
	sort.Strings(verified)
	if err != nil || policy != (rawmime.Policy{Mode: rawmime.FromOff, OnlyUnauthorized: true}) || len(verified) != 1 || verified[0] != "example.com" {
		t.Fatal(policy, verified, err)
	}
	f.exec(`UPDATE apps SET smtp_from_mode='domain',smtp_from_domain='mail.example.com',smtp_from_only_unauthorized=false WHERE id=$1`, f.app)
	policy, verified, err = f.repo.SMTPPolicy(f.ctx, f.app, nil)
	sort.Strings(verified)
	want := rawmime.Policy{Mode: rawmime.FromDomain, Domain: "mail.example.com"}
	if err != nil || policy != want || strings.Join(verified, ",") != "mail.example.com" {
		t.Fatal("target domains are looked up without being asked for", policy, verified, err)
	}
	// The app's tracking defaults ride along.
	f.exec(`UPDATE apps SET default_track_opens=true,default_track_clicks=false WHERE id=$1`, f.app)
	if policy, _, err = f.repo.SMTPPolicy(f.ctx, f.app, nil); err != nil || !policy.DefaultTrackOpens || policy.DefaultTrackClicks {
		t.Fatal("tracking defaults", policy, err)
	}
	if _, _, err = f.repo.SMTPPolicy(f.ctx, strings.Repeat("9", 32), nil); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("unknown app", err)
	}
	// Another app's domains are never visible.
	other := strings.Repeat("c", 32)
	f.exec(`INSERT INTO apps(id,name) VALUES ($1,'O')`, other)
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'theirs.example.net',true,now())`, other)
	if _, verified, err = f.repo.SMTPPolicy(f.ctx, f.app, []string{"theirs.example.net"}); err != nil || len(verified) != 1 || verified[0] != "mail.example.com" {
		t.Fatal("cross-app", verified, err)
	}
}

func TestSMTPReplaySecretIsWriteOnce(t *testing.T) {
	f := newSMTPCommitFixture(t)
	if _, found, err := f.repo.SMTPReplaySealed(f.ctx); err != nil || found {
		t.Fatal("empty", found, err)
	}
	first, second := []byte(strings.Repeat("a", 60)), []byte(strings.Repeat("b", 60))
	if err := f.repo.InsertSMTPReplaySealed(f.ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.InsertSMTPReplaySealed(f.ctx, second); err != nil {
		t.Fatal("a second insert must be a no-op, not an error", err)
	}
	got, found, err := f.repo.SMTPReplaySealed(f.ctx)
	if err != nil || !found || string(got) != string(first) {
		t.Fatal("the first value was replaced", found, err)
	}
	if err = f.repo.InsertSMTPReplaySealed(f.ctx, []byte("short")); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("short", err)
	}
	if n := f.count(`SELECT count(*) FROM smtp_replay_secret`); n != 1 {
		t.Fatal("rows", n)
	}
}
