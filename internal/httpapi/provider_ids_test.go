package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/submission"
)

const hexMessageID = "0123456789abcdef0123456789abcdef"
const secondMessageID = "fedcba9876543210fedcba9876543210"

type hexService struct{ batchFake }

func (*hexService) Accept(context.Context, submission.App, submission.Request) (submission.Receipt, error) {
	return submission.Receipt{ID: hexMessageID, AcceptedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}, nil
}

func newHexService() *hexService {
	s := &hexService{}
	s.app = submission.App{ID: "app-a", KeyID: "key-a"}
	s.receipt = submission.BatchReceipt{IDs: []string{hexMessageID, secondMessageID}}
	return s
}

// The ID a sender receives at submission must be the one webhooks carry, so
// every adapter renders through the shared providerid helpers.
func TestSubmissionResponsesUseProviderIDShapes(t *testing.T) {
	const uuid = "01234567-89ab-cdef-0123-456789abcdef"
	postmark := request(t, New(newHexService()), "/email", `{"From":"Sender <sender@Example.com>","To":"a@example.net","Subject":"Hello","TextBody":"body"}`, map[string]string{"X-Postmark-Server-Token": "valid-key"})
	var pm map[string]any
	if err := json.Unmarshal(postmark.Body.Bytes(), &pm); err != nil || pm["MessageID"] != uuid || pm["MessageID"] != providerid.Postmark(hexMessageID) {
		t.Fatalf("postmark: %s", postmark.Body.String())
	}
	resend := request(t, New(newHexService()), "/emails", `{"from":"sender@example.com","to":"a@example.net","subject":"Hello","text":"body"}`, map[string]string{"Authorization": "Bearer valid-key"})
	var rs map[string]any
	if err := json.Unmarshal(resend.Body.Bytes(), &rs); err != nil || rs["id"] != uuid {
		t.Fatalf("resend: %s", resend.Body.String())
	}
	form := url.Values{"from": {"Sender <sender@Example.com>"}, "to": {"to@example.net"}, "subject": {"hello"}, "text": {"body"}}
	r := httptest.NewRequest(http.MethodPost, "/v3/other.example/messages", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth("api", "valid-key")
	w := httptest.NewRecorder()
	New(newHexService()).ServeHTTP(w, r)
	var mg map[string]any
	// The From domain, lowercased as the repository records it, not the URL domain.
	if err := json.Unmarshal(w.Body.Bytes(), &mg); err != nil || mg["id"] != "<"+hexMessageID+"@example.com>" {
		t.Fatalf("mailgun: %s", w.Body.String())
	}
	if got, ok := providerid.Normalize(mg["id"].(string)); !ok || got != hexMessageID {
		t.Fatal("mailgun id does not normalize to the message id")
	}
	// The response id is the exact Message-ID header every recipient's copy carries.
	if mg["id"] != providerid.MessageIDHeader(hexMessageID, "Example.COM") {
		t.Fatal("mailgun id is not the shared Message-ID header")
	}
}

// SendGrid returns the message ID in X-Message-Id only when the request made
// exactly one message; a multi-personalization batch sends no such header.
func TestSendGridMessageIDHeader(t *testing.T) {
	headers := map[string]string{"Authorization": "Bearer valid-key"}
	single := `{"from":{"email":"sender@example.com"},"subject":"s","personalizations":[{"to":[{"email":"a@example.net"},{"email":"b@example.net"}]}],"content":[{"type":"text/plain","value":"body"}]}`
	w := request(t, New(newHexService()), "/v3/mail/send", single, headers)
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 || w.Header().Get("X-Message-Id") != hexMessageID || len(w.Header().Values("X-Message-Id")) != 1 {
		t.Fatalf("single personalization: %d %q", w.Code, w.Header().Values("X-Message-Id"))
	}
	w = request(t, New(newHexService()), "/v3/mail/send", gridPersonalizations, headers)
	if w.Code != http.StatusAccepted || len(w.Header().Values("X-Message-Id")) != 0 {
		t.Fatalf("multi personalization: %d %q", w.Code, w.Header().Values("X-Message-Id"))
	}
}

func TestBatchResponsesUseProviderIDShapes(t *testing.T) {
	w := request(t, New(newHexService()), "/emails/batch", validResendBatch, map[string]string{"Authorization": "Bearer valid-key"})
	var rs struct{ Data []struct{ ID string } }
	if err := json.Unmarshal(w.Body.Bytes(), &rs); err != nil || len(rs.Data) != 2 || rs.Data[0].ID != "01234567-89ab-cdef-0123-456789abcdef" || rs.Data[1].ID != "fedcba98-7654-3210-fedc-ba9876543210" {
		t.Fatalf("resend batch: %s", w.Body.String())
	}
	w = request(t, New(newHexService()), "/email/batch", `[{"From":"sender@example.com","To":"a@example.net","Subject":"A","TextBody":"body"}]`, map[string]string{"X-Postmark-Server-Token": "valid-key"})
	var pm []struct{ MessageID string }
	if err := json.Unmarshal(w.Body.Bytes(), &pm); err != nil || len(pm) != 1 || pm[0].MessageID != "01234567-89ab-cdef-0123-456789abcdef" {
		t.Fatalf("postmark batch: %s", w.Body.String())
	}
}

// Postmark echoes the request's To string, not a normalized address list.
func TestPostmarkResponseEchoesToAsSubmitted(t *testing.T) {
	headers := map[string]string{"X-Postmark-Server-Token": "valid-key"}
	for _, to := range []string{"user@example.com", "Name <a@example.com>, b@example.com"} {
		body, _ := json.Marshal(map[string]string{"From": "sender@example.com", "To": to, "Subject": "Hello", "TextBody": "body"})
		w := request(t, New(newHexService()), "/email", string(body), headers)
		var single map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &single); err != nil || single["To"] != to {
			t.Fatalf("single To = %v, want %q: %s", single["To"], to, w.Body.String())
		}
		batch, _ := json.Marshal([]map[string]string{{"From": "sender@example.com", "To": to, "Subject": "Hello", "TextBody": "body"}})
		w = request(t, New(newHexService()), "/email/batch", string(batch), headers)
		var items []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil || len(items) != 1 || items[0]["To"] != to {
			t.Fatalf("batch To = %v, want %q: %s", items, to, w.Body.String())
		}
	}
}
