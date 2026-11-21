package tracking

import (
	"bytes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	maxLinkKeysBytes   = 1 << 20
	maxPreviousSeeds   = 4096
	maxLinkPathBytes   = 4096
	linkDomain         = "thinpost-tracking-v3"
	linkKDFInfo        = "thinpost-tracking-v2 aead"
	hostKDFInfo        = "thinpost-tracking-v2 host check"
	linkKeyIDDomain    = "thinpost-tracking-v2 key id\x00"
	linkTokenBytes     = 16
	linkPadBlock       = 64
	linkHeaderBytes    = linkTokenBytes + 2 // token, big-endian destination length
	maxLinkPlaintext   = (linkHeaderBytes + maxTargetBytes + linkPadBlock - 1) / linkPadBlock * linkPadBlock
	maxLinkPayloadSize = chacha20poly1305.NonceSizeX + maxLinkPlaintext + chacha20poly1305.Overhead
)

// LinkKeys encrypts self-contained tracking links with no expiry. Each key
// generation is a 32-byte seed; the XChaCha20-Poly1305 key is derived from it
// with HKDF-SHA256, and the key id is a hash of that derived key, so it
// reveals nothing usable. The active seed issues links; every retained seed
// (active and previous) decrypts links issued under it. A retained seed is a
// secret that must be kept for as long as its links must keep working. Keys
// are immutable and shared by all control-plane replicas in an environment.
type LinkKeys struct {
	active   string
	aeads    map[string]cipher.AEAD
	hostKeys map[string][]byte // key id -> K_host, for the host self-check proof
	sizing   bool              // public all-zero key: never issues proofs
}

func (LinkKeys) String() string   { return "<tracking keys redacted>" }
func (LinkKeys) GoString() string { return "<tracking keys redacted>" }
func (LinkKeys) MarshalJSON() ([]byte, error) {
	return nil, errors.New("tracking keys cannot be serialized")
}

func deriveLinkKey(seed []byte) (string, cipher.AEAD, []byte, error) {
	if len(seed) != 32 {
		return "", nil, nil, errors.New("invalid tracking link keys")
	}
	key, err := hkdf.Key(sha256.New, seed, nil, linkKDFInfo, chacha20poly1305.KeySize)
	if err != nil {
		return "", nil, nil, errors.New("invalid tracking link keys")
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", nil, nil, errors.New("invalid tracking link keys")
	}
	hostKey, err := hkdf.Key(sha256.New, seed, nil, hostKDFInfo, 32)
	if err != nil {
		return "", nil, nil, errors.New("invalid tracking link keys")
	}
	sum := sha256.Sum256(append([]byte(linkKeyIDDomain), key...))
	return hex.EncodeToString(sum[:16]), aead, hostKey, nil
}

// NewLinkKeys accepts the active 32-byte seed and the seeds of earlier
// generations whose links must keep working. Duplicate seeds and the all-zero
// seed (the public sizing key) are rejected.
func NewLinkKeys(seed []byte, previous [][]byte) (*LinkKeys, error) {
	if allZero(seed) {
		return nil, errors.New("invalid tracking link keys")
	}
	for _, old := range previous {
		if allZero(old) {
			return nil, errors.New("invalid tracking link keys")
		}
	}
	return newLinkKeys(seed, previous)
}

