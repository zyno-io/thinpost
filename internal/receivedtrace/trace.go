// Package receivedtrace renders the one Received trace field Thinpost adds to
// every message it prepares. The field names the Thinpost host, the way the
// message arrived and the message ID, and never the client: no address and no
// client host name, because those describe the sender's network.
package receivedtrace

import (
	"bytes"
	"errors"
	"strings"
	"time"
)

// The "with" protocol keywords. Submission over SMTP always requires TLS before
// AUTH, so it is ESMTPSA. The HTTP APIs are served by the control plane behind
// the deployment's own TLS terminator, which the control plane cannot observe,
// so the field says only HTTP.
const (
	WithSMTP = "ESMTPSA"
	WithHTTP = "HTTP"
)

// MaxHostLen is the longest DNS name a field can carry.
const MaxHostLen = 253

// ErrInvalid reports a host, protocol or ID the field cannot carry.
var ErrInvalid = errors.New("invalid received trace input")

// Field returns the folded field, ending in CRLF:
//
//	Received: by <host> (Thinpost)
//	\twith <protocol> id <id>;
//	\t<RFC 5322 date, UTC>
//
// host must be a plain DNS name and id a token without spaces or angle
// brackets. at is the time Thinpost accepted the message.
func Field(host, with, id string, at time.Time) ([]byte, error) {
	if !validHost(host) || (with != WithSMTP && with != WithHTTP) || id == "" || at.IsZero() ||
		strings.ContainsAny(id, " \t\r\n<>@;()") {
		return nil, ErrInvalid
	}
	return []byte("Received: by " + host + " (Thinpost)\r\n\twith " + with + " id " + id + ";\r\n\t" + at.UTC().Format(time.RFC1123Z) + "\r\n"), nil
}

// validHost accepts lowercase or mixed-case LDH labels separated by dots.
func validHost(h string) bool {
	if h == "" || len(h) > MaxHostLen {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// SizingHost is the longest valid host name, used to reserve space before the
// real name is known.
func SizingHost() string {
	l := strings.Repeat("a", 63)
	return l + "." + l + "." + l + "." + strings.Repeat("a", 61)
}

// Valid reports whether b is exactly one Received field, folded with CRLF
// followed by whitespace and ended by a final CRLF.
func Valid(b []byte) bool {
	crlf := []byte("\r\n")
	if !bytes.HasPrefix(b, []byte("Received:")) || !bytes.HasSuffix(b, crlf) {
		return false
	}
	for i, c := range b {
		switch c {
		case '\n':
			if i == 0 || b[i-1] != '\r' || (i+1 < len(b) && b[i+1] != ' ' && b[i+1] != '\t') {
				return false
			}
		case '\r':
			if i+1 >= len(b) || b[i+1] != '\n' {
				return false
			}
		case 0:
			return false
		}
	}
	return true
}
