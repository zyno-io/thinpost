package webhook

import "github.com/zyno-io/thinpost/internal/webhook/format"

// SecretPrefix distinguishes signing secrets from API keys for secret scanners.
const SecretPrefix = format.SecretPrefix

// GenerateSecret returns a native 48-byte signing secret. The receiver uses its
// exact bytes, prefix included, as the HMAC key.
func GenerateSecret() (string, error) {
	native, _ := format.Lookup(format.Thinpost)
	return native.NewSecret()
}

// SecretAAD binds a sealed secret to its owning app and endpoint.
func SecretAAD(appID, id string) []byte {
	return []byte("thinpost-webhook-secret\x00" + appID + "\x00" + id)
}

// AuthAAD binds sealed HTTP authentication to its endpoint. It differs from
// SecretAAD, so a ciphertext cannot be moved between the two columns.
func AuthAAD(appID, id string) []byte {
	return []byte("thinpost-webhook-auth\x00" + appID + "\x00" + id)
}
