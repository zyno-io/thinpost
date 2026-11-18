package tracking

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/objectstore"
)

type memoryEncryptedObjects struct{ objects map[string][]byte }

type protectedVisitor struct {
	store    *ProtectedStore
	key      string
	allowed  map[[32]byte]string
	recorded int
}

func (v *protectedVisitor) Visit(ctx context.Context, hash [32]byte, kind string, record bool) error {
	if v.allowed[hash] != kind {
		return ErrNotFound
	}
	if _, err := v.store.Lookup(ctx, v.key, hash, kind); err != nil {
		return err
	}
	if record {
		v.recorded++
	}
	return nil
}

func (m *memoryEncryptedObjects) Put(_ context.Context, key string, value []byte) error {
	if _, exists := m.objects[key]; exists {
		return errors.New("already exists")
	}
	m.objects[key] = bytes.Clone(value)
	return nil
}
func (m *memoryEncryptedObjects) Get(_ context.Context, key string, maximum int64) ([]byte, error) {
	value, exists := m.objects[key]
	if !exists || int64(len(value)) > maximum {
		return nil, errors.New("missing or oversized")
	}
	return bytes.Clone(value), nil
}
func (m *memoryEncryptedObjects) VerifyPresent(_ context.Context, key string) error {
	if _, exists := m.objects[key]; !exists {
		return errors.New("missing")
	}
	return nil
}

func protectedFixture(t *testing.T) (*ProtectedStore, *memoryEncryptedObjects) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "keyring.json")
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if err := os.WriteFile(file, []byte(fmt.Sprintf(`{"active":"fixture","keys":{"fixture":%q}}`, key)), 0600); err != nil {
		t.Fatal(err)
	}
	ring, err := objectstore.LoadKeyring(file)
	if err != nil {
		t.Fatal(err)
	}
	raw := &memoryEncryptedObjects{objects: make(map[string][]byte)}
	encrypted, err := objectstore.NewEncrypted(raw, ring)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewProtectedStore(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	return store, raw
}

