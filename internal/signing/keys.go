// Package signing resolves app-scoped DKIM keys and checks their DNS records.
// Keys are generated and sealed in the database (internal/dkimkey); this
// package opens them for signing and refuses to sign with a key whose record is
// not published right now.
package signing

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/zyno-io/thinpost/internal/dkimkey"
	"github.com/zyno-io/thinpost/internal/message"
	"github.com/zyno-io/thinpost/internal/preparation"
)

// Reason is the outcome of checking a DKIM TXT record against a key.
type Reason = dkimkey.Reason

// Check results; see CheckRecord.
const (
	ReasonOK              = dkimkey.ReasonOK
	ReasonNotFound        = dkimkey.ReasonNotFound
	ReasonMultipleRecords = dkimkey.ReasonMultipleRecords
	ReasonDifferentKey    = dkimkey.ReasonDifferentKey
	ReasonTestingMode     = dkimkey.ReasonTestingMode
	ReasonUnsupported     = dkimkey.ReasonUnsupported
)

// CheckRecord compares the TXT records found at <selector>._domainkey.<domain>
// with a key's SPKI public key. Exactly one record must exist, it must be a
// plain RSA/SHA-256 email key not in testing mode (t=y), and its p= must be the
// key; whitespace in p= and extra tags are accepted.
func CheckRecord(records []string, spki []byte) Reason { return dkimkey.CheckRecord(records, spki) }

// Store reads signing keys from the database.
type Store interface {
	// ActiveSigningKey returns the active key of a verified domain of an active
	// app, without its private key; dkimkey.ErrNoKey when there is none.
	ActiveSigningKey(ctx context.Context, appID, domain string) (dkimkey.Stored, error)
	// SealedSigningKey returns an active key with its sealed private key.
	SealedSigningKey(ctx context.Context, id string) (dkimkey.Stored, error)
}
type DNS interface {
	LookupTXT(context.Context, string) ([]string, error)
}

// Resolver finds the key that signs an app's mail from a domain.
type Resolver struct {
	store  Store
	sealer dkimkey.Sealer
	dns    DNS
	// now is replaced in tests.
	now func() time.Time

	mu      sync.Mutex
	signers map[string]openedKey // the opened active key by app and domain
	ready   map[string]readiness
}

// openedKey is a decrypted signing key. An entry is replaced when the domain's
// active key id changes, so a switched-out key's private key is dropped from
// memory the next time the domain signs.
type openedKey struct {
	id     string
	signer crypto.Signer
}

// readiness is a cached Ready result.
type readiness struct {
	ok      bool
	expires time.Time
}

// Ready results are cached per (app, domain): a success for 60 seconds, a
// failure for 10, so the SMTP tab neither hammers DNS nor stays red for long.
// Both caches are bounded because any app can ask about any domain name.
const (
	readyTTL        = 60 * time.Second
	readyFailTTL    = 10 * time.Second
	maxCacheEntries = 1024
	errNotReady     = "sender signing is not ready"
	errNoSigningKey = "sender signing key unavailable"
)

func New(store Store, sealer dkimkey.Sealer, dns DNS) (*Resolver, error) {
	if store == nil || sealer == nil || dns == nil {
		return nil, errors.New("invalid signing configuration")
	}
	return &Resolver{store: store, sealer: sealer, dns: dns, now: time.Now, signers: map[string]openedKey{}, ready: map[string]readiness{}}, nil
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
	key, signer, err := r.verify(parent, appID, domain, true)
	if err != nil {
		return preparation.SigningKey{}, err
	}
	return preparation.SigningKey{Domain: domain, Selector: key.Selector, KeyID: key.ID, Signer: signer}, nil
}

