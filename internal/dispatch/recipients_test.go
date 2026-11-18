package dispatch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zyno-io/thinpost/internal/delivery"

	"github.com/zyno-io/thinpost/internal/submission"
)

func TestDispatchSendsToTheEnvelopeRecipientAtTheDeliveryIndex(t *testing.T) {
	msg := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Cc: []string{"Copy <cc@example.net>"}, Bcc: []string{"bcc@example.net"}, Subject: "hello", Text: "body", EnhancedProtection: true}
	source, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{2, msg})
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []string{"b@example.net", "cc@example.net", "bcc@example.net"} {
		p, r, o, lease := setup(t)
		o.source = source
		r.claim.RecipientIndex = index
		start, _, err := p.Next(context.Background(), lease, delivery.EdgeCapabilities{})
		if err != nil || start.Recipient != want {
			t.Fatalf("index %d: recipient %q, %v", index, start.GetRecipient(), err)
		}
	}
	p, r, o, lease := setup(t)
	o.source = source
	r.claim.RecipientIndex = 3
	if _, _, err = p.Next(context.Background(), lease, delivery.EdgeCapabilities{}); err == nil {
		t.Fatal("an index beyond To, Cc and Bcc must be rejected")
	}
}
