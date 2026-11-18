package tracking

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/objectstore"
)

const maxMappingObjectBytes = 512 << 10

// Reference is safe to persist as an index: the raw bearer token and click
// destination remain out of the database. Delivery ownership, expiration and
// event limits must be committed alongside these references separately.
type Reference struct {
	ID   string
	Hash [32]byte
	Kind string
}

type protectedEntry struct {
	Hash        string `json:"hash"`
	Kind        string `json:"kind"`
	Destination string `json:"destination,omitempty"`
}

func (protectedEntry) String() string   { return "<tracking destination redacted>" }
func (protectedEntry) GoString() string { return "<tracking destination redacted>" }

type protectedObject struct {
	Version int              `json:"version"`
	Entries []protectedEntry `json:"entries"`
}

// ProtectedStore writes one encrypted mapping object per delivery. It never
// persists raw capability tokens. The returned references are not usable
// for analytics until a caller commits them with the matching delivery. Encrypted
// redirect/pixel responses do not depend on these references.
type ProtectedStore struct{ objects *objectstore.EncryptedStore }

func NewProtectedStore(objects *objectstore.EncryptedStore) (*ProtectedStore, error) {
	if objects == nil {
		return nil, errors.New("encrypted tracking storage is required")
	}
	return &ProtectedStore{objects: objects}, nil
}

// NewMappingKey uses a distinct namespace. Do not enable tracking until its
// reference-aware collector, expiration and recovery controls are deployed.
func NewMappingKey(deliveryID string, issued time.Time) (string, error) {
	key, err := objectkey.New("tracking", deliveryID, issued)
	if err != nil {
		return "", errors.New("invalid tracking object key")
	}
	return key, nil
}

func validMappingKey(key string) bool {
	if !strings.HasPrefix(key, "tracking/") {
		return false
	}
	parsed, err := objectkey.Parse(key)
	return err == nil && parsed.Kind == "tracking"
}

// Put validates and encrypts the complete set of mappings for one delivery.
// Neither raw bearer tokens nor destinations may be sent to logs or errors.
func (s *ProtectedStore) Put(ctx context.Context, key string, mappings []Mapping) ([]Reference, error) {
	if s == nil || s.objects == nil || !validMappingKey(key) || len(mappings) == 0 || len(mappings) > maxLinks+1 {
		return nil, errors.New("invalid tracking mappings")
	}
	entries := make([]protectedEntry, 0, len(mappings))
	refs := make([]Reference, 0, len(mappings))
	seen := make(map[[32]byte]bool, len(mappings))
	seenIDs := make(map[string]bool, len(mappings))
	opens := 0
	clicks := 0
	for _, mapping := range mappings {
		hash, err := HashToken(mapping.Token)
		if err != nil || seen[hash] {
			return nil, errors.New("invalid tracking mappings")
		}
		seen[hash] = true
		id, err := randomToken()
		if err != nil || seenIDs[id] {
			return nil, errors.New("cannot create tracking identifier")
		}
		seenIDs[id] = true
		switch mapping.Kind {
		case "open":
			opens++
			if opens > 1 || mapping.Destination != "" {
				return nil, errors.New("invalid tracking mappings")
			}
		case "click":
			clicks++
			if clicks > maxLinks || !eligibleDestination(mapping.Destination) {
				return nil, errors.New("invalid tracking mappings")
			}
		default:
			return nil, errors.New("invalid tracking mappings")
		}
		entries = append(entries, protectedEntry{Hash: hex.EncodeToString(hash[:]), Kind: mapping.Kind, Destination: mapping.Destination})
		refs = append(refs, Reference{ID: id, Hash: hash, Kind: mapping.Kind})
	}
	data, err := json.Marshal(protectedObject{Version: 1, Entries: entries})
	if err != nil || len(data) > maxMappingObjectBytes {
		return nil, errors.New("invalid tracking mappings")
	}
	if err = s.objects.Put(ctx, key, data); err != nil {
		return nil, errors.New("tracking object could not be stored")
	}
	return refs, nil
}

