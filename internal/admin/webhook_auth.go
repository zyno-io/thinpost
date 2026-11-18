package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/textproto"
	"strings"
	"unicode/utf8"
)

const redactedAuth = "<webhook auth redacted>"

// MaxWebhookHeaders bounds custom request headers per endpoint.
const MaxWebhookHeaders = 5

// WebhookHeader is one custom request header. It is write-only: it formats,
// stringifies and marshals redacted, so a log line, error or API response
// cannot expose the value. Seal and OpenWebhookAuth are the only ways to
// serialize it.
type WebhookHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (WebhookHeader) String() string               { return redactedAuth }
func (WebhookHeader) GoString() string             { return redactedAuth }
func (WebhookHeader) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redactedAuth)) }
func (WebhookHeader) MarshalJSON() ([]byte, error) { return json.Marshal(redactedAuth) }

// WebhookAuth is optional HTTP authentication for deliveries: HTTP Basic
// credentials and up to five custom headers. Its values sit behind a pointer
// to an unexported struct, so reflection-based printers (fmt on an unexported
// field, slog) show an address, never a secret. It also formats, stringifies
// and marshals redacted. The zero value means no authentication.
type WebhookAuth struct{ v *webhookAuthValues }

type webhookAuthValues struct {
	username, password string
	headers            []WebhookHeader
}

// NewWebhookAuth copies its arguments. Empty arguments give the zero value.
func NewWebhookAuth(username, password string, headers []WebhookHeader) WebhookAuth {
	if username == "" && password == "" && len(headers) == 0 {
		return WebhookAuth{}
	}
	return WebhookAuth{&webhookAuthValues{username, password, append([]WebhookHeader(nil), headers...)}}
}

func (a WebhookAuth) Username() string {
	if a.v == nil {
		return ""
	}
	return a.v.username
}

func (a WebhookAuth) Password() string {
	if a.v == nil {
		return ""
	}
	return a.v.password
}

// Headers returns a copy of the custom headers.
func (a WebhookAuth) Headers() []WebhookHeader {
	if a.v == nil {
		return nil
	}
	return append([]WebhookHeader(nil), a.v.headers...)
}

func (WebhookAuth) String() string               { return redactedAuth }
func (WebhookAuth) GoString() string             { return redactedAuth }
func (WebhookAuth) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redactedAuth)) }
func (WebhookAuth) MarshalJSON() ([]byte, error) { return json.Marshal(redactedAuth) }

// UnmarshalJSON reads the API's write-only wire form,
// {"username","password","headers":[{"name","value"}]}, rejecting unknown
// fields. An empty object gives the zero value, which removes authentication.
func (a *WebhookAuth) UnmarshalJSON(data []byte) error {
	var p sealed
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&p) != nil {
		return ErrInvalidInput
	}
	var headers []WebhookHeader
	for _, h := range p.Headers {
		headers = append(headers, WebhookHeader(h))
	}
	*a = NewWebhookAuth(p.Username, p.Password, headers)
	return nil
}

// IsZero reports an empty value, which means no authentication.
func (a WebhookAuth) IsZero() bool { return a.v == nil }

// sealed mirrors WebhookAuth without redaction. Both directions go through it,
// so the redacting methods above cannot be bypassed by accident.
type sealed struct {
	Username string         `json:"username,omitempty"`
	Password string         `json:"password,omitempty"`
	Headers  []sealedHeader `json:"headers,omitempty"`
}

type sealedHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Seal returns the plaintext to be encrypted for storage.
func (a WebhookAuth) Seal() ([]byte, error) {
	p := sealed{Username: a.Username(), Password: a.Password()}
	for _, h := range a.Headers() {
		p.Headers = append(p.Headers, sealedHeader(h))
	}
	// HTML escaping would inflate <, > and & sixfold past the sealed-size cap.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// OpenWebhookAuth parses the output of Seal. Errors never include the input.
func OpenWebhookAuth(raw []byte) (WebhookAuth, error) {
	var p sealed
	if json.Unmarshal(raw, &p) != nil {
		return WebhookAuth{}, ErrInvalidInput
	}
	var headers []WebhookHeader
	for _, h := range p.Headers {
		headers = append(headers, WebhookHeader(h))
	}
	a := NewWebhookAuth(p.Username, p.Password, headers)
	if !ValidWebhookAuth(a) {
		return WebhookAuth{}, ErrInvalidInput
	}
	return a, nil
}

// deniedHeaders can never be set by an endpoint: they are managed by the
// transport or would let a request be reframed or rerouted.
var deniedHeaders = map[string]bool{
	"Host": true, "Content-Type": true, "Content-Length": true, "Content-Encoding": true,
	"Transfer-Encoding": true, "Connection": true, "Keep-Alive": true, "Upgrade": true,
	"Te": true, "Trailer": true, "Expect": true,
}

// deniedHeaderPrefixes belong to Thinpost and the provider formats' own
// signature and trace headers.
var deniedHeaderPrefixes = []string{"Proxy-", "X-Thinpost-", "Svix-", "Webhook-", "X-Pm-", "X-Twilio-"}

func validToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// ValidWebhookHeader accepts an RFC 9110 token name without underscores, of at most 64 bytes, that is
// not reserved, and a value of 1-1024 printable ASCII bytes. Authorization is
// reserved only when Basic credentials are also set.
func ValidWebhookHeader(h WebhookHeader, basicSet bool) bool {
	// Underscores are valid token characters but are dropped or rewritten by
	// many servers and proxies, which makes the header ambiguous.
	if len(h.Name) > 64 || !validToken(h.Name) || strings.ContainsRune(h.Name, '_') || len(h.Value) < 1 || len(h.Value) > 1024 || !printableASCII(h.Value) {
		return false
	}
	name := textproto.CanonicalMIMEHeaderKey(h.Name)
	if deniedHeaders[name] || basicSet && name == "Authorization" {
		return false
	}
	for _, prefix := range deniedHeaderPrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

func noControls(s string) bool {
	for _, c := range s {
		if c < 0x20 || c == 0x7f || c >= 0x80 && c < 0xa0 {
			return false
		}
	}
	return true
}

// ValidWebhookAuth accepts the empty value, or Basic credentials given
// together (user name 1-256 bytes without ':', password 1-256 bytes, no control
// characters) and/or up to five valid custom headers with unique names.
func ValidWebhookAuth(a WebhookAuth) bool {
	username, password, headers := a.Username(), a.Password(), a.Headers()
	if (username == "") != (password == "") {
		return false
	}
	basic := username != ""
	if basic {
		if len(username) > 256 || len(password) > 256 || !utf8.ValidString(username) || !utf8.ValidString(password) ||
			strings.ContainsRune(username, ':') || !noControls(username) || !noControls(password) {
			return false
		}
	}
	if len(headers) > MaxWebhookHeaders {
		return false
	}
	seen := make(map[string]bool, len(headers))
	for _, h := range headers {
		if !ValidWebhookHeader(h, basic) {
			return false
		}
		name := textproto.CanonicalMIMEHeaderKey(h.Name)
		if seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}
