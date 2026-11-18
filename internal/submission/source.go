package submission

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// SourceVersion is the stored-source version written for every message
// accepted by this release. Version 1 sources (accepted earlier) are still
// readable; they keep the single-recipient To header for life and cannot carry
// Cc or Bcc. Never reinterpret version 2: a change in header behaviour needs a
// new version.
const SourceVersion = 2

// ErrInvalidSource reports a stored source object that is malformed, has an
// unsupported version, or does not match the recorded policy.
var ErrInvalidSource = errors.New("invalid source object")

// Source is a decoded stored source object.
type Source struct {
	Version int
	Message Message
}

// ListsRecipients reports whether copies of this message show the full To and
// Cc lists (version 2 and later) instead of only the recipient's own address.
func (s Source) ListsRecipients() bool { return s.Version >= 2 }

type sourceEnvelope struct {
	Version int     `json:"version"`
	Message Message `json:"message"`
}

// EncodeSource encodes the stored source object for a message accepted now.
func EncodeSource(m Message) ([]byte, error) {
	return json.Marshal(sourceEnvelope{SourceVersion, m})
}

// DecodeSource strictly decodes and validates a stored source object. The
// error is always ErrInvalidSource and never includes content.
func DecodeSource(data []byte, enhancedProtection bool) (Source, error) {
	var env sourceEnvelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&env); err != nil {
		return Source{}, ErrInvalidSource
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Source{}, ErrInvalidSource
	}
	if env.Version != 1 && env.Version != SourceVersion {
		return Source{}, ErrInvalidSource
	}
	if env.Version == 1 && (len(env.Message.Cc) > 0 || len(env.Message.Bcc) > 0) {
		return Source{}, ErrInvalidSource
	}
	if env.Message.EnhancedProtection != enhancedProtection || env.Message.Validate() != nil {
		return Source{}, ErrInvalidSource
	}
	return Source{Version: env.Version, Message: env.Message}, nil
}
