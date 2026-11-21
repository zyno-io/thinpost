package signing

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/dkimkey"
	"github.com/zyno-io/thinpost/internal/objectstore"
)

const app = "0123456789abcdef0123456789abcdef"

func testRing(t *testing.T) *objectstore.Keyring {
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

// store is an in-memory dkim_keys: the active key per (app, domain).
type store struct {
	keys        map[string]dkimkey.Stored
	activeCalls int
	sealedCalls int
}

func (s *store) ActiveSigningKey(_ context.Context, appID, domain string) (dkimkey.Stored, error) {
	s.activeCalls++
	k, ok := s.keys[appID+"|"+domain]
	if !ok {
		return dkimkey.Stored{}, dkimkey.ErrNoKey
	}
	k.Sealed = nil
	return k, nil
}
func (s *store) SealedSigningKey(_ context.Context, id string) (dkimkey.Stored, error) {
	s.sealedCalls++
	for _, k := range s.keys {
		if k.ID == id {
			return k, nil
		}
	}
	return dkimkey.Stored{}, dkimkey.ErrNoKey
}

// add stores a freshly generated, sealed active key.
func (s *store) add(t *testing.T, ring *objectstore.Keyring, appID, domain, selector string) dkimkey.Stored {
	t.Helper()
	material, err := dkimkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("%032x", len(s.keys)+1)
	sealed, err := dkimkey.Seal(ring, id, appID, domain, selector, material.PKCS8)
	if err != nil {
		t.Fatal(err)
	}
	k := dkimkey.Stored{ID: id, AppID: appID, Domain: domain, Selector: selector, PublicKey: material.SPKI, Sealed: sealed}
	if s.keys == nil {
		s.keys = map[string]dkimkey.Stored{}
	}
	s.keys[appID+"|"+domain] = k
	return k
}

type dns struct {
	records []string
	name    string
	lookups int
}

func (d *dns) LookupTXT(_ context.Context, name string) ([]string, error) {
	d.name = name
	d.lookups++
	return d.records, nil
}

func TestSigningUsesTheActiveDatabaseKeyAndRequiresItsRecord(t *testing.T) {
	ring := testRing(t)
	st := &store{}
	key := st.add(t, ring, app, "example.com", "thinpost1")
	value := dkimkey.RecordValue(key.PublicKey)
	d := &dns{records: []string{value}}
	resolver, err := New(st, ring, d)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resolved, err := resolver.ForSender(ctx, app, "Test <sender@Example.com>")
	if err != nil || resolved.Domain != "example.com" || resolved.Selector != "thinpost1" || d.name != "thinpost1._domainkey.example.com" {
		t.Fatal("valid DKIM rejected", err)
	}
	// The signer is the stored key.
	public := resolved.Signer.Public().(*rsa.PublicKey)
	if public.N.BitLen() != dkimkey.Bits {
		t.Fatal("wrong signer")
	}
	// The opened key is kept: a second message does not open it again.
	if _, err = resolver.ForSender(ctx, app, "sender@example.com"); err != nil || st.sealedCalls != 1 {
		t.Fatal("signer not cached", err, st.sealedCalls)
	}
	// Another app, and a domain with no key, never sign.
	if _, err = resolver.ForSender(ctx, "abcdef0123456789abcdef0123456789", "sender@example.com"); err == nil {
		t.Fatal("cross-app key allowed")
	}
	if _, err = resolver.ForSender(ctx, app, "sender@nokey.example"); err == nil {
		t.Fatal("domain without a key signed")
	}
	// Every record that does not publish exactly this key refuses the signing.
	other, _ := dkimkey.Generate()
	for _, records := range [][]string{
		nil,
		{"v=DKIM1; k=rsa; p="},
		{value + "; t=y"},
		{value, value},
		{dkimkey.RecordValue(other.SPKI)},
		{"unrelated"},
	} {
		d.records = records
		if _, err = resolver.ForSender(ctx, app, "sender@example.com"); err == nil {
			t.Errorf("signed with records %v", records)
		}
	}
}

func TestRotationSwitchSignsWithTheNewKey(t *testing.T) {
	ring := testRing(t)
	st := &store{}
	oldKey := st.add(t, ring, app, "example.com", "thinpost1")
	d := &dns{records: []string{dkimkey.RecordValue(oldKey.PublicKey)}}
	resolver, _ := New(st, ring, d)
	first, err := resolver.ForSender(context.Background(), app, "a@example.com")
	if err != nil || first.Selector != "thinpost1" {
		t.Fatal(err)
	}
	newKey := st.add(t, ring, app, "example.com", "thinpost2") // the switch replaced the active row
	d.records = []string{dkimkey.RecordValue(newKey.PublicKey)}
	second, err := resolver.ForSender(context.Background(), app, "a@example.com")
	if err != nil || second.Selector != "thinpost2" || d.name != "thinpost2._domainkey.example.com" {
		t.Fatal("did not switch", err)
	}
	// The switched-out key's opened private key is dropped, not kept beside the new one.
	if len(resolver.signers) != 1 || resolver.signers[app+"|example.com"].id != newKey.ID || second.KeyID != newKey.ID {
		t.Fatal("old signer kept", len(resolver.signers))
	}
}

func TestSigningRefusesAKeyThatCannotBeOpened(t *testing.T) {
	ring := testRing(t)
	st := &store{}
	k := st.add(t, ring, app, "example.com", "thinpost1")
	d := &dns{records: []string{dkimkey.RecordValue(k.PublicKey)}}
	resolver, _ := New(st, ring, d)
	// Ciphertext moved to another row (here: another selector) fails to open.
	moved := st.keys[app+"|example.com"]
	moved.Selector = "thinpost9"
	st.keys[app+"|example.com"] = moved
	d.records = []string{dkimkey.RecordValue(k.PublicKey)}
	if _, err := resolver.ForSender(context.Background(), app, "a@example.com"); err == nil {
		t.Fatal("moved ciphertext signed")
	}
	// A shredded key has no private half.
	moved.Selector, moved.Sealed = "thinpost1", nil
	st.keys[app+"|example.com"] = moved
	if _, err := resolver.ForSender(context.Background(), app, "a@example.com"); err == nil {
		t.Fatal("shredded key signed")
	}
	if _, err := New(nil, ring, d); err == nil {
		t.Fatal("no store accepted")
	}
}

func TestReadyChecksLikeForSenderAndCaches(t *testing.T) {
	ring := testRing(t)
	st := &store{}
	k := st.add(t, ring, app, "example.com", "thinpost1")
	good := dkimkey.RecordValue(k.PublicKey)
	d := &dns{records: []string{good}}
	resolver, _ := New(st, ring, d)
	now := time.Unix(1000, 0)
	resolver.now = func() time.Time { return now }
	ctx := context.Background()

	// Success is cached for 60 seconds, then rechecked.
	if err := resolver.Ready(ctx, app, "Example.com"); err != nil || d.lookups != 1 {
		t.Fatal("ready", err, d.lookups)
	}
	if st.sealedCalls != 0 {
		t.Fatal("readiness opened the private key")
	}
	d.records = nil
	now = now.Add(59 * time.Second)
	if err := resolver.Ready(ctx, app, "example.com"); err != nil || d.lookups != 1 {
		t.Fatal("success not cached", err, d.lookups)
	}
	now = now.Add(2 * time.Second)
	if err := resolver.Ready(ctx, app, "example.com"); err == nil || d.lookups != 2 {
		t.Fatal("stale success served", err, d.lookups)
	}
	// Failure is cached for 10 seconds only.
	d.records = []string{good}
	now = now.Add(9 * time.Second)
	if err := resolver.Ready(ctx, app, "example.com"); err == nil || d.lookups != 2 {
		t.Fatal("failure not cached", err, d.lookups)
	}
	now = now.Add(2 * time.Second)
	if err := resolver.Ready(ctx, app, "example.com"); err != nil || d.lookups != 3 {
		t.Fatal("failure cached too long", err, d.lookups)
	}
	// Another app and an unknown domain are not ready and never reach DNS.
	lookups := d.lookups
	if resolver.Ready(ctx, "abcdef0123456789abcdef0123456789", "example.com") == nil || resolver.Ready(ctx, app, "other.example") == nil || d.lookups != lookups {
		t.Fatal("unknown pair ready or looked up")
	}
}

func TestCachesAreBounded(t *testing.T) {
	ring := testRing(t)
	st := &store{}
	d := &dns{}
	resolver, _ := New(st, ring, d)
	for i := 0; i < maxCacheEntries*2; i++ {
		_ = resolver.Ready(context.Background(), app, fmt.Sprintf("d%d.example", i))
	}
	if len(resolver.ready) > maxCacheEntries {
		t.Fatal("ready cache grew past its bound", len(resolver.ready))
	}
}

func TestCheckRecordIsExported(t *testing.T) {
	material, _ := dkimkey.Generate()
	if CheckRecord([]string{dkimkey.RecordValue(material.SPKI)}, material.SPKI) != ReasonOK || CheckRecord(nil, material.SPKI) != ReasonNotFound {
		t.Fatal("CheckRecord")
	}
}
