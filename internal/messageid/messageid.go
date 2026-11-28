// Package messageid recognizes the Message-ID headers Thinpost generates and
// extracts message identifiers from In-Reply-To and References values. It is
// the single parser behind ARF feedback correlation and inbound reply
// correlation, so a change to the header shape is made here once.
//
// Both Thinpost forms are `<{32 lowercase hex}@{sender domain}>`. The hex part
// is a delivery ID for a message prepared with per-delivery Message-IDs and a
// message ID for one prepared with the shared Message-ID (schema 38). The form
// cannot be told from the header; the database lookup decides.
package messageid

import (
	"regexp"
	"strings"
)

var thinpost = regexp.MustCompile(`^<([0-9a-f]{32})@([A-Za-z0-9.-]{1,253})>$`)

// ParseThinpost splits a Thinpost-shaped Message-ID into its ID and its
// lowercased sender domain. The whole value must be one bracketed ID.
func ParseThinpost(value string) (id, domain string, ok bool) {
	m := thinpost.FindStringSubmatch(value)
	if m == nil {
		return "", "", false
	}
	return m[1], strings.ToLower(m[2]), true
}

// MaxScanned bounds how many identifiers Extract returns.
const MaxScanned = 20

// Extract returns the bracketed identifiers of an In-Reply-To or References
// value in order of appearance, at most limit of them (MaxScanned when limit is
// not positive). Anything outside angle brackets, comments included, is
// ignored; an identifier longer than 998 bytes or containing white space is
// skipped.
func Extract(value string, limit int) []string {
	if limit <= 0 {
		limit = MaxScanned
	}
	var out []string
	for {
		start := strings.IndexByte(value, '<')
		if start < 0 {
			return out
		}
		end := strings.IndexByte(value[start:], '>')
		if end < 0 {
			return out
		}
		candidate := value[start : start+end+1]
		value = value[start+end+1:]
		// A '<' inside the candidate means the earlier one was unterminated.
		if i := strings.LastIndexByte(candidate, '<'); i > 0 {
			candidate = candidate[i:]
		}
		if len(candidate) > 998 || strings.ContainsAny(candidate, " \t\r\n") {
			continue
		}
		out = append(out, candidate)
		if len(out) >= limit {
			return out
		}
	}
}
