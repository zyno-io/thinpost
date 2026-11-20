package rawmime

import "strings"

const foldTarget = 78

// FoldField renders "name: value\r\n", folding at single spaces toward 78 octets
// per line. Every physical line, the name prefix included, must be at most 998
// octets; otherwise ErrInvalid. The value must be one non-empty ASCII line
// without control characters or edge whitespace. Runs of spaces are never split.
func FoldField(name, value string) (string, error) {
	if value == "" || value[0] == ' ' || value[len(value)-1] == ' ' {
		return "", ErrInvalid
	}
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < ' ' && c != '\t' || c >= 0x7f {
			return "", ErrInvalid
		}
	}
	var out strings.Builder
	out.Grow(len(name) + len(value) + len(value)/foldTarget*3 + 8)
	out.WriteString(name)
	out.WriteString(": ")
	line := len(name) + 2
	start := 0
	for start < len(value) {
		// The next word runs up to the next foldable space (a lone space).
		end := len(value)
		for i := start + 1; i < len(value)-1; i++ {
			if value[i] == ' ' && !isBlank(value[i-1]) && !isBlank(value[i+1]) {
				end = i
				break
			}
		}
		word := value[start:end]
		if start > 0 && line+len(word)+1 > foldTarget {
			out.WriteString("\r\n ")
			line = 1
		} else if start > 0 {
			out.WriteByte(' ')
			line++
		}
		out.WriteString(word)
		line += len(word)
		if line > MaxLine {
			return "", ErrInvalid
		}
		start = end + 1
	}
	out.WriteString("\r\n")
	return out.String(), nil
}

func isBlank(c byte) bool { return c == ' ' || c == '\t' }