// NewSizingLinkKeys returns the public, fixed all-zero key used only to
// measure link lengths. It must never encrypt a delivered link.
func NewSizingLinkKeys() *LinkKeys {
	keys, err := newLinkKeys(make([]byte, 32), nil)
	if err != nil {
		panic("tracking sizing keys")
	}
	keys.sizing = true
	return keys
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func newLinkKeys(seed []byte, previous [][]byte) (*LinkKeys, error) {
	if len(previous) > maxPreviousSeeds {
		return nil, errors.New("invalid tracking link keys")
	}
	id, aead, hostKey, err := deriveLinkKey(seed)
	if err != nil {
		return nil, err
	}
	k := &LinkKeys{active: id, aeads: map[string]cipher.AEAD{id: aead}, hostKeys: map[string][]byte{id: hostKey}}
	for _, old := range previous {
		oldID, oldAEAD, oldHost, err := deriveLinkKey(old)
		if err != nil || k.aeads[oldID] != nil {
			return nil, errors.New("invalid tracking link keys")
		}
		k.aeads[oldID] = oldAEAD
		k.hostKeys[oldID] = oldHost
	}
	return k, nil
}

// LoadLinkKeys reads {"seed":"base64", "previous_seeds":["base64"]}, each seed
// 32 bytes in standard base64. A deprecated empty "previous_public_keys" array
// is tolerated and ignored; a non-empty one is invalid. It rejects duplicate/unknown fields and never
// includes file contents in errors.
func LoadLinkKeys(path string) (*LinkKeys, error) {
	invalid := errors.New("invalid tracking link key file")
	file, err := os.Open(path)
	if err != nil {
		return nil, invalid
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxLinkKeysBytes+1))
	if err != nil || len(data) > maxLinkKeysBytes {
		return nil, invalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	open, err := d.Token()
	if err != nil || open != json.Delim('{') {
		return nil, invalid
	}
	seen := map[string]bool{}
	var encodedSeed string
	var previous [][]byte
	for d.More() {
		token, readErr := d.Token()
		name, ok := token.(string)
		if readErr != nil || !ok || seen[name] {
			return nil, invalid
		}
		seen[name] = true
		switch name {
		case "seed":
			if d.Decode(&encodedSeed) != nil {
				return nil, invalid
			}
		case "previous_seeds":
			start, readErr := d.Token()
			if readErr != nil || start != json.Delim('[') {
				return nil, invalid
			}
			for d.More() {
				var encoded string
				if len(previous) >= maxPreviousSeeds || d.Decode(&encoded) != nil {
					return nil, invalid
				}
				old, decodeErr := base64.StdEncoding.Strict().DecodeString(encoded)
				if decodeErr != nil || len(old) != 32 {
					return nil, invalid
				}
				previous = append(previous, old)
			}
			end, readErr := d.Token()
			if readErr != nil || end != json.Delim(']') {
				return nil, invalid
			}
		case "previous_public_keys":
			// Deprecated v1 field: only an empty array, which is ignored.
			start, readErr := d.Token()
			if readErr != nil || start != json.Delim('[') || d.More() {
				return nil, invalid
			}
			if end, readErr := d.Token(); readErr != nil || end != json.Delim(']') {
				return nil, invalid
			}
		default:
			return nil, invalid
		}
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || !seen["seed"] || d.Decode(new(any)) != io.EOF {
		return nil, invalid
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(encodedSeed)
	if err != nil {
		return nil, invalid
	}
	keys, err := NewLinkKeys(seed, previous)
	if err != nil {
		return nil, invalid
	}
	return keys, nil
}

// linkAAD binds a payload to the link kind and the key generation, so it
// cannot be replayed across either. It deliberately does not include the
// origin: any hostname that routes to this deployment serves any link.
func linkAAD(letter, keyID string) []byte {
	return []byte(linkDomain + "\n" + letter + "\n" + keyID)
}

// seal builds origin (a URL prefix only, never authenticated) + "/t/{letter}/v3/{key id}/{payload}". The payload is
// base64url(nonce || AEAD(plaintext)); the URL carries no message content or
// recipient token in the clear. The plaintext is the 16-byte token, a 2-byte
// big-endian destination length, the destination, then zero padding to a
// multiple of 64 bytes, so link length reveals the destination length only to
// the nearest block.
func (k *LinkKeys) seal(origin, letter, token, destination string) (string, error) {
	if k == nil || k.aeads[k.active] == nil {
		return "", errors.New("tracking link encryption unavailable")
	}
	if _, err := HashToken(token); err != nil {
		return "", err
	}
	raw, err := hex.DecodeString(token)
	if err != nil {
		return "", ErrNotFound
	}
	aead := k.aeads[k.active]
	if len(destination) > maxTargetBytes {
		return "", errors.New("invalid tracking destination")
	}
	plaintext := make([]byte, paddedLinkLen(len(destination)))
	copy(plaintext, raw)
	binary.BigEndian.PutUint16(plaintext[linkTokenBytes:], uint16(len(destination)))
	copy(plaintext[linkHeaderBytes:], destination)
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err = rand.Read(nonce); err != nil {
		return "", errors.New("tracking link encryption unavailable")
	}
	sealed := aead.Seal(nonce, nonce, plaintext, linkAAD(letter, k.active))
	return origin + "/t/" + letter + "/v3/" + k.active + "/" + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func paddedLinkLen(destination int) int {
	return (linkHeaderBytes + destination + linkPadBlock - 1) / linkPadBlock * linkPadBlock
}

func (k *LinkKeys) link(origin, kind, token, destination string) (string, error) {
	switch kind {
	case "click":
		if !eligibleDestination(destination) {
			return "", errors.New("invalid tracking destination")
		}
		return k.seal(origin, "c", token, destination)
	case "open":
		if destination != "" {
			return "", errors.New("invalid tracking destination")
		}
		return k.seal(origin, "o", token, "")
	}
	return "", errors.New("invalid tracking kind")
}

// Link builds an open or click URL for a token. kind is "open" or "click";
// destination must be empty for open.
func (k *LinkKeys) Link(origin, kind, token, destination string) (string, error) {
	return k.link(origin, kind, token, destination)
}

// UnsubscribeLink builds the one-click unsubscribe URL of a delivery:
// origin + "/t/u/v3/{key id}/{payload}". The delivery ID is opaque, not an
// address; the kind letter is authenticated, so an open or click payload never
// opens as an unsubscribe (and the reverse). No table row backs the link.
func (k *LinkKeys) UnsubscribeLink(origin, deliveryID string) (string, error) {
	return k.seal(origin, "u", deliveryID, "")
}

// open decrypts "/t/{letter}/v3/{key id}/{payload}" and returns the token and
// destination. It touches no storage. Every failure is ErrNotFound.
func (k *LinkKeys) open(path, letter string) (string, string, error) {
	if k == nil || len(path) > maxLinkPathBytes {
		return "", "", ErrNotFound
	}
	parts := strings.Split(path, "/")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "t" || parts[2] != letter || parts[3] != "v3" || len(parts[4]) != 32 {
		return "", "", ErrNotFound
	}
	aead := k.aeads[parts[4]]
	encoded := parts[5]
	if aead == nil || len(encoded) > base64.RawURLEncoding.EncodedLen(maxLinkPayloadSize) {
		return "", "", ErrNotFound
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(sealed) != encoded || len(sealed) < aead.NonceSize()+linkPadBlock+aead.Overhead() {
		return "", "", ErrNotFound
	}
	plaintext, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], linkAAD(letter, parts[4]))
	if err != nil || len(plaintext) < linkPadBlock || len(plaintext)%linkPadBlock != 0 {
		return "", "", ErrNotFound
	}
	n := int(binary.BigEndian.Uint16(plaintext[linkTokenBytes:]))
	// Canonical form: minimal padding, all zero.
	if n > maxTargetBytes || len(plaintext) != paddedLinkLen(n) || !allZero(plaintext[linkHeaderBytes+n:]) {
		return "", "", ErrNotFound
	}
	return hex.EncodeToString(plaintext[:linkTokenBytes]), string(plaintext[linkHeaderBytes : linkHeaderBytes+n]), nil
}

// verifyUnsubscribe returns the delivery ID of a valid unsubscribe path.
func (k *LinkKeys) verifyUnsubscribe(path string) (string, error) {
	id, destination, err := k.open(path, "u")
	if err != nil || destination != "" {
		return "", ErrNotFound
	}
	return id, nil
}

// verify resolves an open or click path to the token hash, kind and (click
// only) destination.
func (k *LinkKeys) verify(path string) ([32]byte, string, string, error) {
	var zero [32]byte
	kind, letter := "open", "o"
	if strings.HasPrefix(path, "/t/c/") {
		kind, letter = "click", "c"
	}
	token, destination, err := k.open(path, letter)
	if err != nil {
		return zero, "", "", ErrNotFound
	}
	hash, err := HashToken(token)
	if err != nil {
		return zero, "", "", ErrNotFound
	}
	if kind == "open" && destination != "" || kind == "click" && !eligibleDestination(destination) {
		return zero, "", "", ErrNotFound
	}
	return hash, kind, destination, nil
}
