package tracking

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
)

// The host self-check lets control confirm that an HTTPS hostname reaches this
// deployment's tracking handler. It proves "this deployment" only: the MAC
// covers the challenge, not the Host header, because links are not bound to a
// hostname.
const (
	hostCheckPrefix    = "/t/h/v1/"
	hostCheckMACDomain = "thinpost-tracking-host-v1\x00"
	hostCheckBodyTag   = "thinpost-host-check v1 "
	hostChallengeBytes = 32
)

func isHostCheckPath(p string) bool { return strings.HasPrefix(p, hostCheckPrefix) }

// validHostChallenge accepts only the canonical unpadded base64url spelling of
// 32 bytes.
func validHostChallenge(challenge string) bool {
	if len(challenge) != base64.RawURLEncoding.EncodedLen(hostChallengeBytes) {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(challenge)
	return err == nil && len(raw) == hostChallengeBytes && base64.RawURLEncoding.EncodeToString(raw) == challenge
}

func hostCheckMAC(hostKey []byte, challenge string) string {
	mac := hmac.New(sha256.New, hostKey)
	mac.Write([]byte(hostCheckMACDomain + challenge))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// HostCheckProof returns the response body for a challenge:
// "thinpost-host-check v1 <key-id> <base64url(HMAC)>". The active key signs.
// Sizing keys and malformed challenges are refused.
func (k *LinkKeys) HostCheckProof(challenge string) (string, error) {
	if k == nil || k.sizing || k.hostKeys[k.active] == nil || !validHostChallenge(challenge) {
		return "", errors.New("host check unavailable")
	}
	return hostCheckBodyTag + k.active + " " + hostCheckMAC(k.hostKeys[k.active], challenge), nil
}

// VerifyHostCheck reports whether body is a valid proof of challenge under any
// retained key generation. It compares the MAC in constant time.
func (k *LinkKeys) VerifyHostCheck(challenge, body string) bool {
	if k == nil || k.sizing || !validHostChallenge(challenge) {
		return false
	}
	rest, ok := strings.CutPrefix(strings.TrimSpace(body), hostCheckBodyTag)
	if !ok {
		return false
	}
	keyID, mac, ok := strings.Cut(rest, " ")
	hostKey := k.hostKeys[keyID]
	if !ok || hostKey == nil {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(hostCheckMAC(hostKey, challenge)))
}

func (h *Handler) serveHostCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body, err := h.Links.HostCheckProof(strings.TrimPrefix(r.URL.Path, hostCheckPrefix))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(body))
	}
}
