package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/submission"
)

func validDomain(domain string) bool {
	if len(domain) == 0 || len(domain) > 253 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

type gridAddress struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

type sendGridInput struct {
	From             gridAddress    `json:"from"`
	ReplyTo          *gridAddress   `json:"reply_to"`
	ReplyToList      *[]gridAddress `json:"reply_to_list"`
	Personalizations []struct {
		To      []gridAddress   `json:"to"`
		Cc      *[]gridAddress  `json:"cc"`
		Bcc     *[]gridAddress  `json:"bcc"`
		From    json.RawMessage `json:"from"`
		Subject json.RawMessage `json:"subject"`
	} `json:"personalizations"`
	Subject string `json:"subject"`
	Content []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"content"`
	TrackingSettings json.RawMessage `json:"tracking_settings"`
	Attachments      json.RawMessage `json:"attachments"`
	Headers          json.RawMessage `json:"headers"`
}

func (a gridAddress) String() string { return (&mail.Address{Name: a.Name, Address: a.Email}).String() }
func decodeSendGrid(r *http.Request, lim limits) ([]submission.Message, error) {
	var input sendGridInput
	if err := decodeJSON(r, &input, lim); err != nil {
		return nil, err
	}
	if len(input.Personalizations) == 0 || len(input.Personalizations) > 100 {
		return nil, submission.ErrInvalidMessage
	}
	if _, err := mail.ParseAddress(input.From.String()); err != nil {
		return nil, submission.ErrInvalidMessage
	}
	msg := submission.Message{From: input.From.String(), Subject: input.Subject}
	if input.ReplyTo != nil && input.ReplyToList != nil {
		return nil, submission.ErrInvalidMessage
	}
	if input.ReplyTo != nil {
		msg.ReplyTo = append(msg.ReplyTo, input.ReplyTo.String())
	}
	if input.ReplyToList != nil {
		if len(*input.ReplyToList) == 0 {
			return nil, submission.ErrInvalidMessage
		}
		for _, address := range *input.ReplyToList {
			msg.ReplyTo = append(msg.ReplyTo, address.String())
		}
	}
	seen := make(map[string]bool)
	for _, content := range input.Content {
		if seen[content.Type] {
			return nil, submission.ErrInvalidMessage
		}
		seen[content.Type] = true
		switch content.Type {
		case "text/plain":
			msg.Text = content.Value
		case "text/html":
			msg.HTML = content.Value
		default:
			return nil, submission.ErrInvalidMessage
		}
	}
	if len(input.Headers) > 0 {
		pairs, err := objectHeaders(input.Headers)
		if err == nil {
			err = applyPassthroughHeaders(&msg, pairs)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(input.TrackingSettings) > 0 {
		opens, clicks, err := decodeSendGridTracking(input.TrackingSettings)
		if err != nil {
			return nil, err
		}
		msg.RequestTracking(opens, clicks)
	}
	if len(input.Attachments) > 0 {
		var err error
		msg.Attachments, err = decodeAttachments(input.Attachments, "sendgrid", lim.attachment)
		if err != nil {
			return nil, err
		}
	}
	messages := make([]submission.Message, 0, len(input.Personalizations))
	// Personalizations copy msg, so they share its Tracking and TrackingOff
	// pointers: nothing may mutate those options in place.
	for _, personalization := range input.Personalizations {
		item := msg
		if len(personalization.From) > 0 {
			var from gridAddress
			if err := decodeStrictObject(personalization.From, &from); err != nil {
				return nil, err
			}
			item.From = from.String()
		}
		if len(personalization.Subject) > 0 {
			if bytes.Equal(bytes.TrimSpace(personalization.Subject), []byte("null")) || json.Unmarshal(personalization.Subject, &item.Subject) != nil {
				return nil, submission.ErrInvalidMessage
			}
		}
		for _, address := range personalization.To {
			item.To = append(item.To, address.String())
		}
		for _, list := range []struct {
			addresses *[]gridAddress
			target    *[]string
		}{{personalization.Cc, &item.Cc}, {personalization.Bcc, &item.Bcc}} {
			if list.addresses == nil {
				continue
			}
			// An empty array is rejected rather than stored as an empty list.
			if len(*list.addresses) == 0 {
				return nil, submission.ErrInvalidMessage
			}
			for _, address := range *list.addresses {
				*list.target = append(*list.target, address.String())
			}
		}
		messages = append(messages, item)
	}
	return messages, nil
}

// decodeSendGridTracking returns the per-kind choice: nil when the setting is
// absent (app default), otherwise whether it is enabled.
func decodeSendGridTracking(data []byte) (opens, clicks *bool, err error) {
	var settings struct {
		Clicks json.RawMessage `json:"click_tracking"`
		Opens  json.RawMessage `json:"open_tracking"`
	}
	if err := decodeStrictObject(data, &settings); err != nil {
		return nil, nil, err
	}
	if len(settings.Clicks) > 0 {
		var click struct {
			Enable     json.RawMessage `json:"enable"`
			EnableText json.RawMessage `json:"enable_text"`
		}
		if err := decodeStrictObject(settings.Clicks, &click); err != nil {
			return nil, nil, submission.ErrInvalidMessage
		}
		enabled, boolErr := requiredTrackingBool(click.Enable)
		if boolErr != nil || (enabled && len(click.EnableText) == 0) {
			return nil, nil, submission.ErrInvalidMessage
		}
		clicks = &enabled
		if len(click.EnableText) > 0 {
			textEnabled, textErr := requiredTrackingBool(click.EnableText)
			if textErr != nil || textEnabled {
				return nil, nil, submission.ErrInvalidMessage
			}
		}
	}
	if len(settings.Opens) > 0 {
		var open struct {
			Enable *bool `json:"enable"`
		}
		if err := decodeStrictObject(settings.Opens, &open); err != nil || open.Enable == nil {
			return nil, nil, submission.ErrInvalidMessage
		}
		opens = open.Enable
	}
	return opens, clicks, nil
}

func requiredTrackingBool(data []byte) (bool, error) {
	var value bool
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) || json.Unmarshal(data, &value) != nil {
		return false, submission.ErrInvalidMessage
	}
	return value, nil
}

func decodeStrictObject(data []byte, target any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' || rejectAmbiguousJSONFields(data) != nil {
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

func decodeMailgun(r *http.Request, lim limits) (submission.Message, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(lim.body)+1))
	if err != nil {
		return submission.Message{}, fmt.Errorf("%w: %w", errBodyRead, err)
	}
	if len(data) > lim.body {
		return submission.Message{}, submission.ErrInvalidMessage
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return submission.Message{}, err
	}
	values := url.Values{}
	var attachments []submission.Attachment
	switch mediaType {
	case "application/x-www-form-urlencoded":
		values, err = url.ParseQuery(string(data))
		if err != nil {
			return submission.Message{}, err
		}
	case "multipart/form-data":
		reader := multipart.NewReader(bytes.NewReader(data), params["boundary"])
		for {
			part, partErr := reader.NextRawPart()
			if partErr == io.EOF {
				break
			}
			if partErr != nil {
				return submission.Message{}, partErr
			}
			value, readErr := io.ReadAll(part)
			if readErr != nil {
				return submission.Message{}, readErr
			}
			if part.FormName() == "attachment" || part.FormName() == "inline" {
				attachment, parseErr := mailgunAttachment(part, value)
				if parseErr != nil || len(attachments) >= submission.MaxAttachments {
					return submission.Message{}, submission.ErrInvalidMessage
				}
				attachments = append(attachments, attachment)
			} else {
				if part.FileName() != "" || part.Header.Get("Content-Transfer-Encoding") != "" {
					return submission.Message{}, submission.ErrInvalidMessage
				}
				values.Add(part.FormName(), string(value))
			}
		}
	default:
		return submission.Message{}, submission.ErrInvalidMessage
	}
	var passthrough []headerPair
	for k, v := range values {
		if name := strings.TrimPrefix(k, "h:"); name != k && isPassthroughHeader(name) {
			if len(v) != 1 || !utf8.ValidString(v[0]) {
				return submission.Message{}, submission.ErrInvalidMessage
			}
			passthrough = append(passthrough, headerPair{k[len("h:"):], v[0]})
			delete(values, k)
			continue
		}
		if k != "from" && k != "to" && k != "cc" && k != "bcc" && k != "subject" && k != "text" && k != "html" && k != "o:require-tls" && k != "o:skip-verification" && k != "o:tracking-opens" && k != "o:tracking-clicks" {
			return submission.Message{}, submission.ErrInvalidMessage
		}
		if k != "to" && k != "cc" && k != "bcc" && len(v) != 1 {
			return submission.Message{}, submission.ErrInvalidMessage
		}
		for _, value := range v {
			if !utf8.ValidString(value) {
				return submission.Message{}, submission.ErrInvalidMessage
			}
		}
	}
	var recipients, ccRecipients, bccRecipients []string
	for _, list := range []struct {
		field  string
		target *[]string
	}{{"to", &recipients}, {"cc", &ccRecipients}, {"bcc", &bccRecipients}} {
		for _, value := range values[list.field] {
			if strings.ContainsAny(value, "\r\n") {
				return submission.Message{}, submission.ErrInvalidMessage
			}
			addresses, parseErr := mail.ParseAddressList(value)
			if parseErr != nil {
				return submission.Message{}, parseErr
			}
			for _, address := range addresses {
				*list.target = append(*list.target, address.String())
			}
		}
	}
	enhancedProtection := false
	if raw, ok := values["o:require-tls"]; ok {
		switch strings.ToLower(raw[0]) {
		case "yes", "true":
			enhancedProtection = true
		case "no", "false":
		default:
			return submission.Message{}, submission.ErrInvalidMessage
		}
	}
	if raw, ok := values["o:skip-verification"]; ok {
		switch strings.ToLower(raw[0]) {
		case "no", "false":
		default:
			return submission.Message{}, submission.ErrInvalidMessage
		}
	}
	var opens, clicks *bool
	yes, no := true, false
	if raw, ok := values["o:tracking-opens"]; ok {
		switch strings.ToLower(raw[0]) {
		case "yes":
			opens = &yes
		case "no":
			opens = &no
		default:
			return submission.Message{}, submission.ErrInvalidMessage
		}
	}
	if raw, ok := values["o:tracking-clicks"]; ok {
		switch strings.ToLower(raw[0]) {
		case "htmlonly":
			clicks = &yes
		case "no":
			clicks = &no
		default:
			return submission.Message{}, submission.ErrInvalidMessage
		}
	}
	message := submission.Message{From: values.Get("from"), To: recipients, Cc: ccRecipients, Bcc: bccRecipients, Subject: values.Get("subject"), Text: values.Get("text"), HTML: values.Get("html"), EnhancedProtection: enhancedProtection, Attachments: attachments}
	if len(passthrough) > 0 {
		if err := applyPassthroughHeaders(&message, passthrough); err != nil {
			return submission.Message{}, err
		}
	}
	message.RequestTracking(opens, clicks)
	return message, nil
}
