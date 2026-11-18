// Package edgeprotocol validates bounded delivery frames received over the
// authenticated trusted-to-edge stream. Session and lease wiring is pending.
package edgeprotocol

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"google.golang.org/protobuf/proto"
)

const MaxMIMEBytes = 8 << 20
const MaxChunkBytes = 64 << 10
const Version = 7

// DeliveryDigest binds a permit to every execution field, including the MIME
// digest, recipient, return path, route and TLS requirement.
func DeliveryDigest(start *controlv1.DeliveryStart) ([32]byte, error) {
	if !validStart(start) {
		return [32]byte{}, errors.New("invalid delivery descriptor")
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(start)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

type Assembler struct {
	id       string
	size     uint64
	expected [32]byte
	body     []byte
	done     bool
}

func NewAssembler(start *controlv1.DeliveryStart) (*Assembler, error) {
	if !validStart(start) {
		return nil, errors.New("invalid delivery start")
	}
	assembler := &Assembler{id: start.AttemptId, size: start.ExpectedSize, body: make([]byte, 0, int(start.ExpectedSize))}
	copy(assembler.expected[:], start.Sha256)
	return assembler, nil
}

// VerifyMessage is used again at the SMTP execution boundary so no caller can
// bypass the declared size and digest checks by skipping chunk assembly.
func VerifyMessage(start *controlv1.DeliveryStart, body []byte) bool {
	if !validStart(start) || uint64(len(body)) != start.ExpectedSize {
		return false
	}
	actual := sha256.Sum256(body)
	return subtle.ConstantTimeCompare(actual[:], start.Sha256) == 1
}

func validStart(start *controlv1.DeliveryStart) bool {
	return start != nil && opaque(start.AttemptId) && opaque(start.DeliveryId) && start.Epoch != 0 && start.ExpectedSize != 0 && start.ExpectedSize <= MaxMIMEBytes && len(start.Sha256) == sha256.Size && start.TrackingSendCutoffUnixMs >= 0 && start.TrackingSendCutoffUnixMs <= 253402300799999
}

// Add returns the assembled bytes once the final, hash-verified chunk arrives.
// A stream owner must discard the assembler on any error or disconnect.
func (a *Assembler) Add(chunk *controlv1.DataChunk) ([]byte, error) {
	if a.done || chunk == nil || chunk.Id != a.id || chunk.Offset != uint64(len(a.body)) || len(chunk.Data) == 0 || len(chunk.Data) > MaxChunkBytes || uint64(len(chunk.Data)) > a.size-uint64(len(a.body)) {
		a.done = true
		a.body = nil
		return nil, errors.New("invalid MIME chunk")
	}
	a.body = append(a.body, chunk.Data...)
	if chunk.Last != (uint64(len(a.body)) == a.size) {
		a.done = true
		a.body = nil
		return nil, errors.New("invalid final chunk")
	}
	if !chunk.Last {
		return nil, nil
	}
	a.done = true
	actual := sha256.Sum256(a.body)
	if subtle.ConstantTimeCompare(actual[:], a.expected[:]) != 1 {
		a.body = nil
		return nil, errors.New("MIME digest mismatch")
	}
	result := a.body
	a.body = nil
	return result, nil
}
func opaque(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
