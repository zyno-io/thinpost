package webhook

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/objectstore"
)

func TestGenerateSecret(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		secret, err := GenerateSecret()
		if err != nil {
			t.Fatal(err)
		}
		if len(secret) != 48 || !strings.HasPrefix(secret, SecretPrefix) || seen[secret] {
			t.Fatalf("unexpected secret length %d", len(secret))
		}
		seen[secret] = true
	}
}

func TestSecretAAD(t *testing.T) {
	if got := string(SecretAAD("app", "id")); got != "thinpost-webhook-secret\x00app\x00id" {
		t.Fatalf("aad %q", got)
	}
	if bytes.Equal(SecretAAD("a", "bc"), SecretAAD("ab", "c")) || bytes.Equal(SecretAAD("a", "b"), SecretAAD("b", "a")) {
		t.Fatal("AAD not separated")
	}
}

// Sealed authentication and sealed secrets use different AAD, so neither
// ciphertext can be moved into the other column and opened.
func TestSealedAuthAndSecretAreNotInterchangeable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if err := os.WriteFile(path, []byte(`{"active":"k1","keys":{"k1":"`+key+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ring, err := objectstore.LoadKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	const app, hook = "app", "hook"
	if string(AuthAAD(app, hook)) == string(SecretAAD(app, hook)) {
		t.Fatal("AAD must differ")
	}
	auth := admin.NewWebhookAuth("user", "pass", nil)
	plain, _ := auth.Seal()
	sealedAuth, err := ring.Seal(AuthAAD(app, hook), plain)
	if err != nil {
		t.Fatal(err)
	}
	sealedSecret, err := ring.Seal(SecretAAD(app, hook), []byte("tpwh_secret"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ring.Open(AuthAAD(app, hook), sealedAuth); err != nil || string(got) != string(plain) {
		t.Fatal("auth does not open under its own AAD", err)
	}
	if got, err := ring.Open(SecretAAD(app, hook), sealedSecret); err != nil || string(got) != "tpwh_secret" {
		t.Fatal("secret does not open under its own AAD", err)
	}
	if _, err = ring.Open(SecretAAD(app, hook), sealedAuth); err == nil {
		t.Fatal("sealed auth opened as a secret")
	}
	if _, err = ring.Open(AuthAAD(app, hook), sealedSecret); err == nil {
		t.Fatal("sealed secret opened as auth")
	}
	// Bound to the endpoint too.
	if _, err = ring.Open(AuthAAD(app, "other"), sealedAuth); err == nil {
		t.Fatal("sealed auth opened for another endpoint")
	}
}
