package controlsession

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/bounceingest"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
)

type bounceAssembler struct {
	start *controlv1.BounceStart
	body  []byte
}

func newBounceAssembler(start *controlv1.BounceStart) (*bounceAssembler, error) {
	if start == nil || !hexID(start.BounceId) || !hexID(start.RecipientToken) || start.ExpectedSize == 0 || start.ExpectedSize > bounceingest.MaxBytes || len(start.Sha256) != sha256.Size {
		return nil, errors.New("invalid bounce start")
	}
	return &bounceAssembler{start: start, body: make([]byte, 0, int(start.ExpectedSize))}, nil
}
func hexID(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func (a *bounceAssembler) add(chunk *controlv1.DataChunk) ([]byte, error) {
	if chunk == nil || chunk.Id != a.start.BounceId || chunk.Offset != uint64(len(a.body)) || len(chunk.Data) == 0 || len(chunk.Data) > edgeprotocol.MaxChunkBytes || uint64(len(chunk.Data)) > a.start.ExpectedSize-uint64(len(a.body)) {
		return nil, errors.New("invalid bounce chunk")
	}
	a.body = append(a.body, chunk.Data...)
	if chunk.Last != (uint64(len(a.body)) == a.start.ExpectedSize) {
		return nil, errors.New("invalid final bounce chunk")
	}
	if !chunk.Last {
		return nil, nil
	}
	digest := sha256.Sum256(a.body)
	if subtle.ConstantTimeCompare(digest[:], a.start.Sha256) != 1 {
		return nil, errors.New("bounce digest mismatch")
	}
	return a.body, nil
}
