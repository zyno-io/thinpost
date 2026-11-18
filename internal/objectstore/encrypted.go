package objectstore

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
)

const (
	envelopeMagic       = "TPOST\x00\x01"
	secretMagic         = "TPSEC\x00\x01"
	maxSecretPlaintext  = 16 << 10
	objectSaltSize      = 32
	maxKeyIDSize        = 64
	maxKeys             = 32
	maxKeyringBytes     = 16 << 10
	maxPlaintextBytes   = 16 << 20
	maxEnvelopeOverhead = len(envelopeMagic) + 1 + maxKeyIDSize + objectSaltSize + 28
)

// RawObjects stores opaque immutable bytes. An implementation must honor the
// size limit on reads and reject overwrites.
type RawObjects interface {
	Put(context.Context, string, []byte) error
	Get(context.Context, string, int64) ([]byte, error)
	VerifyPresent(context.Context, string) error
}

// Keyring is immutable after loading. Keep old versions until every object and
// backup that uses them has expired or been re-encrypted.
type Keyring struct {
	active string
	keys   map[string][]byte
}

// LoadKeyring reads a bounded JSON object with an active key ID and a map of
// IDs to base64-encoded 256-bit keys. Errors never contain secret values.
func LoadKeyring(path string) (*Keyring, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open object encryption keyring")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxKeyringBytes+1))
	if err != nil || len(data) > maxKeyringBytes {
		return nil, errors.New("invalid object encryption keyring")
	}
	return parseKeyring(data)
}

func parseKeyring(data []byte) (*Keyring, error) {
	invalid := errors.New("invalid object encryption keyring")
	decoder := json.NewDecoder(bytes.NewReader(data))
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') {
		return nil, invalid
	}
	ring := &Keyring{keys: make(map[string][]byte)}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return nil, invalid
		}
		seen[name] = true
		switch name {
		case "active":
			if decoder.Decode(&ring.active) != nil || !validKeyID(ring.active) {
				return nil, invalid
			}
		case "keys":
			start, err := decoder.Token()
			if err != nil || start != json.Delim('{') {
				return nil, invalid
			}
			for decoder.More() {
				keyToken, err := decoder.Token()
				id, ok := keyToken.(string)
				if err != nil || !ok || !validKeyID(id) || len(ring.keys) >= maxKeys || ring.keys[id] != nil {
					return nil, invalid
				}
				var encoded string
				if decoder.Decode(&encoded) != nil {
					return nil, invalid
				}
				key, err := base64.StdEncoding.Strict().DecodeString(encoded)
				if err != nil || len(key) != 32 {
					return nil, invalid
				}
				for _, prior := range ring.keys {
					if bytes.Equal(prior, key) {
						return nil, invalid
					}
				}
				ring.keys[id] = key
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return nil, invalid
			}
		default:
			return nil, invalid
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || !seen["active"] || !seen["keys"] || ring.keys[ring.active] == nil {
		return nil, invalid
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, invalid
	}
	return ring, nil
}

