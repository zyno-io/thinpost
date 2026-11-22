// Package prepared is the stored form of an SMTP delivery's signed message: a
// recipe. Instead of a full copy of the message per recipient, a delivery's
// "final/" object holds the exact output header block (DKIM-Signature, trace,
// Message-ID and the rest) and a list of byte splices over the message's stored
// source. Control rebuilds the message from the source at dispatch and checks
// its size and SHA-256 before anything is sent, so the bytes sent are the bytes
// that were signed.
//
// Object layout:
//
//	"thinpost-prepared-v1\n" | uint32 BE json_len (1..65536) | strict JSON |
//	header block (header_len bytes, ends CRLF CRLF) | splice data (sum of len)
//
// The JSON is a wire struct (see below); unknown fields, trailing data and any
// other version are refused. The output message is the header block, then the
// source body (source bytes from body_start) with every bare LF turned into
// CRLF, where each splice replaces source[start:end] with its data verbatim.
//
// This package must not import internal/rawmime: rawmime builds the same body
// bytes through AppendBody, which keeps exactly one CRLF normalizer. A test in
// rawmime pins MaxHeader to rawmime.MaxPreparedHeaders.
package prepared

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/zyno-io/thinpost/internal/edgeprotocol"
)

// The stored formats of a delivery's final object (deliveries.prepared_format).
const (
	// FormatFull is a complete signed MIME message. HTTP deliveries, and every
	// delivery prepared before recipes existed, use it.
	FormatFull = 1
	// FormatRecipe is a recipe object over the message's stored raw source.
	FormatRecipe = 2
)

const (
	// Magic starts every recipe object. A full MIME object starts with a header
	// field name and a colon, so it can never start with it.
	Magic = "thinpost-prepared-v1\n"
	// Version is the only recipe version.
	Version = 1
	// BaseRawSource names the only supported base: the raw bytes of a version 3
	// stored source.
	BaseRawSource = "raw_source"
	// MaxJSON bounds the JSON part.
	MaxJSON = 65536
	// MaxHeader bounds the header block: rawmime.MaxPreparedHeaders plus 4 KiB.
	MaxHeader = 60<<10 + 4096
	// MaxSplices bounds the splice list.
	MaxSplices = 256
	// MaxOverhead is the most a recipe object holds beyond splice data, which
	// itself is part of the message: a recipe object is at most
	// edgeprotocol.MaxMIMEBytes + MaxOverhead.
	MaxOverhead = len(Magic) + 4 + MaxJSON + MaxHeader
)

var (
	// ErrInvalid is returned for a malformed recipe or inputs that cannot form one.
	ErrInvalid = errors.New("invalid prepared recipe")
	// ErrMismatch is returned when a well-formed recipe does not assemble against
	// the base it is given.
	ErrMismatch = errors.New("prepared recipe does not match its base")
)

// Splice replaces base[Start:End] with the next Len bytes of splice data.
type Splice struct {
	Start int `json:"start"`
	End   int `json:"end"`
	Len   int `json:"len"`
}

// Replacement is a Splice with its data, the form New takes.
type Replacement struct {
	Start, End int
	Data       []byte
}

// wire is the strict JSON part.
type wire struct {
	Version    int      `json:"version"`
	DeliveryID string   `json:"delivery_id"`
	Base       string   `json:"base"`
	BaseSHA256 string   `json:"base_sha256"`
	BodyStart  int      `json:"body_start"`
	HeaderLen  int      `json:"header_len"`
	Splices    []Splice `json:"splices"`
	Size       int      `json:"size"`
	SHA256     string   `json:"sha256"`
}

// Recipe is a decoded (or freshly built) recipe.
type Recipe struct {
	w      wire
	header []byte
	data   []byte
}

// IsRecipe reports whether an object is a recipe (starts with the magic).
func IsRecipe(object []byte) bool { return bytes.HasPrefix(object, []byte(Magic)) }

// Header is the exact header block of the output message, ending CRLF CRLF.
// The caller must not modify it.
func (r *Recipe) Header() []byte { return r.header }

// DeliveryID is the delivery the recipe was built for.
func (r *Recipe) DeliveryID() string { return r.w.DeliveryID }

