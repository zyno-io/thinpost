package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"strings"
)

// Fingerprints computes the keyed duplicate-detection digest of a message for
// one app, once per key (the first key is the active one). It covers the app,
// the envelope sender, the sorted recipients and the exact body, so a sender's
// retry of one transaction matches and nothing else does. Keys never leave
// control; the digest reveals nothing about the content.
func Fingerprints(keys [][]byte, appID string, e envelope, body []byte) [][32]byte {
	rcpts := make([]string, len(e.rcpts))
	for i, r := range e.rcpts {
		rcpts[i] = strings.ToLower(r)
	}
	sort.Strings(rcpts)
	out := make([][32]byte, len(keys))
	for i, key := range keys {
		mac := hmac.New(sha256.New, key)
		field := func(s string) {
			var n [binary.MaxVarintLen64]byte
			_, _ = mac.Write(n[:binary.PutUvarint(n[:], uint64(len(s)))])
			_, _ = mac.Write([]byte(s))
		}
		_, _ = mac.Write([]byte("thinpost-inbound-v1\x00"))
		field(appID)
		field(strings.ToLower(e.mailFrom))
		var n [binary.MaxVarintLen64]byte
		_, _ = mac.Write(n[:binary.PutUvarint(n[:], uint64(len(rcpts)))])
		for _, r := range rcpts {
			field(r)
		}
		_, _ = mac.Write(body)
		copy(out[i][:], mac.Sum(nil))
	}
	return out
}
