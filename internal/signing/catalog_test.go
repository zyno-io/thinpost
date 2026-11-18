package signing

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogLoadsMountedPrivateKeyAndRejectsInvalidInputs(t *testing.T) {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "private.pem")
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "catalog.json")
	data, err := json.Marshal([]catalogEntry{{AppID: "0123456789abcdef0123456789abcdef", Domain: "example.com", Selector: "mail1", KeyPath: keyPath}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := LoadCatalog(path)
	if err != nil || len(entries) != 1 {
		t.Fatal("valid catalog rejected", err)
	}
	if _, ok := entries[0].Signer.Public().(*rsa.PublicKey); !ok {
		t.Fatal("RSA key not loaded")
	}
	if err = os.WriteFile(path, []byte(`[{"key_path":"relative"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadCatalog(path); err == nil || strings.Contains(err.Error(), "relative") {
		t.Fatal("invalid path accepted or exposed")
	}
	if err = os.WriteFile(path, append(data, []byte(` {}`)...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadCatalog(path); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}
