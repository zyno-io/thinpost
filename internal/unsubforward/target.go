// Package unsubforward forwards a recipient's one-click unsubscribe to the
// sender's own List-Unsubscribe link. Thinpost replaces a message's own
// List-Unsubscribe headers with its per-delivery link when an app has one-click
// on; this package decides, from the original headers, whether the sender's
// link can be called (RFC 8058) and delivers that call from the control plane
// through the same public-destination checks as webhooks.
//
// Nothing here logs. A link can carry a per-recipient token from the sender, so
// it is treated like recipient data: never in an error, a log line or an API
// response (only its host is shown).
package unsubforward

import (
	"net/url"
	"strings"

	"github.com/zyno-io/thinpost/internal/admin"
)

// OneClickPost is the only List-Unsubscribe-Post value (RFC 8058).
const OneClickPost = "List-Unsubscribe=One-Click"

// Reasons a link is not forwarded. They are stored and shown as is.
const (
	// ReasonMailtoOnly: the header has no https link (a mailto link, which is
	// not called).
	ReasonMailtoOnly = "mailto_only"
	// ReasonNoOneClick: an https link, but no List-Unsubscribe-Post header that
	// says it takes a one-click POST.
	ReasonNoOneClick = "no_one_click_post"
	// ReasonUnusable: the header is malformed, too long, not ASCII, names a
	// link that is not a plain public https address, or the fields repeat.
	ReasonUnusable = "unusable_link"
)

const (
	maxValue = 4096
	maxItems = 8
	maxDepth = 4
	maxURI   = 2048
)

// Original is what a message's own headers said. For raw MIME the counts are
// the number of fields of that name; for an API message, 1 when set.
type Original struct {
	ListFields int
	Value      string
	PostFields int
	PostValue  string
}

// Present reports whether the message had any List-Unsubscribe field of its own.
func (o Original) Present() bool { return o.ListFields > 0 || o.PostFields > 0 }

// Target is a link that can be called.
type Target struct {
	// URL is the canonical https URL (lowercase host, no explicit port 443).
	URL string
	// Host is the DNS name from URL, for display.
	Host string
}

// Decide reports the link to call, or a reason it is not forwarded. Exactly one
// of the two is set; call it only when o.Present().
func Decide(o Original) (Target, string) {
	if o.ListFields != 1 || o.PostFields > 1 {
		return Target{}, ReasonUnusable
	}
	uris, ok := parseList(o.Value)
	if !ok {
		return Target{}, ReasonUnusable
	}
	first, mailto := "", false
	for _, uri := range uris {
		switch {
		case hasScheme(uri, "https:"):
			if first == "" {
				first = uri
			}
		case hasScheme(uri, "mailto:"):
			mailto = true
		}
	}
	if first == "" {
		if mailto {
			return Target{}, ReasonMailtoOnly
		}
		return Target{}, ReasonUnusable
	}
	if o.PostFields != 1 || strings.TrimSpace(o.PostValue) != OneClickPost {
		return Target{}, ReasonNoOneClick
	}
	// The same rules as a webhook endpoint: https, a public-looking DNS name (no
	// IP literal), no userinfo or fragment, port 443 only.
	canonical, ok := admin.NormalizeWebhookEndpoint(first)
	if !ok {
		return Target{}, ReasonUnusable
	}
	u, err := url.Parse(canonical)
	if err != nil || u.Hostname() == "" {
		return Target{}, ReasonUnusable
	}
	return Target{URL: canonical, Host: u.Hostname()}, ""
}

func hasScheme(uri, scheme string) bool {
	return len(uri) > len(scheme) && strings.EqualFold(uri[:len(scheme)], scheme)
}

// parseList reads an RFC 2369 list: one to maxItems "<uri>" items separated by
// commas, with white space and (nested) comments allowed between the tokens. It
// accepts only printable ASCII (and tab), at most maxValue bytes, and returns
// false for anything else, including a trailing comma and text outside the
// brackets.
func parseList(value string) ([]string, bool) {
	if value == "" || len(value) > maxValue {
		return nil, false
	}
	for i := 0; i < len(value); i++ {
		if c := value[i]; c != '\t' && (c < ' ' || c >= 0x7f) {
			return nil, false
		}
	}
	var uris []string
	i := skip(value, 0)
	for {
		if i < 0 || i >= len(value) || value[i] != '<' {
			return nil, false
		}
		end := strings.IndexByte(value[i+1:], '>')
		if end < 1 {
			return nil, false
		}
		uri := value[i+1 : i+1+end]
		if len(uri) > maxURI || strings.ContainsAny(uri, "< \t") {
			return nil, false
		}
		if uris = append(uris, uri); len(uris) > maxItems {
			return nil, false
		}
		i = skip(value, i+end+2)
		switch {
		case i < 0:
			return nil, false
		case i == len(value):
			return uris, true
		case value[i] != ',':
			return nil, false
		}
		i = skip(value, i+1)
	}
}

// skip returns the index of the next byte that is not white space or part of a
// comment, or -1 for a comment that is unbalanced or nested too deeply.
func skip(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t':
			i++
		case '(':
			depth := 0
			for ; i < len(s); i++ {
				switch s[i] {
				case '\\':
					i++
				case '(':
					if depth++; depth > maxDepth {
						return -1
					}
				case ')':
					depth--
				}
				if depth == 0 {
					break
				}
			}
			if depth != 0 || i >= len(s) {
				return -1
			}
			i++
		default:
			return i
		}
	}
	return i
}
