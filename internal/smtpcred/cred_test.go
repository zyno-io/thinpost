package smtpcred

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	a, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if a.Secret == b.Secret || a.ID == b.ID {
		t.Fatal("generated credentials are not distinct")
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(a.ID) {
		t.Fatal("the ID must be 16 bytes of hex")
	}
	if len(a.Secret) != 47 || !regexp.MustCompile(`^tps_[A-Za-z0-9_-]{43}$`).MatchString(a.Secret) {
		t.Fatal("unexpected secret shape", len(a.Secret))
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(a.Secret, SecretPrefix))
	if err != nil || len(decoded) != 32 {
		t.Fatal("secret must contain 256 random bits")
	}
	if sha256.Sum256([]byte(a.Secret)) != a.Digest {
		t.Fatal("incorrect digest")
	}
}
