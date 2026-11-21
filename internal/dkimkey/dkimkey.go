// Package dkimkey generates, seals and opens the DKIM keys Thinpost creates
// for each app and sender domain, and checks a published DKIM record against
// a key's public half. It depends on nothing else in the module, so both the
// operator API and the signing resolver can use it.
package dkimkey

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
)

// Bits is the size of every generated key.
const Bits = 2048

// SelectorPrefix starts every generated selector (thinpost1, thinpost2, ...).
const SelectorPrefix = "thinpost"

// Errors reported by key stores.
var (
	// ErrNoKey means the (app, domain) has no usable active key: the domain is
	// not verified or has no key yet.
	ErrNoKey = errors.New("no active DKIM key")
)

// Stored is a key row as the signing path reads it. Sealed is empty unless the
// private key was asked for.
type Stored struct {
	ID, AppID, Domain, Selector string
	PublicKey                   []byte
	Sealed                      []byte
}

// Sealer is objectstore.Keyring's Seal and Open.
type Sealer interface {
	Seal(aad, plaintext []byte) ([]byte, error)
	Open(aad, envelope []byte) ([]byte, error)
}

// Material is a freshly generated key: the PKCS#8 private key (to be sealed
// immediately) and the SPKI public key.
type Material struct {
	PKCS8, SPKI []byte
}

// Generate makes a new RSA-2048 key.
func Generate() (Material, error) {
	key, err := rsa.GenerateKey(rand.Reader, Bits)
	if err != nil {
		return Material{}, errors.New("cannot generate DKIM key")
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Material{}, errors.New("cannot encode DKIM key")
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return Material{}, errors.New("cannot encode DKIM key")
	}
	return Material{PKCS8: private, SPKI: public}, nil
}

// AAD binds a sealed private key to its row: a ciphertext copied to another
// row (a different id, app, domain or selector) fails to open.
func AAD(id, appID, domain, selector string) []byte {
	return []byte("thinpost-dkim-v1\x00" + id + "\x00" + appID + "\x00" + domain + "\x00" + selector)
}

// Seal encrypts a PKCS#8 private key for its row.
func Seal(s Sealer, id, appID, domain, selector string, pkcs8 []byte) ([]byte, error) {
	if s == nil {
		return nil, errors.New("DKIM keys need the object encryption keyring")
	}
	return s.Seal(AAD(id, appID, domain, selector), pkcs8)
}

// Open decrypts a stored key and checks that it is an RSA key whose public
// half is the row's public key.
func Open(s Sealer, k Stored) (*rsa.PrivateKey, error) {
	if s == nil || len(k.Sealed) == 0 {
		return nil, errors.New("signing key unavailable")
	}
	plain, err := s.Open(AAD(k.ID, k.AppID, k.Domain, k.Selector), k.Sealed)
	if err != nil {
		return nil, errors.New("signing key unavailable")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(plain)
	for i := range plain {
		plain[i] = 0
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if err != nil || !ok || key.Validate() != nil {
		return nil, errors.New("signing key unavailable")
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil || subtle.ConstantTimeCompare(public, k.PublicKey) != 1 {
		return nil, errors.New("signing key unavailable")
	}
	return key, nil
}

// RecordName is the DNS name of a selector's TXT record.
func RecordName(selector, domain string) string {
	return selector + "._domainkey." + domain
}

// RecordValue is the TXT value to publish for a public key.
func RecordValue(spki []byte) string {
	return "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(spki)
}

// ValidSelector reports a DNS-label selector.
func ValidSelector(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Reason is the result of checking a selector's TXT records against a key.
type Reason string

const (
	ReasonOK              Reason = "ok"
	ReasonNotFound        Reason = "not_found"
	ReasonMultipleRecords Reason = "multiple_records"
	ReasonDifferentKey    Reason = "different_key"
	ReasonTestingMode     Reason = "testing_mode"
	ReasonUnsupported     Reason = "unsupported"
)

// CheckRecord compares what DNS returns at <selector>._domainkey.<domain>
// (each record's strings already joined, as Go's resolver does) with a key's
// SPKI. Exactly one record must exist: resolvers differ on which of several
// they honor. The record must be a plain RSA/SHA-256 email key that is not in
// testing mode (t=y) and whose p= is the key. Whitespace inside p= and extra
// tags are accepted.
func CheckRecord(records []string, spki []byte) Reason {
	switch len(records) {
	case 0:
		return ReasonNotFound
	case 1:
	default:
		return ReasonMultipleRecords
	}
	record := records[0]
	if len(record) > 8192 {
		return ReasonUnsupported
	}
	tags := map[string]string{}
	first := ""
	for _, part := range strings.Split(record, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		// Tag names are case-sensitive (RFC 6376 section 3.2).
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if first == "" {
			first = name
		}
		if !ok || name == "" {
			return ReasonUnsupported
		}
		if _, exists := tags[name]; exists {
			return ReasonUnsupported
		}
		tags[name] = value
	}
	if version, exists := tags["v"]; exists && (version != "DKIM1" || first != "v") {
		return ReasonUnsupported
	}
	if kind := tags["k"]; kind != "" && kind != "rsa" {
		return ReasonUnsupported
	}
	if hashes := tags["h"]; hashes != "" && !hasValue(hashes, "sha256") {
		return ReasonUnsupported
	}
	if services := tags["s"]; services != "" && !hasValue(services, "email") && !hasValue(services, "*") {
		return ReasonUnsupported
	}
	encoded := strings.Map(func(c rune) rune {
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			return -1
		}
		return c
	}, tags["p"])
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(spki) == 0 || len(decoded) != len(spki) || subtle.ConstantTimeCompare(decoded, spki) != 1 {
		return ReasonDifferentKey
	}
	if hasValue(tags["t"], "y") {
		return ReasonTestingMode
	}
	return ReasonOK
}

func hasValue(values, want string) bool {
	for _, value := range strings.Split(values, ":") {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}
