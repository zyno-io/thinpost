package messagebody

import (
	"bytes"
	"errors"
	"mime"

	"github.com/zyno-io/thinpost/internal/admin"
)

const (
	maxHeaderBlock  = 64 << 10
	maxHeaderFields = 256
)

var errHeaders = errors.New("invalid MIME headers")

// ParseHeaders reads the header block of a MIME message: everything before the
// first empty line (CRLF CRLF), at most 64 KiB. Continuation lines are
// unfolded by removing the line break, values are otherwise raw (RFC 2047
// words are not decoded in Value), and DecodedValue is set only when RFC 2047
// decoding changes the value. At most 256 fields are accepted and names must
// be printable ASCII without ":". Errors are constant and never quote input.
func ParseHeaders(data []byte) ([]admin.HeaderField, error) {
	limit := len(data)
	if limit > maxHeaderBlock+4 {
		limit = maxHeaderBlock + 4
	}
	end := bytes.Index(data[:limit], []byte("\r\n\r\n"))
	if end < 0 {
		return nil, errHeaders
	}
	block := data[:end+2] // keep the final CRLF so every line ends the same way
	var fields []admin.HeaderField
	var name string
	var value []byte
	have := false
	flush := func() {
		if !have {
			return
		}
		raw := string(bytes.TrimRight(bytes.TrimLeft(value, " \t"), " \t"))
		field := admin.HeaderField{Name: name, Value: raw}
		if decoded, err := new(mime.WordDecoder).DecodeHeader(raw); err == nil && decoded != raw {
			field.DecodedValue = decoded
		}
		fields = append(fields, field)
		have = false
	}
	for len(block) > 0 {
		line, rest, found := bytes.Cut(block, []byte("\r\n"))
		if !found {
			return nil, errHeaders
		}
		block = rest
		if len(line) == 0 {
			return nil, errHeaders
		}
		if line[0] == ' ' || line[0] == '\t' {
			if !have {
				return nil, errHeaders
			}
			value = append(value, line...)
			continue
		}
		flush()
		colon := bytes.IndexByte(line, ':')
		if colon < 1 || len(fields) >= maxHeaderFields {
			return nil, errHeaders
		}
		for _, c := range line[:colon] {
			if c < 33 || c > 126 {
				return nil, errHeaders
			}
		}
		name = string(line[:colon])
		value = append(value[:0:0], line[colon+1:]...)
		have = true
	}
	flush()
	if len(fields) == 0 {
		return nil, errHeaders
	}
	return fields, nil
}
