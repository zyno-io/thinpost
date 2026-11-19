package format

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SecretPrefix distinguishes native signing secrets from API keys for secret
// scanners.
const SecretPrefix = "tpwh_"

// Outcomes lists every outcome a native webhook event can carry.
var Outcomes = []string{
	"accepted", "rejected", "route_rejected", "uncertain", "null_mx", "domain_not_found",
	"retry_expired", "recipient_policy_denied", "tracking_unavailable", "tracking_abandoned",
	"preparation_failed", "bounce_received_unverified", "opened", "clicked", "suppressed", "unsubscribed", "complained",
	"held", "released", "cancelled", "hold_expired",
}

const minThinpostSecret = 32

type thinpostFormat struct{}

func (thinpostFormat) EventTypes() []string { return Outcomes }

func (thinpostFormat) Classify(f Fact) (string, bool) {
	return f.Outcome, slices.Contains(Outcomes, f.Outcome)
}

// NewSecret returns a 48-byte signing secret. The receiver uses its exact
// bytes, prefix included, as the HMAC key.
func (thinpostFormat) NewSecret() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", errors.New("cannot generate webhook secret")
	}
	return SecretPrefix + base64.RawURLEncoding.EncodeToString(random), nil
}

func (thinpostFormat) Build(in Input) (Request, error) {
	if len(in.Secret) < minThinpostSecret || in.Now.IsZero() || !opaque(in.Event.ID) {
		return Request{}, errInvalidInput
	}
	var body []byte
	var err error
	if in.Test {
		body, err = json.Marshal(struct {
			ID         string    `json:"id"`
			Outcome    string    `json:"outcome"`
			OccurredAt time.Time `json:"occurred_at"`
		}{in.Event.ID, "test", in.Event.OccurredAt})
	} else {
		body, err = json.Marshal(in.Event)
	}
	if err != nil {
		return Request{}, err
	}
	timestamp := strconv.FormatInt(in.Now.Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Thinpost-Event-ID", in.Event.ID)
	h.Set("X-Thinpost-Timestamp", timestamp)
	h.Set("X-Thinpost-Signature", "v1="+thinpostMAC(in.Secret, timestamp, in.Event.ID, body))
	return Request{Body: body, Header: h}, nil
}

func thinpostMAC(secret []byte, timestamp, eventID string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp + "." + eventID + "."))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyThinpost checks freshness and authenticates a native request in
// constant time. A consumer must additionally deduplicate event IDs to prevent
// replay within the accepted time window.
func VerifyThinpost(secret []byte, eventID, timestamp string, payload []byte, header string, now time.Time, maxAge time.Duration) bool {
	if len(secret) < minThinpostSecret || !opaque(eventID) || timestamp == "" || !strings.HasPrefix(header, "v1=") || now.IsZero() || maxAge <= 0 {
		return false
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	signedAt := time.Unix(seconds, 0)
	if signedAt.After(now.Add(time.Minute)) || now.Sub(signedAt) > maxAge {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(header, "v1="))
	if err != nil {
		return false
	}
	expected, _ := hex.DecodeString(thinpostMAC(secret, timestamp, eventID, payload))
	return hmac.Equal(provided, expected)
}
