package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/zyno-io/thinpost/internal/submission"
)

// Header values may contain addresses or tokens: errors here never include
// them. Only List-Unsubscribe, List-Unsubscribe-Post, In-Reply-To, References
// and Reply-To are accepted from apps. Cc and Bcc are never accepted as
// headers: they would add recipients (or, for Bcc, reveal them).
type headerPair struct{ name, value string }

// maxPassthroughHeaders is the number of distinct accepted header names.
const maxPassthroughHeaders = 5

// isPassthroughHeader reports whether name (any case) is an accepted app header.
func isPassthroughHeader(name string) bool {
	switch asciiLower(name) {
	case "list-unsubscribe", "list-unsubscribe-post", "in-reply-to", "references", "reply-to":
		return true
	}
	return false
}

// postmarkHeaders decodes Postmark's Headers:[{Name,Value}] array.
func postmarkHeaders(raw []byte) ([]headerPair, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, submission.ErrInvalidMessage
	}
	var items []struct {
		Name  *string `json:"Name"`
		Value *string `json:"Value"`
	}
	if err := decodeStrictList(raw, &items); err != nil || len(items) > maxPassthroughHeaders {
		return nil, submission.ErrInvalidMessage
	}
	pairs := make([]headerPair, 0, len(items))
	for _, item := range items {
		if item.Name == nil || item.Value == nil {
			return nil, submission.ErrInvalidMessage
		}
		pairs = append(pairs, headerPair{*item.Name, *item.Value})
	}
	return pairs, nil
}

// objectHeaders decodes the Resend and SendGrid headers object of strings.
func objectHeaders(raw []byte) ([]headerPair, error) {
	var values map[string]string
	if err := decodeStrictObject(raw, &values); err != nil || len(values) > maxPassthroughHeaders {
		return nil, submission.ErrInvalidMessage
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]headerPair, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, headerPair{name, values[name]})
	}
	return pairs, nil
}

// asciiLower lowercases an ASCII header name. A name with any non-ASCII byte
// returns "", which matches no allowed header: Unicode folding would map, for
// example, U+0130 to "i".
func asciiLower(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			return ""
		}
	}
	return strings.ToLower(name)
}

// applyPassthroughHeaders accepts exactly List-Unsubscribe,
// List-Unsubscribe-Post, In-Reply-To, References and Reply-To
// (case-insensitive, no duplicates). A Reply-To header together with the
// provider's own reply field is rejected: adapters set that field first. URI validation happens in submission.Message.Validate; threading
// values are normalized here so the stored and fingerprinted form is canonical.
func applyPassthroughHeaders(msg *submission.Message, pairs []headerPair) error {
	var haveList, havePost, haveInReplyTo, haveRefs, haveReplyTo bool
	for _, pair := range pairs {
		switch asciiLower(pair.name) {
		case "list-unsubscribe":
			if haveList || pair.value == "" {
				return submission.ErrInvalidMessage
			}
			haveList = true
			msg.ListUnsubscribe = pair.value
		case "list-unsubscribe-post":
			if havePost || pair.value != submission.ListUnsubscribePostValue {
				return submission.ErrInvalidMessage
			}
			havePost = true
			msg.ListUnsubscribePost = true
		case "in-reply-to":
			value, err := submission.NormalizeInReplyTo(pair.value)
			if haveInReplyTo || err != nil {
				return submission.ErrInvalidMessage
			}
			haveInReplyTo = true
			msg.InReplyTo = value
		case "references":
			value, err := submission.NormalizeReferences(pair.value)
			if haveRefs || err != nil {
				return submission.ErrInvalidMessage
			}
			haveRefs = true
			msg.References = value
		case "reply-to":
			// An empty value means no reply address (callers send replyTo || ''); it
			// still counts as the header being present for duplicate detection.
			addresses, err := parseAddressList(pair.value)
			if haveReplyTo || err != nil || len(msg.ReplyTo) > 0 && len(addresses) > 0 {
				return submission.ErrInvalidMessage
			}
			haveReplyTo = true
			if len(addresses) > 0 {
				msg.ReplyTo = addresses
			}
		default:
			return submission.ErrInvalidMessage
		}
	}
	if havePost && !haveList {
		return submission.ErrInvalidMessage
	}
	return nil
}

func decodeStrictList(data []byte, target any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' || rejectAmbiguousJSONFields(data) != nil {
		return submission.ErrInvalidMessage
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return submission.ErrInvalidMessage
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return submission.ErrInvalidMessage
	}
	return nil
}