// BaseSHA256 is the lowercase hex SHA-256 of the base it assembles against.
func (r *Recipe) BaseSHA256() string { return r.w.BaseSHA256 }

// Size is the length of the assembled message.
func (r *Recipe) Size() int { return r.w.Size }

// SHA256 is the lowercase hex SHA-256 of the assembled message.
func (r *Recipe) SHA256() string { return r.w.SHA256 }

// New builds the recipe for a signed message whose first headerLen bytes are the
// header block and whose remainder is the source body from bodyStart with the
// replacements applied. It does not check that; callers assemble the encoded
// recipe against the source and compare (the preparation self-check).
func New(deliveryID, baseSHA string, bodyStart int, signed []byte, headerLen int, replacements []Replacement) (*Recipe, error) {
	sum := sha256.Sum256(signed)
	w := wire{
		Version: Version, DeliveryID: deliveryID, Base: BaseRawSource, BaseSHA256: baseSHA,
		BodyStart: bodyStart, HeaderLen: headerLen, Splices: make([]Splice, 0, len(replacements)),
		Size: len(signed), SHA256: hex.EncodeToString(sum[:]),
	}
	var data []byte
	for _, p := range replacements {
		w.Splices = append(w.Splices, Splice{Start: p.Start, End: p.End, Len: len(p.Data)})
		data = append(data, p.Data...)
	}
	if headerLen < 0 || headerLen > len(signed) {
		return nil, ErrInvalid
	}
	r := &Recipe{w: w, header: signed[:headerLen], data: data}
	if r.validate() != nil {
		return nil, ErrInvalid
	}
	return r, nil
}

// Encode returns the stored object.
func (r *Recipe) Encode() ([]byte, error) {
	j, err := json.Marshal(r.w)
	if err != nil || len(j) < 1 || len(j) > MaxJSON {
		return nil, ErrInvalid
	}
	out := make([]byte, 0, len(Magic)+4+len(j)+len(r.header)+len(r.data))
	out = append(out, Magic...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(j)))
	out = append(out, j...)
	out = append(out, r.header...)
	return append(out, r.data...), nil
}

// Decode strictly parses a recipe object. The returned Recipe aliases object.
func Decode(object []byte) (*Recipe, error) {
	if !IsRecipe(object) {
		return nil, ErrInvalid
	}
	rest := object[len(Magic):]
	if len(rest) < 4 {
		return nil, ErrInvalid
	}
	n := binary.BigEndian.Uint32(rest)
	rest = rest[4:]
	if n < 1 || n > MaxJSON || uint64(n) > uint64(len(rest)) {
		return nil, ErrInvalid
	}
	var w wire
	decoder := json.NewDecoder(bytes.NewReader(rest[:n]))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&w) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, ErrInvalid
	}
	rest = rest[n:]
	// Cheap bounds first so the arithmetic below cannot overflow.
	if w.HeaderLen < 4 || w.HeaderLen > MaxHeader || w.HeaderLen > len(rest) || len(w.Splices) > MaxSplices {
		return nil, ErrInvalid
	}
	r := &Recipe{w: w, header: rest[:w.HeaderLen], data: rest[w.HeaderLen:]}
	if r.validate() != nil {
		return nil, ErrInvalid
	}
	return r, nil
}

func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// crlfOnly reports data in which every LF follows a CR and every CR precedes an LF.
func crlfOnly(data []byte) bool {
	for i, c := range data {
		switch c {
		case '\n':
			if i == 0 || data[i-1] != '\r' {
				return false
			}
		case '\r':
			if i+1 >= len(data) || data[i+1] != '\n' {
				return false
			}
		}
	}
	return true
}

