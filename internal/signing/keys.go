// Package signing resolves app-scoped DKIM keys and checks their DNS records.
// Key provisioning, DNS automation and runtime scheduling are separate.
package signing

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/message"
	"github.com/zyno-io/thinpost/internal/preparation"
)

type Domains interface {
	SigningDomainActive(context.Context, string, string) (bool, error)
}
type DNS interface {
	LookupTXT(context.Context, string) ([]string, error)
}
type Entry struct {
	AppID, Domain, Selector string
	Signer                  crypto.Signer
}
type Resolver struct {
	domains Domains
	dns     DNS
	keys    map[string]Entry
}

func New(domains Domains, dns DNS, entries []Entry) (*Resolver, error) {
	if domains == nil || dns == nil || len(entries) == 0 || len(entries) > 1024 {
		return nil, errors.New("invalid signing configuration")
	}
	r := &Resolver{domains: domains, dns: dns, keys: make(map[string]Entry, len(entries))}
	for _, e := range entries {
		domain, ok := admin.NormalizeDomain(e.Domain)
		if !ok || domain != e.Domain || !selector(e.Selector) || len(e.Selector)+len("._domainkey.")+len(e.Domain) > 253 || len(e.AppID) != 32 || e.Signer == nil {
			return nil, errors.New("invalid signing entry")
		}
		if _, err := hex.DecodeString(e.AppID); err != nil {
			return nil, errors.New("invalid signing entry")
		}
		public, ok := e.Signer.Public().(*rsa.PublicKey)
		if !ok || public == nil || public.N == nil || public.N.BitLen() < 2048 || public.N.BitLen() > message.MaxDKIMRSABits || public.E < 65537 || public.E%2 == 0 {
			return nil, errors.New("unsupported signing key")
		}
		id := e.AppID + "\x00" + e.Domain
		if _, exists := r.keys[id]; exists {
			return nil, errors.New("duplicate signing domain")
		}
		r.keys[id] = e
	}
	return r, nil
}
func selector(value string) bool {
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

// ForSender rechecks app/domain authorization and the currently published DKIM
// key each time. DNS lookup is not DNSSEC authenticated; network policy and
// DNS integrity are separate deployment requirements.
func (r *Resolver) ForSender(parent context.Context, appID, from string) (preparation.SigningKey, error) {
	address, err := mail.ParseAddress(from)
	if err != nil {
		return preparation.SigningKey{}, errors.New("invalid sender")
	}
	at := strings.LastIndexByte(address.Address, '@')
	if at < 1 || at == len(address.Address)-1 {
		return preparation.SigningKey{}, errors.New("invalid sender")
	}
	domain := strings.ToLower(address.Address[at+1:])
	entry, ok := r.keys[appID+"\x00"+domain]
	if !ok {
		return preparation.SigningKey{}, errors.New("sender signing key unavailable")
	}
	active, err := r.domains.SigningDomainActive(parent, appID, domain)
	if err != nil || !active {
		return preparation.SigningKey{}, errors.New("sender domain unavailable")
	}
	publicDER, err := x509.MarshalPKIXPublicKey(entry.Signer.Public())
	if err != nil {
		return preparation.SigningKey{}, errors.New("invalid signing key")
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	records, err := r.dns.LookupTXT(ctx, entry.Selector+"._domainkey."+domain)
	if err != nil || len(records) != 1 || !matches(records[0], publicDER) {
		return preparation.SigningKey{}, errors.New("DKIM public key not verified")
	}
	return preparation.SigningKey{Domain: domain, Selector: entry.Selector, Signer: entry.Signer}, nil
}

func matches(record string, expected []byte) bool {
	if len(record) > 8192 {
		return false
	}
	tags := map[string]string{}
	first := ""
	for _, part := range strings.Split(record, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if first == "" {
			first = name
		}
		if !ok || name == "" {
			return false
		}
		if _, exists := tags[name]; exists {
			return false
		}
		tags[name] = value
	}
	if version, exists := tags["v"]; exists && (version != "DKIM1" || first != "v") {
		return false
	}
	if kind := tags["k"]; kind != "" && kind != "rsa" {
		return false
	}
	if hashes := tags["h"]; hashes != "" && !hasValue(hashes, "sha256") {
		return false
	}
	if services := tags["s"]; services != "" && !hasValue(services, "email") && !hasValue(services, "*") {
		return false
	}
	if hasValue(tags["t"], "y") {
		return false
	}
	encoded := strings.Map(func(c rune) rune {
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			return -1
		}
		return c
	}, tags["p"])
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	return err == nil && len(expected) > 0 && len(decoded) == len(expected) && subtle.ConstantTimeCompare(decoded, expected) == 1
}
func hasValue(values, want string) bool {
	for _, value := range strings.Split(values, ":") {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}