func validKeyID(value string) bool {
	if len(value) == 0 || len(value) > maxKeyIDSize {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// EncryptedStore authenticates the object key and a versioned envelope as
// associated data. A random salt derives a separate AES-256-GCM key for each
// write; GCM supplies its own random nonce. No plaintext fallback is provided.
type EncryptedStore struct {
	raw  RawObjects
	keys *Keyring
}

func NewEncrypted(raw RawObjects, keys *Keyring) (*EncryptedStore, error) {
	if raw == nil || keys == nil || keys.keys[keys.active] == nil {
		return nil, errors.New("object storage and encryption keyring are required")
	}
	return &EncryptedStore{raw: raw, keys: keys}, nil
}

func (s *EncryptedStore) Put(ctx context.Context, key string, data []byte) error {
	if !validObjectKey(key) || len(data) > maxPlaintextBytes {
		return errors.New("invalid object")
	}
	header := make([]byte, 0, len(envelopeMagic)+1+len(s.keys.active)+objectSaltSize)
	header = append(header, envelopeMagic...)
	header = append(header, byte(len(s.keys.active)))
	header = append(header, s.keys.active...)
	salt := make([]byte, objectSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return errors.New("cannot generate object encryption salt")
	}
	header = append(header, salt...)
	aead, err := objectAEAD(s.keys.keys[s.keys.active], salt)
	if err != nil {
		return errors.New("cannot initialize object encryption")
	}
	aad := append(bytes.Clone(header), key...)
	envelope := aead.Seal(bytes.Clone(header), nil, data, aad)
	return s.raw.Put(ctx, key, envelope)
}

func (s *EncryptedStore) PutMIME(ctx context.Context, key string, data []byte) error {
	return s.Put(ctx, key, data)
}

// VerifyPresent is used under the database's per-object lock immediately
// before committing a reference. The storage service must provide strong
// read-after-write and read-after-delete consistency for HEAD operations.
func (s *EncryptedStore) VerifyPresent(ctx context.Context, key string) error {
	if !validObjectKey(key) {
		return errors.New("invalid object key")
	}
	if err := s.raw.VerifyPresent(ctx, key); err != nil {
		return errors.New("object presence cannot be verified")
	}
	return nil
}

func (s *EncryptedStore) Get(ctx context.Context, key string, maximum int64) ([]byte, error) {
	if !validObjectKey(key) || maximum <= 0 || maximum > maxPlaintextBytes {
		return nil, errors.New("invalid object size limit or key")
	}
	envelope, err := s.raw.Get(ctx, key, maximum+int64(maxEnvelopeOverhead))
	if err != nil {
		return nil, err
	}
	if len(envelope) < len(envelopeMagic)+1 || string(envelope[:len(envelopeMagic)]) != envelopeMagic {
		return nil, errors.New("invalid encrypted object")
	}
	idLength := int(envelope[len(envelopeMagic)])
	headerLength := len(envelopeMagic) + 1 + idLength + objectSaltSize
	if idLength == 0 || idLength > maxKeyIDSize || len(envelope) < headerLength+28 {
		return nil, errors.New("invalid encrypted object")
	}
	id := string(envelope[len(envelopeMagic)+1 : len(envelopeMagic)+1+idLength])
	master := s.keys.keys[id]
	if master == nil {
		return nil, errors.New("object encryption key version unavailable")
	}
	header := envelope[:headerLength]
	salt := header[headerLength-objectSaltSize:]
	aead, err := objectAEAD(master, salt)
	if err != nil {
		return nil, errors.New("cannot initialize object decryption")
	}
	aad := append(bytes.Clone(header), key...)
	plaintext, err := aead.Open(nil, nil, envelope[headerLength:], aad)
	if err != nil || int64(len(plaintext)) > maximum {
		return nil, errors.New("invalid or oversized encrypted object")
	}
	return plaintext, nil
}

// Seal encrypts a small database-stored secret with the active key. aad must
// identify the owning row; Open fails if the ciphertext is moved to another row.
func (k *Keyring) Seal(aad, plaintext []byte) ([]byte, error) {
	if k == nil || k.keys[k.active] == nil || len(plaintext) > maxSecretPlaintext {
		return nil, errors.New("invalid secret")
	}
	header := make([]byte, 0, len(secretMagic)+1+len(k.active)+objectSaltSize)
	header = append(header, secretMagic...)
	header = append(header, byte(len(k.active)))
	header = append(header, k.active...)
	salt := make([]byte, objectSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, errors.New("cannot generate secret encryption salt")
	}
	header = append(header, salt...)
	aead, err := newAEAD(k.keys[k.active], salt, secretKeyInfo)
	if err != nil {
		return nil, errors.New("cannot initialize secret encryption")
	}
	return aead.Seal(bytes.Clone(header), nil, plaintext, append(bytes.Clone(header), aad...)), nil
}

// Open decrypts an envelope produced by Seal using any key ID in the ring.
func (k *Keyring) Open(aad, envelope []byte) ([]byte, error) {
	invalid := errors.New("invalid sealed secret")
	if k == nil || len(envelope) < len(secretMagic)+1 || string(envelope[:len(secretMagic)]) != secretMagic ||
		len(envelope) > len(secretMagic)+1+maxKeyIDSize+objectSaltSize+maxSecretPlaintext+28 {
		return nil, invalid
	}
	idLength := int(envelope[len(secretMagic)])
	headerLength := len(secretMagic) + 1 + idLength + objectSaltSize
	if idLength == 0 || idLength > maxKeyIDSize || len(envelope) < headerLength+28 {
		return nil, invalid
	}
	master := k.keys[string(envelope[len(secretMagic)+1:len(secretMagic)+1+idLength])]
	if master == nil {
		return nil, errors.New("secret encryption key version unavailable")
	}
	header := envelope[:headerLength]
	aead, err := newAEAD(master, header[headerLength-objectSaltSize:], secretKeyInfo)
	if err != nil {
		return nil, errors.New("cannot initialize secret decryption")
	}
	plaintext, err := aead.Open(nil, nil, envelope[headerLength:], append(bytes.Clone(header), aad...))
	if err != nil || len(plaintext) > maxSecretPlaintext {
		return nil, invalid
	}
	return plaintext, nil
}

func objectAEAD(master, salt []byte) (cipher.AEAD, error) {
	return newAEAD(master, salt, "thinpost-object-aes-gcm-v1")
}

const secretKeyInfo = "thinpost-secret-aes-gcm-v1"

func newAEAD(master, salt []byte, info string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, master, salt, info, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func validObjectKey(key string) bool {
	return len(key) > 0 && len(key) <= 1024 && !bytes.ContainsRune([]byte(key), '\x00')
}