// Lookup reads an encrypted object already selected by a committed database
// reference. Callers must separately check app ownership, kind, expiry and
// revocation and must not accept object keys or destinations from the request.
func (s *ProtectedStore) Lookup(ctx context.Context, key string, hash [32]byte, kind string) (string, error) {
	if s == nil || s.objects == nil || !validMappingKey(key) || (kind != "open" && kind != "click") {
		return "", errors.New("invalid tracking lookup")
	}
	data, err := s.objects.Get(ctx, key, maxMappingObjectBytes)
	if err != nil {
		return "", errors.New("tracking object unavailable")
	}
	object, err := decodeProtectedObject(data)
	if err != nil {
		return "", errors.New("invalid tracking object")
	}
	seen := make(map[string]bool, len(object.Entries))
	var destination string
	found := false
	opens := 0
	clicks := 0
	for _, entry := range object.Entries {
		decoded, decodeErr := hex.DecodeString(entry.Hash)
		if decodeErr != nil || len(decoded) != 32 || entry.Hash != strings.ToLower(entry.Hash) || seen[entry.Hash] {
			return "", errors.New("invalid tracking object")
		}
		seen[entry.Hash] = true
		switch entry.Kind {
		case "open":
			opens++
			if opens > 1 || entry.Destination != "" {
				return "", errors.New("invalid tracking object")
			}
		case "click":
			clicks++
			if clicks > maxLinks || !eligibleDestination(entry.Destination) {
				return "", errors.New("invalid tracking object")
			}
		default:
			return "", errors.New("invalid tracking object")
		}
		if subtle.ConstantTimeCompare(decoded, hash[:]) == 1 && entry.Kind == kind {
			found, destination = true, entry.Destination
		}
	}
	if !found {
		return "", ErrNotFound
	}
	return destination, nil
}

// Reject duplicate keys as well as unknown fields so a corrupt object cannot
// be interpreted differently by different readers after a future upgrade.
func decodeProtectedObject(data []byte) (protectedObject, error) {
	var object protectedObject
	invalid := errors.New("invalid tracking object")
	d := json.NewDecoder(bytes.NewReader(data))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return object, invalid
	}
	seen := map[string]bool{}
	for d.More() {
		token, readErr := d.Token()
		name, ok := token.(string)
		if readErr != nil || !ok || seen[name] {
			return object, invalid
		}
		seen[name] = true
		switch name {
		case "version":
			var version *int
			if d.Decode(&version) != nil || version == nil {
				return object, invalid
			}
			object.Version = *version
		case "entries":
			open, readErr := d.Token()
			if readErr != nil || open != json.Delim('[') {
				return object, invalid
			}
			for d.More() {
				if len(object.Entries) >= maxLinks+1 {
					return object, invalid
				}
				entry, readErr := decodeProtectedEntry(d)
				if readErr != nil {
					return object, invalid
				}
				object.Entries = append(object.Entries, entry)
			}
			close, readErr := d.Token()
			if readErr != nil || close != json.Delim(']') {
				return object, invalid
			}
		default:
			return object, invalid
		}
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || !seen["version"] || !seen["entries"] || object.Version != 1 || len(object.Entries) == 0 {
		return protectedObject{}, invalid
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return protectedObject{}, invalid
	}
	return object, nil
}

func decodeProtectedEntry(d *json.Decoder) (protectedEntry, error) {
	var entry protectedEntry
	invalid := errors.New("invalid tracking object")
	open, err := d.Token()
	if err != nil || open != json.Delim('{') {
		return entry, invalid
	}
	seen := map[string]bool{}
	for d.More() {
		token, readErr := d.Token()
		name, ok := token.(string)
		if readErr != nil || !ok || seen[name] {
			return entry, invalid
		}
		seen[name] = true
		switch name {
		case "hash":
			value, readErr := decodeProtectedString(d)
			if readErr != nil {
				return entry, invalid
			}
			entry.Hash = value
		case "kind":
			value, readErr := decodeProtectedString(d)
			if readErr != nil {
				return entry, invalid
			}
			entry.Kind = value
		case "destination":
			value, readErr := decodeProtectedString(d)
			if readErr != nil {
				return entry, invalid
			}
			entry.Destination = value
		default:
			return entry, invalid
		}
	}
	close, err := d.Token()
	if err != nil || close != json.Delim('}') || !seen["hash"] || !seen["kind"] {
		return protectedEntry{}, invalid
	}
	return entry, nil
}

func decodeProtectedString(d *json.Decoder) (string, error) {
	var value *string
	if err := d.Decode(&value); err != nil || value == nil {
		return "", errors.New("invalid tracking object")
	}
	return *value, nil
}