func (r *Recipe) validate() error {
	w := &r.w
	if w.Version != Version || w.Base != BaseRawSource || !lowerHex(w.DeliveryID, 32) || !lowerHex(w.BaseSHA256, 64) || !lowerHex(w.SHA256, 64) {
		return ErrInvalid
	}
	if w.Size < 1 || w.Size > edgeprotocol.MaxMIMEBytes || w.BodyStart < 0 {
		return ErrInvalid
	}
	if w.HeaderLen < 4 || w.HeaderLen > MaxHeader || w.HeaderLen != len(r.header) || !bytes.HasSuffix(r.header, []byte("\r\n\r\n")) {
		return ErrInvalid
	}
	if len(w.Splices) > MaxSplices {
		return ErrInvalid
	}
	total, prevEnd := int64(0), 0
	for _, p := range w.Splices {
		if p.Start < w.BodyStart || p.Start > p.End || p.Start < prevEnd || p.Len < 0 || p.Len > edgeprotocol.MaxMIMEBytes {
			return ErrInvalid
		}
		prevEnd = p.End
		total += int64(p.Len)
		if total > int64(len(r.data)) {
			return ErrInvalid
		}
	}
	if total != int64(len(r.data)) || int64(w.HeaderLen)+total > int64(w.Size) {
		return ErrInvalid
	}
	// Each splice's data is whole CRLF lines, so no splice can inject a bare LF
	// or CR, or leave a CR for the base to pair with.
	pos := 0
	for _, p := range w.Splices {
		if !crlfOnly(r.data[pos : pos+p.Len]) {
			return ErrInvalid
		}
		pos += p.Len
	}
	return nil
}

// AppendBody appends the output body to dst: base from bodyStart on, every bare
// LF turned into CRLF, with each splice's span replaced by its data verbatim.
// splices must be in order and within [bodyStart, len(base)], and data exactly
// their lengths; anything else returns ErrInvalid. A splice boundary between a
// CR and an LF of base is refused by Assemble, not here (nothing is read across
// it: an LF's preceding CR is looked up in base itself).
//
// It is the only CRLF normalizer: rawmime builds an untracked message's body
// through it with no splices.
func AppendBody(dst, base []byte, bodyStart int, splices []Splice, data []byte) ([]byte, error) {
	if bodyStart < 0 || bodyStart > len(base) {
		return dst, ErrInvalid
	}
	pos, dataPos := bodyStart, 0
	for _, p := range splices {
		if p.Start < pos || p.Start > p.End || p.End > len(base) || p.Len < 0 || p.Len > len(data)-dataPos {
			return dst, ErrInvalid
		}
		dst = appendNormalized(dst, base, pos, p.Start)
		dst = append(dst, data[dataPos:dataPos+p.Len]...)
		pos, dataPos = p.End, dataPos+p.Len
	}
	if dataPos != len(data) {
		return dst, ErrInvalid
	}
	return appendNormalized(dst, base, pos, len(base)), nil
}

func appendNormalized(dst, base []byte, from, to int) []byte {
	for pos := from; pos < to; {
		i := bytes.IndexByte(base[pos:to], '\n')
		if i < 0 {
			return append(dst, base[pos:to]...)
		}
		at := pos + i
		dst = append(dst, base[pos:at]...)
		if at == 0 || base[at-1] != '\r' {
			dst = append(dst, '\r')
		}
		dst = append(dst, '\n')
		pos = at + 1
	}
	return dst
}

// Assemble rebuilds the message from base (the raw bytes of the stored source)
// and checks it: baseSHA must be the SHA-256 the recipe was built against, the
// splices (and the body start) must lie within the base and not divide a CRLF of it, and the
// result must have the recorded size and SHA-256.
func (r *Recipe) Assemble(base []byte, baseSHA string) ([]byte, error) {
	w := &r.w
	if baseSHA != w.BaseSHA256 || w.BodyStart > len(base) || divides(base, w.BodyStart) {
		return nil, ErrMismatch
	}
	for _, p := range w.Splices {
		if p.End > len(base) || divides(base, p.Start) || divides(base, p.End) {
			return nil, ErrMismatch
		}
	}
	out := make([]byte, 0, w.Size)
	out = append(out, r.header...)
	out, err := AppendBody(out, base, w.BodyStart, w.Splices, r.data)
	if err != nil || len(out) != w.Size {
		return nil, ErrMismatch
	}
	sum := sha256.Sum256(out)
	if hex.EncodeToString(sum[:]) != w.SHA256 {
		return nil, ErrMismatch
	}
	return out, nil
}

// divides reports a position that falls between a CR and an LF of base.
func divides(base []byte, at int) bool {
	return at > 0 && at < len(base) && base[at-1] == '\r' && base[at] == '\n'
}
