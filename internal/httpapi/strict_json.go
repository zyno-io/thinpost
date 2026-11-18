package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/submission"
)

// Reject duplicate names recursively before decoding into Go structs. The JSON
// decoder matches struct fields case-insensitively and otherwise permits later
// values to replace earlier ones, making the requested recipients or content
// ambiguous to callers and intermediaries. The input is already size-limited.
func rejectAmbiguousJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := checkJSONValue(decoder, 0); err != nil {
		return submission.ErrInvalidMessage
	}
	if _, err := decoder.Token(); err != io.EOF {
		return submission.ErrInvalidMessage
	}
	return nil
}

func checkJSONValue(decoder *json.Decoder, depth int) error {
	// Supported provider payloads are shallow. Bound stack usage even for a
	// syntactically valid but hostile request with thousands of nested arrays.
	if depth > 100 {
		return submission.ErrInvalidMessage
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			key, keyErr := decoder.Token()
			name, ok := key.(string)
			if keyErr != nil || !ok {
				return submission.ErrInvalidMessage
			}
			folded := foldJSONField(name)
			if _, exists := seen[folded]; exists {
				return submission.ErrInvalidMessage
			}
			seen[folded] = struct{}{}
			if err = checkJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, endErr := decoder.Token()
		if endErr != nil || end != json.Delim('}') {
			return submission.ErrInvalidMessage
		}
	case '[':
		for decoder.More() {
			if err = checkJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, endErr := decoder.Token()
		if endErr != nil || end != json.Delim(']') {
			return submission.ErrInvalidMessage
		}
	default:
		return submission.ErrInvalidMessage
	}
	return nil
}

// Use the same simple Unicode case-fold equivalence as encoding/json when it
// matches a JSON name to a struct field, including non-ASCII folds such as K.
func foldJSONField(name string) string {
	result := make([]byte, 0, len(name))
	for _, r := range name {
		if r < utf8.RuneSelf {
			if r >= 'a' && r <= 'z' {
				r -= 'a' - 'A'
			}
		} else {
			for {
				next := unicode.SimpleFold(r)
				if next <= r {
					r = next
					break
				}
				r = next
			}
		}
		result = utf8.AppendRune(result, r)
	}
	return string(result)
}