func TestEncryptedMappingsDoNotStoreTokensOrPlaintextDestinations(t *testing.T) {
	ctx := context.Background()
	store, raw := protectedFixture(t)
	key, err := NewMappingKey("0123456789abcdef0123456789abcdef", time.Unix(1800000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	mappings := []Mapping{
		{Token: "000102030405060708090a0b0c0d0e0f", Kind: "open"},
		{Token: "101112131415161718191a1b1c1d1e1f", Kind: "click", Destination: "https://example.com/private?patient=example"},
	}
	refs, err := store.Put(ctx, key, mappings)
	if err != nil || len(refs) != 2 {
		t.Fatal("store mappings", err)
	}
	ciphertext := raw.objects[key]
	for _, private := range []string{mappings[0].Token, mappings[1].Token, mappings[1].Destination, "patient=example"} {
		if bytes.Contains(ciphertext, []byte(private)) {
			t.Fatal("private tracking data persisted in plaintext")
		}
	}
	open, err := store.Lookup(ctx, key, refs[0].Hash, "open")
	if err != nil || open != "" {
		t.Fatal("open lookup", err)
	}
	target, err := store.Lookup(ctx, key, refs[1].Hash, "click")
	if err != nil || target != mappings[1].Destination {
		t.Fatal("click lookup", err)
	}
	if _, err = store.Lookup(ctx, key, refs[1].Hash, "open"); !errors.Is(err, ErrNotFound) {
		t.Fatal("kind mismatch accepted", err)
	}
	if _, err = store.Lookup(ctx, key, [32]byte{1}, "click"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown capability accepted", err)
	}
	other, err := NewMappingKey("abcdef0123456789abcdef0123456789", time.Unix(1800000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	raw.objects[other] = bytes.Clone(ciphertext)
	if _, err = store.Lookup(ctx, other, refs[1].Hash, "click"); err == nil {
		t.Fatal("ciphertext swapped to another object key")
	}
	if _, err = store.Put(ctx, key, mappings); err == nil {
		t.Fatal("existing encrypted object overwritten")
	}
}

func TestProtectedMappingsRejectInvalidOrLeakingInputs(t *testing.T) {
	ctx := context.Background()
	store, raw := protectedFixture(t)
	key, err := NewMappingKey("0123456789abcdef0123456789abcdef", time.Unix(1800000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	const token = "000102030405060708090a0b0c0d0e0f"
	for _, mappings := range [][]Mapping{
		nil,
		{{Token: token, Kind: "open", Destination: "https://secret.example/"}},
		{{Token: token, Kind: "click", Destination: "https://user:secret@example.com/"}},
		{{Token: token, Kind: "open"}, {Token: token, Kind: "click", Destination: "https://example.com/"}},
		{{Token: token, Kind: "open"}, {Token: "101112131415161718191a1b1c1d1e1f", Kind: "open"}},
		{{Token: "not-a-capability", Kind: "open"}},
	} {
		_, err = store.Put(ctx, key, mappings)
		if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid mapping accepted or exposed")
		}
	}
	if len(raw.objects) != 0 {
		t.Fatal("invalid mappings were persisted")
	}
	if _, err = store.Put(ctx, "final/v1/1800000000/0123456789abcdef0123456789abcdef", []Mapping{{Token: token, Kind: "open"}}); err == nil {
		t.Fatal("accepted a different object namespace")
	}
	if _, err = NewMappingKey("not-an-id", time.Now()); err == nil {
		t.Fatal("accepted invalid delivery identifier")
	}
}

func TestProtectedLookupRejectsAmbiguousEncryptedJSON(t *testing.T) {
	ctx := context.Background()
	store, _ := protectedFixture(t)
	const digest = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	for i, body := range []string{
		`{"version":1,"version":1,"entries":[{"hash":"` + digest + `","kind":"open"}]}`,
		`{"version":1,"entries":[{"hash":"` + digest + `","kind":"click","destination":"https://safe.example","destination":"https://unsafe.example"}]}`,
		`{"version":1,"entries":[{"hash":"` + digest + `","kind":"open","extra":"x"}]}`,
		`{"version":1,"entries":[]} {}`,
	} {
		key, err := NewMappingKey(fmt.Sprintf("%032x", i+1), time.Unix(1800000000, 0))
		if err != nil {
			t.Fatal(err)
		}
		if err = store.objects.Put(ctx, key, []byte(body)); err != nil {
			t.Fatal(err)
		}
		if _, err = store.Lookup(ctx, key, [32]byte{}, "open"); err == nil || strings.Contains(err.Error(), "unsafe.example") {
			t.Fatal("ambiguous object accepted or leaked", err)
		}
	}
}

func TestRewrittenCapabilitiesResolveThroughEncryptedMappings(t *testing.T) {
	store, raw := protectedFixture(t)
	key, err := NewMappingKey("0123456789abcdef0123456789abcdef", time.Unix(1800000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	links := testLinkKeys(t)
	html, mappings, err := Rewrite(`<html><body><a href="https://example.net/private?patient=example">Open</a></body></html>`, Options{Links: links, Origin: "https://track.example.com", Opens: true, Clicks: true})
	if err != nil || len(mappings) != 2 {
		t.Fatal("rewrite", err)
	}
	refs, err := store.Put(context.Background(), key, mappings)
	if err != nil {
		t.Fatal(err)
	}
	visitor := &protectedVisitor{store: store, key: key, allowed: make(map[[32]byte]string)}
	for _, ref := range refs {
		visitor.allowed[ref.Hash] = ref.Kind
	}
	h := &Handler{Origin: "https://track.example.com", Links: links, Visitor: visitor}
	for _, mapping := range mappings {
		if strings.Contains(html, mapping.Token) || bytes.Contains(raw.objects[key], []byte(mapping.Token)) {
			t.Fatal("capability not confined to rewritten content")
		}
		path := regexp.MustCompile(`/t/` + mapping.Kind[:1] + `/v2/[A-Za-z0-9_/-]+`).FindString(html)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if mapping.Kind == "click" && (w.Code != 302 || w.Header().Get("Location") != mapping.Destination) {
			t.Fatal("committed click did not resolve", w.Code)
		}
		if mapping.Kind == "open" && w.Code != 200 {
			t.Fatal("committed open did not resolve", w.Code)
		}
	}
	if visitor.recorded != 2 {
		t.Fatal("expected both visits")
	}
}
