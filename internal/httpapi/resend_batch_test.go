package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

type batchFake struct {
	fakeService
	batch      submission.BatchRequest
	batchApp   submission.App
	batchCalls int
	receipt    submission.BatchReceipt
	err        error
}

func (f *batchFake) AcceptBatch(_ context.Context, app submission.App, req submission.BatchRequest) (submission.BatchReceipt, error) {
	f.batchCalls++
	f.batch, f.batchApp = req, app
	return f.receipt, f.err
}

const validResendBatch = `[{"from":"a@example.com","to":"one@example.net","subject":"one","text":"first"},{"from":"a@example.com","to":["two@example.net"],"subject":"two","html":"<b>second</b>"}]`

func TestResendBatchUsesOneAtomicCallAndOrderedResponse(t *testing.T) {
	f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app-a", KeyID: "key-a"}}, receipt: submission.BatchReceipt{IDs: []string{"first-id", "second-id"}}}
	w := request(t, New(f), "/emails/batch", validResendBatch, map[string]string{"Authorization": "Bearer valid-key", "Idempotency-Key": "batch-key", "X-Thinpost-Enhanced-Protection": "true"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if f.batchCalls != 1 || len(f.requests) != 0 || f.batchApp.ID != "app-a" || f.batch.Adapter != "resend" || f.batch.IdempotencyKey != "batch-key" || len(f.batch.Messages) != 2 || !f.batch.Messages[0].EnhancedProtection || !f.batch.Messages[1].EnhancedProtection {
		t.Fatal("batch was not passed atomically with its policy")
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Data) != 2 || body.Data[0].ID != "first-id" || body.Data[1].ID != "second-id" {
		t.Fatal("batch response did not preserve order", err)
	}
}

func TestResendBatchRejectsWholeInvalidRequest(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		headers    map[string]string
	}{
		{"empty", `[]`, nil},
		{"malformed second message", `[{"from":"a@example.com","to":"one@example.net","subject":"one","text":"x"},{"from":"a@example.com","to":"two@example.net","subject":"two","attachments":[]}]`, nil},
		{"invalid TLS", validResendBatch, map[string]string{"X-Thinpost-Enhanced-Protection": "sometimes"}},
		{"empty idempotency key", validResendBatch, map[string]string{"Idempotency-Key": ""}},
		{"too many messages", `[` + strings.Repeat(`{},`, 100) + `{}` + `]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &batchFake{fakeService: fakeService{app: submission.App{ID: "a"}}}
			headers := map[string]string{"Authorization": "Bearer valid-key"}
			for k, v := range tc.headers {
				headers[k] = v
			}
			w := request(t, New(f), "/emails/batch", tc.body, headers)
			if w.Code != http.StatusBadRequest || f.batchCalls != 0 || len(f.requests) != 0 {
				t.Fatalf("invalid batch was processed: status %d", w.Code)
			}
		})
	}
	f := &batchFake{fakeService: fakeService{app: submission.App{ID: "a"}}}
	r := httptest.NewRequest(http.MethodPost, "/emails/batch", strings.NewReader(validResendBatch))
	r.Header.Set("Authorization", "Bearer valid-key")
	r.Header.Add("Idempotency-Key", "one")
	r.Header.Add("Idempotency-Key", "two")
	w := httptest.NewRecorder()
	New(f).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || f.batchCalls != 0 {
		t.Fatal("duplicate idempotency headers were accepted")
	}
	recipients := make([]string, 51)
	for i := range recipients {
		recipients[i] = "recipient@example.net"
	}
	payload, err := json.Marshal([]map[string]any{{"from": "a@example.com", "to": recipients, "subject": "one", "text": "body"}})
	if err != nil {
		t.Fatal(err)
	}
	w = request(t, New(f), "/emails/batch", string(payload), map[string]string{"Authorization": "Bearer valid-key"})
	if w.Code != http.StatusBadRequest || f.batchCalls != 0 {
		t.Fatal("batch accepted more than 50 recipients in one message")
	}
}

func TestResendBatchRequiresAtomicServiceAndMapsFailures(t *testing.T) {
	headers := map[string]string{"Authorization": "Bearer valid-key"}
	legacy := &fakeService{app: submission.App{ID: "a"}}
	if w := request(t, New(legacy), "/emails/batch", validResendBatch, headers); w.Code != http.StatusServiceUnavailable || len(legacy.requests) != 0 {
		t.Fatal("batch fell back to individual submissions")
	}
	for _, tc := range []struct {
		err  error
		code int
	}{{submission.ErrIdempotencyConflict, http.StatusConflict}, {submission.ErrQuotaExceeded, http.StatusTooManyRequests}, {submission.ErrRateLimited, http.StatusTooManyRequests}, {submission.ErrInvalidMessage, http.StatusBadRequest}, {errors.New("database unavailable"), http.StatusServiceUnavailable}} {
		f := &batchFake{fakeService: fakeService{app: submission.App{ID: "a"}}, err: tc.err}
		if w := request(t, New(f), "/emails/batch", validResendBatch, headers); w.Code != tc.code {
			t.Fatalf("error %v returned %d", tc.err, w.Code)
		}
	}
	f := &batchFake{fakeService: fakeService{app: submission.App{ID: "a"}}, receipt: submission.BatchReceipt{IDs: []string{"same", "same"}}}
	if w := request(t, New(f), "/emails/batch", validResendBatch, headers); w.Code != http.StatusServiceUnavailable {
		t.Fatal("accepted invalid atomic batch response")
	}
}
