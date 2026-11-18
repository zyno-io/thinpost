package objectstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type memoryRaw struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (m *memoryRaw) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objects == nil {
		m.objects = make(map[string][]byte)
	}
	if _, ok := m.objects[key]; ok {
		return errors.New("object exists")
	}
	m.objects[key] = bytes.Clone(data)
	return nil
}

func (m *memoryRaw) Get(_ context.Context, key string, maximum int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	if !ok || int64(len(data)) > maximum {
		return nil, errors.New("missing or oversized object")
	}
	return bytes.Clone(data), nil
}

func (m *memoryRaw) VerifyPresent(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[key]; !ok {
		return errors.New("missing object")
	}
	return nil
}

func testRing(t *testing.T, active string, ids ...string) *Keyring {
	t.Helper()
	keys := make([]string, 0, len(ids))
	for i, id := range ids {
		key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{byte(i + 1)}, 32))
		keys = append(keys, fmt.Sprintf("%q:%q", id, key))
	}
	ring, err := parseKeyring([]byte(fmt.Sprintf(`{"active":%q,"keys":{%s}}`, active, strings.Join(keys, ","))))
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func TestEncryptedObjectRoundTripRotationAndTampering(t *testing.T) {
	ctx := context.Background()
	raw := &memoryRaw{}
	old, err := NewEncrypted(raw, testRing(t, "old", "old"))
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("private message content and recipient")
	if err = old.Put(ctx, "submissions/one", message); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw.objects["submissions/one"], message) {
		t.Fatal("raw object contains plaintext")
	}
	got, err := old.Get(ctx, "submissions/one", int64(len(message)))
	if err != nil || !bytes.Equal(got, message) {
		t.Fatalf("round trip failed: %v", err)
	}
	if _, err = old.Get(ctx, "submissions/one", int64(len(message)-1)); err == nil {
		t.Fatal("oversized plaintext accepted")
	}
	rotated, err := NewEncrypted(raw, testRing(t, "new", "old", "new"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err = rotated.Get(ctx, "submissions/one", int64(len(message))); err != nil || !bytes.Equal(got, message) {
		t.Fatal("retained key could not read an old object", err)
	}
	wrong, err := NewEncrypted(raw, testRing(t, "old", "different", "old"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wrong.Get(ctx, "submissions/one", int64(len(message))); err == nil {
		t.Fatal("different key material under the same version was accepted")
	}
	if err = rotated.PutMIME(ctx, "final/two", message); err != nil {
		t.Fatal(err)
	}
	if _, err = old.Get(ctx, "final/two", int64(len(message))); err == nil {
		t.Fatal("unknown new key version accepted")
	}
	if err = rotated.Put(ctx, "final/two", message); err == nil {
		t.Fatal("immutable object was overwritten")
	}
	if bytes.Equal(raw.objects["submissions/one"], raw.objects["final/two"]) {
		t.Fatal("identical plaintext produced identical objects")
	}
	original := bytes.Clone(raw.objects["submissions/one"])
	for i := range original {
		modified := bytes.Clone(original)
		modified[i] ^= 1
		raw.objects["submissions/one"] = modified
		if _, err := rotated.Get(ctx, "submissions/one", int64(len(message))); err == nil {
			t.Fatalf("tampering at byte %d was accepted", i)
		}
	}
	for length := range len(original) {
		raw.objects["submissions/one"] = bytes.Clone(original[:length])
		if _, err := rotated.Get(ctx, "submissions/one", int64(len(message))); err == nil {
			t.Fatalf("truncation at byte %d was accepted", length)
		}
	}
	raw.objects["submissions/one"] = original
	raw.objects["submissions/other"] = original
	if _, err = rotated.Get(ctx, "submissions/other", int64(len(message))); err == nil {
		t.Fatal("object substitution under a different key was accepted")
	}
	raw.objects["submissions/plaintext"] = message
	if _, err = rotated.Get(ctx, "submissions/plaintext", int64(len(message))); err == nil {
		t.Fatal("unencrypted object was accepted")
	}
	if err = rotated.Put(ctx, "", message); err == nil {
		t.Fatal("empty object key accepted")
	}
	if _, err = rotated.Get(ctx, "submissions/one", maxPlaintextBytes+1); err == nil {
		t.Fatal("unbounded object read accepted")
	}
}

func TestEncryptionKeyringValidation(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	other := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	valid := fmt.Sprintf(`{"active":"v1","keys":{"v1":%q,"v2":%q}}`, key, other)
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(path); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		`{}`,
		fmt.Sprintf(`{"active":"v1","active":"v2","keys":{"v1":%q}}`, key),
		fmt.Sprintf(`{"active":"v1","keys":{"v1":%q,"v1":%q}}`, key, other),
		fmt.Sprintf(`{"active":"v1","keys":{"v1":%q,"v2":%q}}`, key, key),
		fmt.Sprintf(`{"active":"missing","keys":{"v1":%q}}`, key),
		fmt.Sprintf(`{"active":"v1","keys":{"v1":%q},"unexpected":true}`, key),
		fmt.Sprintf(`{"active":"v1","keys":{"v1":%q}} {}`, key),
		`{"active":"v1","keys":{"v1":"c2hvcnQ="}}`,
		fmt.Sprintf(`{"active":"bad/id","keys":{"bad/id":%q}}`, key),
	} {
		if _, err := parseKeyring([]byte(value)); err == nil {
			t.Fatal("invalid keyring accepted")
		}
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, maxKeyringBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(path); err == nil {
		t.Fatal("oversized keyring accepted")
	}
}

func TestConcurrentEncryptionProducesIndependentObjects(t *testing.T) {
	raw := &memoryRaw{}
	store, err := NewEncrypted(raw, testRing(t, "v1", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 64 {
		group.Go(func() {
			errs <- store.Put(context.Background(), fmt.Sprintf("source/%d", i), []byte("same body"))
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]bool)
	for _, object := range raw.objects {
		start := len(envelopeMagic) + 1 + len("v1")
		salt := string(object[start : start+objectSaltSize])
		if seen[salt] {
			t.Fatal("duplicate object encryption salt")
		}
		seen[salt] = true
	}
	if len(seen) != 64 {
		t.Fatal("missing encrypted objects")
	}
}

func TestSealOpen(t *testing.T) {
	ring := testRing(t, "k1", "k1")
	aad := []byte("row-1")
	envelope, err := ring.Seal(aad, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(envelope, []byte("secret")) || !strings.HasPrefix(string(envelope), secretMagic) {
		t.Fatal("unexpected envelope")
	}
	got, err := ring.Open(aad, envelope)
	if err != nil || string(got) != "secret" {
		t.Fatalf("round trip: %q %v", got, err)
	}
	other, _ := ring.Seal(aad, []byte("secret"))
	if bytes.Equal(envelope, other) {
		t.Fatal("envelopes must be randomized")
	}
	if empty, err := ring.Seal(aad, nil); err != nil {
		t.Fatal(err)
	} else if got, err := ring.Open(aad, empty); err != nil || len(got) != 0 {
		t.Fatalf("empty round trip: %v", err)
	}
	if _, err := ring.Open([]byte("row-2"), envelope); err == nil {
		t.Fatal("wrong AAD opened")
	}
	headerLength := len(secretMagic) + 1 + len("k1") + objectSaltSize
	for _, index := range []int{0, len(secretMagic), len(secretMagic) + 1, len(secretMagic) + 3, headerLength - 1, headerLength, len(envelope) - 1} {
		tampered := bytes.Clone(envelope)
		tampered[index] ^= 1
		if _, err := ring.Open(aad, tampered); err == nil {
			t.Fatalf("tampered byte %d opened", index)
		}
	}
	for _, length := range []int{0, 1, len(secretMagic), len(secretMagic) + 1, headerLength, headerLength + 27, len(envelope) - 1} {
		if _, err := ring.Open(aad, envelope[:length]); err == nil {
			t.Fatalf("truncated to %d opened", length)
		}
	}
	if _, err := ring.Open(aad, append(bytes.Clone(envelope), 0)); err == nil {
		t.Fatal("extended envelope opened")
	}
}

func TestSealKeyRotationAndSeparation(t *testing.T) {
	old := testRing(t, "old", "old")
	envelope, err := old.Seal([]byte("a"), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	rotated := testRing(t, "new", "old", "new")
	if got, err := rotated.Open([]byte("a"), envelope); err != nil || string(got) != "secret" {
		t.Fatalf("old envelope: %q %v", got, err)
	}
	if _, err := testRing(t, "new", "new").Open([]byte("a"), envelope); err == nil {
		t.Fatal("unknown key ID opened")
	}
	// Same ID, different key material.
	if _, err := testRing(t, "old", "x", "old").Open([]byte("a"), envelope); err == nil {
		t.Fatal("wrong key opened")
	}

	ctx := context.Background()
	raw := &memoryRaw{}
	store, err := NewEncrypted(raw, old)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Put(ctx, "obj", []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err = old.Open([]byte("obj"), raw.objects["obj"]); err == nil {
		t.Fatal("object envelope opened as secret")
	}
	// An object envelope relabeled as a secret still fails: distinct HKDF info.
	relabeled := bytes.Clone(raw.objects["obj"])
	copy(relabeled, secretMagic)
	if _, err = old.Open([]byte("obj"), relabeled); err == nil {
		t.Fatal("relabeled object envelope opened as secret")
	}
	raw.objects["sealed"] = envelope
	if _, err = store.Get(ctx, "sealed", 1024); err == nil {
		t.Fatal("sealed secret read as object")
	}
}

func TestSealLimits(t *testing.T) {
	ring := testRing(t, "k1", "k1")
	if _, err := ring.Seal(nil, make([]byte, maxSecretPlaintext+1)); err == nil {
		t.Fatal("oversize plaintext sealed")
	}
	max, err := ring.Seal(nil, make([]byte, maxSecretPlaintext))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ring.Open(nil, max); err != nil || len(got) != maxSecretPlaintext {
		t.Fatalf("max plaintext: %v", err)
	}
	// Valid magic and key ID; rejected before any decryption work.
	oversize := append([]byte(secretMagic), 2, 'k', '1')
	oversize = append(oversize, make([]byte, objectSaltSize+maxSecretPlaintext+29)...)
	if _, err := ring.Open(nil, oversize); err == nil {
		t.Fatal("oversize envelope opened")
	}
	var nilRing *Keyring
	if _, err := nilRing.Seal(nil, []byte("x")); err == nil {
		t.Fatal("nil ring sealed")
	}
}

// TestSealEnvelopeConstruction pins the key derivation and associated data
// independently of Open, so a change to either is caught.
func TestSealEnvelopeConstruction(t *testing.T) {
	ring := testRing(t, "k1", "k1")
	aad := []byte("row")
	envelope, err := ring.Seal(aad, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	headerLength := len(secretMagic) + 1 + len("k1") + objectSaltSize
	header, body := envelope[:headerLength], envelope[headerLength:]
	salt := header[headerLength-objectSaltSize:]
	open := func(info string, ad []byte) error {
		aead, err := newAEAD(ring.keys["k1"], salt, info)
		if err != nil {
			t.Fatal(err)
		}
		_, err = aead.Open(nil, nil, body, ad)
		return err
	}
	if err := open("thinpost-secret-aes-gcm-v1", append(bytes.Clone(header), aad...)); err != nil {
		t.Fatal("envelope not sealed with secret info and header||aad")
	}
	if open("thinpost-object-aes-gcm-v1", append(bytes.Clone(header), aad...)) == nil {
		t.Fatal("secret key equals object key")
	}
	if open("thinpost-secret-aes-gcm-v1", aad) == nil {
		t.Fatal("header not bound into associated data")
	}
}
