package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"sort"
)

// FingerprintKeys derives the duplicate-detection keys from the bounce keyring
// the control plane already holds, one per key version, the active version
// first. The derivation is domain-separated from every other use of the bounce
// keys. Rotating the active key starts a new duplicate window for retries
// across the rotation only; older versions are still consulted.
func FingerprintKeys(active string, keys map[string][]byte) [][]byte {
	versions := make([]string, 0, len(keys))
	for version := range keys {
		if version != active {
			versions = append(versions, version)
		}
	}
	sort.Strings(versions)
	if _, ok := keys[active]; ok {
		versions = append([]string{active}, versions...)
	}
	out := make([][]byte, 0, len(versions))
	for _, version := range versions {
		mac := hmac.New(sha256.New, keys[version])
		_, _ = mac.Write([]byte("thinpost-inbound-fingerprint-key-v1"))
		out = append(out, mac.Sum(nil))
	}
	return out
}
