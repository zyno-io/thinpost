package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

const gridPersonalizations = `{"from":{"email":"sender@example.com"},"subject":"global","personalizations":[{"to":[{"email":"one@example.net"}],"subject":"first"},{"to":[{"email":"two@example.net"}],"from":{"email":"other@example.com"}}],"content":[{"type":"text/plain","value":"body"}]}`

func TestSendGridPersonalizationsUseAtomicBatch(t *testing.T) {
	f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app", KeyID: "key"}}, receipt: submission.BatchReceipt{IDs: []string{"one", "two"}}}
	w := request(t, New(f), "/v3/mail/send", gridPersonalizations, map[string]string{"Authorization": "Bearer valid-key", "X-Thinpost-Enhanced-Protection": "true"})
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 || f.batchCalls != 1 || len(f.requests) != 0 {
		t.Fatal("batch was not accepted atomically", w.Code)
	}
	if f.batch.Adapter != "sendgrid" || f.batch.IdempotencyKey != "" || f.batchApp.ID != "app" || len(f.batch.Messages) != 2 {
		t.Fatal("incorrect batch identity")
	}
	first, second := f.batch.Messages[0], f.batch.Messages[1]
	if first.Subject != "first" || second.Subject != "global" || !strings.Contains(first.From, "sender@example.com") || !strings.Contains(second.From, "other@example.com") || len(first.To) != 1 || !strings.Contains(first.To[0], "one@example.net") || len(second.To) != 1 || !strings.Contains(second.To[0], "two@example.net") || !first.EnhancedProtection || !second.EnhancedProtection {
		t.Fatal("personalization boundaries, overrides or TLS policy were lost")
	}
}

func TestSendGridPersonalizationsEnforceTrackingGate(t *testing.T) {
	body := strings.Replace(gridPersonalizations, `"content":[{"type":"text/plain","value":"body"}]`, `"reply_to":{"email":"reply@example.com"},"content":[{"type":"text/html","value":"<b>body</b>"}],"tracking_settings":{"open_tracking":{"enable":true}},"attachments":[{"filename":"a.txt","content":"eA==","type":"text/plain"}]`, 1)
	for _, options := range []Options{{}} {
		f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}}
		if w := request(t, NewWithOptions(f, options), "/v3/mail/send", body, map[string]string{"Authorization": "Bearer valid-key"}); w.Code != http.StatusBadRequest || f.batchCalls != 0 {
			t.Fatal("batch bypassed a feature gate", options, w.Code)
		}
	}
	f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"one", "two"}}}
	w := request(t, NewWithOptions(f, Options{Tracking: true}), "/v3/mail/send", body, map[string]string{"Authorization": "Bearer valid-key"})
	if w.Code != http.StatusAccepted || f.batchCalls != 1 {
		t.Fatal("enabled batch rejected", w.Code)
	}
	for _, message := range f.batch.Messages {
		if len(message.ReplyTo) != 1 || len(message.Attachments) != 1 || message.Tracking == nil || !message.Tracking.Opens {
			t.Fatal("shared options lost from personalization")
		}
	}
}

func TestSendGridPersonalizationRejectsWholeInvalidRequest(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"invalid second recipient", strings.Replace(gridPersonalizations, `"two@example.net"`, `"invalid"`, 1)},
		{"null subject", strings.Replace(gridPersonalizations, `"subject":"first"`, `"subject":null`, 1)},
		{"null sender", strings.Replace(gridPersonalizations, `"from":{"email":"other@example.com"}`, `"from":null`, 1)},
		{"unknown override", strings.Replace(gridPersonalizations, `"subject":"first"`, `"subject":"first","custom_args":{"x":"y"}`, 1)},
		{"missing root sender", strings.Replace(gridPersonalizations, `"from":{"email":"sender@example.com"},`, ``, 1)},
		{"too many personalizations", `{"from":{"email":"sender@example.com"},"subject":"global","personalizations":[` + strings.Repeat(`{"to":[{"email":"one@example.net"}]},`, 100) + `{"to":[{"email":"one@example.net"}]}],"content":[{"type":"text/plain","value":"body"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}}
			w := request(t, New(f), "/v3/mail/send", test.body, map[string]string{"Authorization": "Bearer valid-key"})
			if w.Code != http.StatusBadRequest || f.batchCalls != 0 || len(f.requests) != 0 {
				t.Fatal("invalid personalization reached acceptance", w.Code)
			}
		})
	}
}

func TestSendGridPersonalizationErrorsNeverPartiallyFallback(t *testing.T) {
	headers := map[string]string{"Authorization": "Bearer valid-key"}
	legacy := &fakeService{app: submission.App{ID: "app"}}
	if w := request(t, New(legacy), "/v3/mail/send", gridPersonalizations, headers); w.Code != http.StatusServiceUnavailable || len(legacy.requests) != 0 {
		t.Fatal("missing batch service used partial fallback")
	}
	for _, test := range []struct {
		err  error
		code int
	}{
		{submission.ErrInvalidKey, 401}, {submission.ErrInvalidMessage, 400}, {submission.ErrQuotaExceeded, 429}, {submission.ErrBacklogExceeded, 429}, {submission.ErrRateLimited, 429}, {errors.New("internal failure"), 503},
	} {
		f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, err: test.err}
		w := request(t, New(f), "/v3/mail/send", gridPersonalizations, headers)
		if w.Code != test.code || f.batchCalls != 1 || len(f.requests) != 0 || strings.Contains(w.Body.String(), "internal failure") {
			t.Fatal("batch error handling", test.err, w.Code)
		}
	}
	for _, ids := range [][]string{nil, {"one"}, {"one", "one"}, {"one", ""}} {
		f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: ids}}
		if w := request(t, New(f), "/v3/mail/send", gridPersonalizations, headers); w.Code != http.StatusServiceUnavailable {
			t.Fatal("malformed receipt accepted", ids, w.Code)
		}
	}
}

func TestSendGridPersonalizationRecipientLimit(t *testing.T) {
	to := `[` + strings.TrimSuffix(strings.Repeat(`{"email":"recipient@example.net"},`, 100), ",") + `]`
	personalization := `{"to":` + to + `}`
	body := `{"from":{"email":"sender@example.com"},"subject":"global","personalizations":[` + strings.TrimSuffix(strings.Repeat(personalization+",", 11), ",") + `],"content":[{"type":"text/plain","value":"body"}]}`
	f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}}
	w := request(t, New(f), "/v3/mail/send", body, map[string]string{"Authorization": "Bearer valid-key"})
	if w.Code != http.StatusBadRequest || f.batchCalls != 0 {
		t.Fatal("over-limit recipient batch reached acceptance", w.Code)
	}
}

func TestSingleSendGridPersonalizationOverride(t *testing.T) {
	f := &fakeService{app: submission.App{ID: "app"}}
	body := `{"from":{"email":"sender@example.com"},"subject":"global","personalizations":[{"to":[{"email":"one@example.net"}],"from":{"email":"other@example.com"},"subject":"override"}],"content":[{"type":"text/plain","value":"body"}]}`
	if w := request(t, New(f), "/v3/mail/send", body, map[string]string{"Authorization": "Bearer valid-key"}); w.Code != http.StatusAccepted || len(f.requests) != 1 || f.requests[0].Message.Subject != "override" || !strings.Contains(f.requests[0].Message.From, "other@example.com") {
		t.Fatal("single personalization override rejected", w.Code)
	}
}