// verify is the check ForSender and Ready share: the app's active key for the
// domain (the database also requires an active app and a verified domain) and
// its published DKIM record. Only ForSender needs the private key.
func (r *Resolver) verify(parent context.Context, appID, domain string, sign bool) (dkimkey.Stored, crypto.Signer, error) {
	key, err := r.store.ActiveSigningKey(parent, appID, domain)
	if err != nil {
		if errors.Is(err, dkimkey.ErrNoKey) {
			// A removed domain never signs again, so the lazy replacement of a
			// cached key would never happen; drop its private key now.
			r.mu.Lock()
			delete(r.signers, appID+"|"+domain)
			r.mu.Unlock()
		}
		return dkimkey.Stored{}, nil, errors.New(errNoSigningKey)
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	records, err := r.dns.LookupTXT(ctx, dkimkey.RecordName(key.Selector, domain))
	if err != nil || CheckRecord(records, key.PublicKey) != ReasonOK {
		return dkimkey.Stored{}, nil, errors.New("DKIM public key not verified")
	}
	if !sign {
		return key, nil, nil
	}
	signer, err := r.signer(parent, key)
	if err != nil {
		return dkimkey.Stored{}, nil, err
	}
	return key, signer, nil
}

// signer opens a key once and keeps it for the (app, domain). Keys are
// immutable, so the entry is valid for as long as the active key id is the one
// cached; when the id changes the entry is replaced.
func (r *Resolver) signer(ctx context.Context, key dkimkey.Stored) (crypto.Signer, error) {
	slot := key.AppID + "|" + key.Domain
	r.mu.Lock()
	cached, ok := r.signers[slot]
	r.mu.Unlock()
	if ok && cached.id == key.ID {
		return cached.signer, nil
	}
	sealed, err := r.store.SealedSigningKey(ctx, key.ID)
	if err != nil {
		return nil, errors.New(errNoSigningKey)
	}
	private, err := dkimkey.Open(r.sealer, sealed)
	if err != nil {
		return nil, errors.New(errNoSigningKey)
	}
	if !supported(private) {
		return nil, errors.New("unsupported signing key")
	}
	// Public key must still be what the lookup returned (it cannot change).
	if der, derErr := x509.MarshalPKIXPublicKey(&private.PublicKey); derErr != nil || string(der) != string(key.PublicKey) {
		return nil, errors.New(errNoSigningKey)
	}
	r.mu.Lock()
	if _, exists := r.signers[slot]; !exists && len(r.signers) >= maxCacheEntries {
		for k := range r.signers {
			delete(r.signers, k)
			break
		}
	}
	r.signers[slot] = openedKey{id: key.ID, signer: private}
	r.mu.Unlock()
	return private, nil
}

func supported(key *rsa.PrivateKey) bool {
	public := &key.PublicKey
	return public.N != nil && public.N.BitLen() >= 2048 && public.N.BitLen() <= message.MaxDKIMRSABits && public.E >= 65537 && public.E%2 != 0
}

// Ready reports whether mail from the app's domain would be signed now: the
// same checks as ForSender (without opening the private key), cached per
// (app, domain) for 60 seconds on success and 10 on failure. Any failure is an
// error; callers treat it as misconfigured.
func (r *Resolver) Ready(ctx context.Context, appID, domain string) error {
	domain = strings.ToLower(domain)
	id := appID + "\x00" + domain
	r.mu.Lock()
	cached, hit := r.ready[id]
	r.mu.Unlock()
	if hit && r.now().Before(cached.expires) {
		if cached.ok {
			return nil
		}
		return errors.New(errNotReady)
	}
	_, _, err := r.verify(ctx, appID, domain, false)
	ttl := readyTTL
	if err != nil {
		ttl = readyFailTTL
	}
	now := r.now()
	r.mu.Lock()
	if _, exists := r.ready[id]; !exists && len(r.ready) >= maxCacheEntries {
		for k, v := range r.ready {
			if !now.Before(v.expires) {
				delete(r.ready, k)
			}
		}
		for k := range r.ready {
			if len(r.ready) < maxCacheEntries {
				break
			}
			delete(r.ready, k)
		}
	}
	r.ready[id] = readiness{ok: err == nil, expires: now.Add(ttl)}
	r.mu.Unlock()
	return err
}
