package apikey

import (
	"crypto/sha256"
	"encoding/base64"
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
	if a.Secret == b.Secret || a.ID == b.ID || !strings.HasPrefix(a.Secret, "tp_") {
		t.Fatal("generated keys are not distinct")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(a.Secret, "tp_"))
	if err != nil || len(decoded) != 32 {
		t.Fatal("key must contain 256 random bits")
	}
	if sha256.Sum256([]byte(a.Secret)) != a.Digest {
		t.Fatal("incorrect lookup digest")
	}
}
