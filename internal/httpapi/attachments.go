package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime"
	"mime/multipart"
	"path/filepath"
	"strings"

	"github.com/zyno-io/thinpost/internal/submission"
)

// The API's encoded request limit applies before decoding. Reject URLs and
// noncanonical encodings instead of fetching remote data or guessing intent.
func decodeAttachments(data []byte, provider string, limit int) ([]submission.Attachment, error) {
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '[' {
		return nil, submission.ErrInvalidMessage
	}
	var entries []json.RawMessage
	if json.Unmarshal(data, &entries) != nil || len(entries) > submission.MaxAttachments {
		return nil, submission.ErrInvalidMessage
	}
	result := make([]submission.Attachment, 0, len(entries))
	for _, entry := range entries {
		var filename, contentType, contentID, disposition string
		var content *string
		switch provider {
		case "postmark":
			var item struct {
				Name          string  `json:"Name"`
				Content       *string `json:"Content"`
				ContentType   string  `json:"ContentType"`
				ContentID     *string `json:"ContentID"`
				ContentLength *int    `json:"ContentLength"`
				Disposition   *string `json:"Disposition"`
			}
			if decodeStrictObject(entry, &item) != nil {
				return nil, submission.ErrInvalidMessage
			}
			filename, content, contentType = item.Name, item.Content, item.ContentType
			if item.ContentID != nil && *item.ContentID != "" {
				if !strings.HasPrefix(*item.ContentID, "cid:") {
					return nil, submission.ErrInvalidMessage
				}
				contentID = strings.TrimPrefix(*item.ContentID, "cid:")
				if contentID == "" {
					return nil, submission.ErrInvalidMessage
				}
			}
			if item.Disposition != nil {
				disposition = *item.Disposition
			}
			decoded, err := decodeAttachmentContent(content, limit)
			if err != nil || item.ContentLength != nil && *item.ContentLength != len(decoded) {
				return nil, submission.ErrInvalidMessage
			}
			attachment, err := makeAttachment(filename, contentType, contentID, disposition, decoded)
			if err != nil {
				return nil, err
			}
			result = append(result, attachment)
			continue
		case "resend":
			var item struct {
				Filename    string  `json:"filename"`
				Content     *string `json:"content"`
				ContentType string  `json:"content_type"`
				ContentID   string  `json:"content_id"`
			}
			if decodeStrictObject(entry, &item) != nil {
				return nil, submission.ErrInvalidMessage
			}
			filename, content, contentType, contentID = item.Filename, item.Content, item.ContentType, item.ContentID
		case "sendgrid":
			var item struct {
				Filename    string  `json:"filename"`
				Content     *string `json:"content"`
				Type        string  `json:"type"`
				Disposition string  `json:"disposition"`
				ContentID   string  `json:"content_id"`
			}
			if decodeStrictObject(entry, &item) != nil {
				return nil, submission.ErrInvalidMessage
			}
			filename, content, contentType, contentID, disposition = item.Filename, item.Content, item.Type, item.ContentID, item.Disposition
		default:
			return nil, submission.ErrInvalidMessage
		}
		decoded, err := decodeAttachmentContent(content, limit)
		if err != nil || provider == "sendgrid" && len(decoded) == 0 {
			return nil, submission.ErrInvalidMessage
		}
		attachment, err := makeAttachment(filename, contentType, contentID, disposition, decoded)
		if err != nil {
			return nil, err
		}
		result = append(result, attachment)
	}
	return result, nil
}

func decodeAttachmentContent(content *string, limit int) ([]byte, error) {
	if content == nil || len(*content) > base64.StdEncoding.EncodedLen(limit) {
		return nil, submission.ErrInvalidMessage
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(*content)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != *content {
		return nil, submission.ErrInvalidMessage
	}
	if decoded == nil {
		decoded = []byte{}
	}
	return decoded, nil
}

func makeAttachment(filename, contentType, contentID, disposition string, content []byte) (submission.Attachment, error) {
	if contentType == "" {
		contentType, _, _ = mime.ParseMediaType(mime.TypeByExtension(filepath.Ext(filename)))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}
	inline := contentID != ""
	if disposition != "" && disposition != "inline" && disposition != "attachment" || disposition == "inline" && !inline || disposition == "attachment" && inline {
		return submission.Attachment{}, submission.ErrInvalidMessage
	}
	return submission.Attachment{Filename: filename, ContentType: contentType, Content: content, ContentID: contentID, Inline: inline}, nil
}

func mailgunAttachment(part *multipart.Part, content []byte) (submission.Attachment, error) {
	if part.Header.Get("Content-Transfer-Encoding") != "" || len(part.Header.Values("Content-Disposition")) != 1 || len(part.Header.Values("Content-Type")) > 1 {
		return submission.Attachment{}, submission.ErrInvalidMessage
	}
	disposition, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil || disposition != "form-data" || params["filename"] == "" || len(params) != 2 {
		return submission.Attachment{}, submission.ErrInvalidMessage
	}
	contentID := ""
	if part.FormName() == "inline" {
		contentID = params["filename"]
	}
	return makeAttachment(params["filename"], part.Header.Get("Content-Type"), contentID, "", content)
}
