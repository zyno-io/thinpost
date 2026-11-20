package smtpintake

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"
	"sync"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/rawmime"
)

// ReplayKey supplies the deployment-wide secret that keys the replay digests.
type ReplayKey interface {
	Secret(ctx context.Context) ([]byte, error)
}

// replayAAD binds the sealed replay secret to its purpose, so a webhook secret
// or any other sealed value cannot be moved into its row.
const replayAAD = "thinpost-smtp-replay-secret-v1"

// replaySecretSize is the length of the random secret.
const replaySecretSize = 32

// Domain-separation labels of the two replay kinds. The kinds also have
// different key prefixes (acceptance.ReplayMessageIDPrefix and
// ReplayContentPrefix), so a digest of one kind can never match a row of the
// other.
const (
	labelMessageID = "thinpost-smtp-replay-v1\x00msgid\x00"
	labelContent   = "thinpost-smtp-replay-v1\x00content\x00"
)

// ReplayStore is the database side of the replay secret.
type ReplayStore interface {
	SMTPReplaySealed(ctx context.Context) ([]byte, bool, error)
	InsertSMTPReplaySealed(ctx context.Context, sealed []byte) error
}

// Sealer is objectstore.Keyring's Seal and Open.
type Sealer interface {
	Seal(aad, plaintext []byte) ([]byte, error)
	Open(aad, envelope []byte) ([]byte, error)
}

// ReplaySecret loads the replay secret from the database, creating it on first
// use. It is generated once per deployment, sealed under the object keyring
// version active at creation, written once (INSERT ... ON CONFLICT DO NOTHING)
// and never re-sealed: Open accepts any retained keyring version, so a replica
// whose keyring lacks that version fails closed instead of deriving another
// secret. Concurrent replicas converge on the stored value, which each reads
// back and opens. The opened secret is cached in memory; errors are not, so the
// next submission retries.
type ReplaySecret struct {
	Store ReplayStore
	Keys  Sealer

	mu     sync.Mutex
	secret []byte
}

// Secret returns the replay secret. The result must not be modified.
func (r *ReplaySecret) Secret(ctx context.Context) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.secret != nil {
		return r.secret, nil
	}
	if r.Store == nil || r.Keys == nil {
		return nil, errors.New("replay secret is not configured")
	}
	sealed, found, err := r.Store.SMTPReplaySealed(ctx)
	if err != nil {
		return nil, err
	}
	if !found {
		fresh := make([]byte, replaySecretSize)
		if _, err = rand.Read(fresh); err != nil {
			return nil, errors.New("cannot generate the replay secret")
		}
		if sealed, err = r.Keys.Seal([]byte(replayAAD), fresh); err != nil {
			return nil, errors.New("cannot seal the replay secret")
		}
		if err = r.Store.InsertSMTPReplaySealed(ctx, sealed); err != nil {
			return nil, err
		}
		// Read back: a concurrent replica's insert may have won.
		if sealed, found, err = r.Store.SMTPReplaySealed(ctx); err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("replay secret was not stored")
		}
	}
	secret, err := r.Keys.Open([]byte(replayAAD), sealed)
	if err != nil {
		return nil, errors.New("cannot open the replay secret")
	}
	if len(secret) != replaySecretSize {
		return nil, errors.New("invalid replay secret")
	}
	r.secret = secret
	return secret, nil
}

// MailFromKey is the key of a MAIL FROM address in the digest: the mailbox
// key (rawmime.MailboxKey), or "" for the null path.
func MailFromKey(mailFrom string) string {
	if mailFrom == "" || mailFrom == "<>" {
		return ""
	}
	if key, ok := rawmime.MailboxKey(mailFrom); ok {
		return key
	}
	return mailFrom
}

// RecipientKeys returns the sorted, de-duplicated mailbox keys of the envelope
// recipients, or false if one does not parse.
func RecipientKeys(recipients []string) ([]string, bool) {
	seen := make(map[string]struct{}, len(recipients))
	keys := make([]string, 0, len(recipients))
	for _, rcpt := range recipients {
		key, ok := rawmime.MailboxKey(rcpt)
		if !ok {
			return nil, false
		}
		if _, dup := seen[key]; !dup {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, true
}

func lp(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// Digest is the replay digest (plan section 4.5):
//
//	common  = lp(appID) || lp(mailFromKey) || uvarint(n) || lp(rcptKey)*n   (keys sorted)
//	kind M  = HMAC-SHA256(secret, "thinpost-smtp-replay-v1\x00msgid\x00"   || common || lp(messageID) || SHA-256(raw))
//	kind C  = HMAC-SHA256(secret, "thinpost-smtp-replay-v1\x00content\x00" || common || SHA-256(raw))
//
// where lp(x) is uvarint(len(x)) || x, mailFrom is as MailFromKey, and
// recipientKeys come from RecipientKeys. The messageID is used by kind M only.
func Digest(secret []byte, kind acceptance.ReplayKind, appID, mailFrom string, recipientKeys []string, messageID string, raw []byte) [32]byte {
	var buf []byte
	switch kind {
	case acceptance.ReplayMessageID:
		buf = append(buf, labelMessageID...)
	default:
		buf = append(buf, labelContent...)
	}
	buf = lp(buf, appID)
	buf = lp(buf, MailFromKey(mailFrom))
	buf = binary.AppendUvarint(buf, uint64(len(recipientKeys)))
	for _, key := range recipientKeys {
		buf = lp(buf, key)
	}
	if kind == acceptance.ReplayMessageID {
		buf = lp(buf, messageID)
	}
	sum := sha256.Sum256(raw)
	buf = append(buf, sum[:]...)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(buf)
	var digest [32]byte
	copy(digest[:], mac.Sum(nil))
	return digest
}

// ReplayKindOf picks the replay kind of a scanned message: M with a valid
// Message-ID, else C with a Date, else none.
func ReplayKindOf(s *rawmime.Scanned) acceptance.ReplayKind {
	switch {
	case s.HasMessageID:
		return acceptance.ReplayMessageID
	case s.HasDate:
		return acceptance.ReplayContent
	}
	return acceptance.ReplayNone
}

// replayKey is the idempotency key of a digest.
func replayKey(kind acceptance.ReplayKind, digest [32]byte) string {
	prefix := acceptance.ReplayContentPrefix
	if kind == acceptance.ReplayMessageID {
		prefix = acceptance.ReplayMessageIDPrefix
	}
	return prefix + hex.EncodeToString(digest[:])
}
