package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

func TestPostmarkBatchReturnsPerItemOutcomes(t *testing.T) {
	service := &fakeService{app: submission.App{ID: "app-a"}}
	body := `[{"From":"sender@example.com","To":"a@example.net","Subject":"A","TextBody":"body"},{"From":"sender@example.com","To":"b@example.net","Subject":"B","Attachments":[]},{"From":"sender@example.com","To":"c@example.net","Subject":"C","TextBody":"body"}]`
	w := request(t, New(service), "/email/batch", body, map[string]string{"X-Postmark-Server-Token": "valid-key", "X-Thinpost-Enhanced-Protection": "true"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var results []struct {
		ErrorCode int
		MessageID string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[0].ErrorCode != 0 || results[1].ErrorCode == 0 || results[2].ErrorCode != 0 || results[0].MessageID == "" {
		t.Fatal("unexpected per-item outcomes")
	}
	if len(service.requests) != 2 || !service.requests[0].Message.EnhancedProtection || !service.requests[1].Message.EnhancedProtection {
		t.Fatal("batch TLS requirement lost")
	}
	w = request(t, New(service), "/email/batch", "["+strings.Repeat(`{},`, 100)+`{}]`, map[string]string{"X-Postmark-Server-Token": "valid-key"})
	if w.Code != http.StatusBadRequest {
		t.Fatal("oversized batch accepted")
	}
	w = request(t, New(service), "/email/batch", body, map[string]string{"X-Postmark-Server-Token": "wrong"})
	if w.Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated batch accepted")
	}
}
