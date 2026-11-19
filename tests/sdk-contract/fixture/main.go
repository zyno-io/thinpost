// Command fixture is a loopback-only, in-memory adapter fixture. It never
// sends or stores real mail and must not be used as a production server.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/zyno-io/thinpost/internal/httpapi"
	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

// Fixed, public test secrets. They sign nothing real.
var fixtureSecrets = map[string]string{
	format.Thinpost: "tpwh_0123456789abcdef0123456789abcdef0123456789",
	format.Resend:   "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYX",
	format.Mailgun:  "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
}

// webhookCases are the events the fixture can render for a recorded message.
var webhookCases = map[string]format.Event{
	"accepted": {Outcome: "accepted", SMTPCode: 250, DeliveryState: "accepted"},
	"bounced":  {Outcome: "rejected", SMTPCode: 550, DeliveryState: "failed"},
	"opened":   {Outcome: "opened"},
}

type record struct {
	ID             string             `json:"id"`
	Adapter        string             `json:"adapter"`
	Message        submission.Message `json:"message"`
	IdempotencyKey string             `json:"idempotencyKey,omitempty"`
	SendingDomain  string             `json:"sendingDomain,omitempty"`
	Batch          bool               `json:"batch,omitempty"`
	Hold           bool               `json:"hold,omitempty"`
}

type fixture struct {
	mu      sync.Mutex
	records []record
}

// suppressionStore is a fixed in-memory list for the Postmark SDK dump case.
// It honours the reason and origin filters only; writes are not exercised.
type suppressionStore struct {
	items []submission.ProviderSuppression
}

func (st suppressionStore) ListProviderSuppressions(_ context.Context, _ string, f submission.ProviderSuppressionFilter) ([]submission.ProviderSuppression, bool, error) {
	has := func(list []string, v string) bool {
		if len(list) == 0 {
			return true
		}
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}
	var out []submission.ProviderSuppression
	for _, item := range st.items {
		if has(f.Reasons, item.Reason) && has(f.Origins, item.Origin) {
			out = append(out, item)
		}
	}
	return out, false, nil
}
func (suppressionStore) AddProviderSuppressions(context.Context, submission.App, []string) error {
	return nil
}
func (suppressionStore) DeleteProviderSuppressions(_ context.Context, _ submission.App, addresses []string) ([]bool, error) {
	return make([]bool, len(addresses)), nil
}

func (*fixture) Authenticate(_ context.Context, key string) (submission.App, error) {
	if key != "fixture-key" {
		return submission.App{}, submission.ErrInvalidKey
	}
	return submission.App{ID: "fixture-app", KeyID: "fixture-key-id"}, nil
}

func (f *fixture) Accept(_ context.Context, _ submission.App, req submission.Request) (submission.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("%032x", len(f.records)+1)
	f.records = append(f.records, record{ID: id, Adapter: req.Adapter, Message: req.Message, IdempotencyKey: req.IdempotencyKey, SendingDomain: req.SendingDomain, Hold: req.Hold})
	return submission.Receipt{ID: id, AcceptedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}, nil
}

func (f *fixture) AcceptBatch(_ context.Context, _ submission.App, req submission.BatchRequest) (submission.BatchReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(req.Messages))
	for _, message := range req.Messages {
		id := fmt.Sprintf("%032x", len(f.records)+1)
		f.records = append(f.records, record{ID: id, Adapter: req.Adapter, Message: message, IdempotencyKey: req.IdempotencyKey, Batch: true, Hold: req.Hold})
		ids = append(ids, id)
	}
	return submission.BatchReceipt{IDs: ids}, nil
}

// webhook renders the webhook a recorded message would produce, with the same
// format package production uses. record is the ID the adapter returned to the
// SDK, in any provider rendering.
func (f *fixture) webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, ok := providerid.Normalize(r.URL.Query().Get("record"))
	impl, known := format.Lookup(r.URL.Query().Get("format"))
	event, validCase := webhookCases[r.URL.Query().Get("case")]
	if !ok || !known || !validCase {
		http.Error(w, "unknown record, format or case", http.StatusBadRequest)
		return
	}
	var rec *record
	f.mu.Lock()
	for i := range f.records {
		if f.records[i].ID == id {
			rec = &f.records[i]
		}
	}
	f.mu.Unlock()
	if rec == nil {
		http.Error(w, "unknown record", http.StatusNotFound)
		return
	}
	if len(rec.Message.To) == 0 {
		http.Error(w, "record has no recipient", http.StatusInternalServerError)
		return
	}
	from, err := mail.ParseAddress(rec.Message.From)
	to, toErr := mail.ParseAddress(rec.Message.To[0])
	if err != nil || toErr != nil {
		http.Error(w, "unparseable address", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	event.ID, event.MessageID, event.OccurredAt = "evt_"+id[len(id)-8:], id, now
	name := r.URL.Query().Get("format")
	ctx := format.Context{AcceptedAt: now, Subject: rec.Message.Subject, EnhancedProtection: rec.Message.EnhancedProtection,
		SenderDomain: strings.ToLower(from.Address[strings.LastIndexByte(from.Address, '@')+1:]), SenderAddress: from.Address, SenderName: from.Name, Recipients: []string{to.Address}}
	if event.Outcome == "opened" {
		first := true
		ctx.FirstOpen = &first
	}
	req, err := impl.Build(format.Input{Event: event, Context: ctx, AppID: "fixture-app", Secret: []byte(fixtureSecrets[name]), Now: now})
	if err != nil {
		http.Error(w, "cannot build webhook", http.StatusUnprocessableEntity)
		return
	}
	headers := map[string]string{}
	for key, values := range req.Header {
		headers[strings.ToLower(key)] = values[0]
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"headers": headers, "body": string(req.Body), "secret": fixtureSecrets[name], "senderDomain": ctx.SenderDomain})
}

func main() {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		log.Fatal("fixture listener failed")
	}
	f := &fixture{}
	mux := http.NewServeMux()
	mux.Handle("/", httpapi.NewWithOptions(f, httpapi.Options{Tracking: true, Suppressions: suppressionStore{items: []submission.ProviderSuppression{
		{ID: "00000000000000000000000000000001", Address: "bounced@example.net", Reason: "hard_bounce", Origin: "recipient", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{ID: "00000000000000000000000000000002", Address: "manual@example.net", Reason: "manual", Origin: "api", CreatedAt: time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)},
	}}}))
	mux.HandleFunc("/fixture/records", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.records)
	})
	mux.HandleFunc("/fixture/webhook", f.webhook)
	fmt.Printf("http://%s\n", listener.Addr())
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	if err = server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal("fixture server failed")
	}
}
