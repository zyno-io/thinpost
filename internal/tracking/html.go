// Package tracking prepares opt-in, per-recipient HTML tracking. It does not
// persist mappings itself; callers commit mappings atomically with deliveries.
package tracking

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const maxHTMLBytes = 2 << 20
const maxLinks = 100
const maxTargetBytes = 2048
const maxNodes = 100000
const MaxOriginBytes = 512

type Mapping struct {
	// Token is a random bearer capability. Store only its hash in the database
	// and never put it or the destination into access logs.
	Token       string
	Kind        string // "open" or "click"
	Destination string // Only for click mappings; protect as message content.
}

// Mappings contain capabilities and potentially sensitive destination URLs.
// Reject accidental JSON serialization and redact common formatting verbs;
// persistence must explicitly protect each field as appropriate.
func (Mapping) String() string   { return "<tracking mapping redacted>" }
func (Mapping) GoString() string { return "<tracking mapping redacted>" }
func (Mapping) MarshalJSON() ([]byte, error) {
	return nil, errors.New("tracking mappings require protected persistence")
}

type Options struct {
	Origin string // Dedicated HTTPS origin without a path, query or fragment.
	Links  *LinkKeys
	Opens  bool
	Clicks bool
}

// Rewrite operates on one recipient's HTML before MIME construction and DKIM
// signing. Only absolute HTTP(S) anchor destinations without URL credentials
// are rewritten. Relative, fragment, mailto and other links are left intact.
// Callers must commit mappings with the recipient's delivery before exposing
// signed MIME. Link resolution itself is independent of that storage. Errors
// never include HTML or destination URLs.
func Rewrite(input string, options Options) (string, []Mapping, error) {
	if !options.Opens && !options.Clicks {
		return input, nil, nil
	}
	if len(input) == 0 || len(input) > maxHTMLBytes || !utf8.ValidString(input) {
		return "", nil, errors.New("invalid tracking HTML")
	}
	origin, err := NormalizeOrigin(options.Origin)
	if err != nil {
		return "", nil, err
	}
	if options.Links == nil {
		return "", nil, errors.New("tracking link signing is required")
	}
	document, err := html.Parse(strings.NewReader(input))
	if err != nil {
		return "", nil, errors.New("cannot parse tracking HTML")
	}
	type anchor struct {
		node        *html.Node
		attribute   int
		destination string
	}
	var anchors []anchor
	var body *html.Node
	stack := []*html.Node{document}
	for visited := 0; len(stack) > 0; visited++ {
		if visited >= maxNodes {
			return "", nil, errors.New("tracking HTML is too complex")
		}
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if node.Type == html.ElementNode && node.Namespace == "" {
			if node.Data == "body" && body == nil {
				body = node
			}
			if options.Clicks && node.Data == "a" {
				index, count := 0, 0
				for i, attribute := range node.Attr {
					if attribute.Namespace == "" && strings.EqualFold(attribute.Key, "href") {
						index, count = i, count+1
					}
				}
				if count == 1 && len(node.Attr[index].Val) > maxTargetBytes && isHTTPPrefix(node.Attr[index].Val) {
					return "", nil, errors.New("tracking link is too long")
				}
				if count == 1 && eligibleDestination(node.Attr[index].Val) {
					if len(anchors) >= maxLinks {
						return "", nil, errors.New("too many trackable links")
					}
					anchors = append(anchors, anchor{node: node, attribute: index, destination: node.Attr[index].Val})
				}
			}
		}
		// Push in reverse to preserve document order in the returned mappings.
		for child := node.LastChild; child != nil; child = child.PrevSibling {
			stack = append(stack, child)
		}
	}
	if options.Opens && body == nil {
		return "", nil, errors.New("tracking HTML has no body")
	}
	mappings := make([]Mapping, 0, len(anchors)+1)
	seen := make(map[string]bool)
	issue := func(kind, destination string) (string, error) {
		token, err := randomToken()
		if err != nil || seen[token] {
			return "", errors.New("cannot create tracking token")
		}
		seen[token] = true
		mappings = append(mappings, Mapping{Token: token, Kind: kind, Destination: destination})
		return options.Links.link(origin, kind, token, destination)
	}
	for _, entry := range anchors {
		link, err := issue("click", entry.destination)
		if err != nil {
			return "", nil, err
		}
		entry.node.Attr[entry.attribute].Val = link
	}
	if options.Opens {
		pixel, err := issue("open", "")
		if err != nil {
			return "", nil, err
		}
		body.AppendChild(&html.Node{Type: html.ElementNode, Data: "img", Attr: []html.Attribute{
			{Key: "src", Val: pixel}, {Key: "width", Val: "1"}, {Key: "height", Val: "1"},
			{Key: "alt", Val: ""}, {Key: "style", Val: "display:none!important"},
		}})
	}
	var output bytes.Buffer
	if err = html.Render(&output, document); err != nil || output.Len() > 4*maxHTMLBytes {
		return "", nil, errors.New("cannot render tracking HTML")
	}
	return output.String(), mappings, nil
}

// NormalizeOrigin validates the same HTTPS origin used by message rewriting.
// The host must be ASCII letters, digits, '-' and '.', with an optional numeric
// port, and an IDN must be given in punycode (xn--) form. Case is preserved
// because the origin is only the URL prefix of each link; links are not bound
// to it, so links already sent keep working wherever their hostname still
// routes to this deployment.
// Such an origin needs no HTML or quoted-printable escaping in a message, which
// lets the acceptance preflight measure a maximum-length placeholder origin as
// an upper bound on a real one. IP literals with colons (IPv6) are rejected.
func NormalizeOrigin(value string) (string, error) {
	if len(value) > MaxOriginBytes || hasControl(value) {
		return "", errors.New("invalid tracking origin")
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return "", errors.New("invalid tracking origin")
	}
	host, port, hasPort := strings.Cut(u.Host, ":")
	if !plainHost(host) || hasPort && (port == "" || len(port) > 5 || strings.Trim(port, "0123456789") != "") {
		return "", errors.New("invalid tracking origin")
	}
	if hasPort {
		host += ":" + port
	}
	return "https://" + host, nil
}

func plainHost(host string) bool {
	if host == "" || host[0] == '-' || host[0] == '.' {
		return false
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func eligibleDestination(value string) bool {
	if len(value) == 0 || len(value) > maxTargetBytes || hasControl(value) {
		return false
	}
	u, err := url.Parse(value)
	return err == nil && (strings.EqualFold(u.Scheme, "https") || strings.EqualFold(u.Scheme, "http")) && u.Hostname() != "" && u.User == nil && u.Opaque == ""
}

func isHTTPPrefix(value string) bool {
	value = strings.ToLower(value)
	return strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://")
}

func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func randomToken() (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
