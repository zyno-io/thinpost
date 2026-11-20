// Package smtpcred creates SMTP submission credentials. The ID doubles as the
// SMTP username. The plaintext secret must only be returned to an authorized
// administrator once and must never be logged.
package smtpcred

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// SecretPrefix starts every SMTP secret so that secret scanners can find a
// leaked one.
const SecretPrefix = "tps_"

// MaxActivePerApp is the number of unrevoked credentials one app may hold.
const MaxActivePerApp = 10

type Generated struct {
	ID     string
	Secret string
	Digest [32]byte
}

// Generate returns a 16-byte hex ID and a secret of "tps_" plus 32 random
// bytes in unpadded base64url (47 characters), with the secret's SHA-256.
func Generate() (Generated, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return Generated{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Generated{}, err
	}
	value := SecretPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	return Generated{ID: hex.EncodeToString(id[:]), Secret: value, Digest: sha256.Sum256([]byte(value))}, nil
}
