// Package apikey creates application credentials. Plaintext keys must only be
// returned to an authorized administrator once and must never be logged.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

type Generated struct {
	ID     string
	Secret string
	Digest [32]byte
}

func Generate() (Generated, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return Generated{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Generated{}, err
	}
	value := "tp_" + base64.RawURLEncoding.EncodeToString(secret[:])
	return Generated{ID: hex.EncodeToString(id[:]), Secret: value, Digest: sha256.Sum256([]byte(value))}, nil
}
