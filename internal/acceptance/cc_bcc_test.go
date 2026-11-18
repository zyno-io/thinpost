package acceptance

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"reflect"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

type denyAddress string

func (d denyAddress) Allows(value string) bool {
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address != string(d)
}

func ccRequest() submission.Request {
	req := validRequest()
	req.IdempotencyKey = ""
	req.Message.EnhancedProtection = false
	req.Message.To = []string{"to1@example.net", "to2@example.net"}
	req.Message.Cc = []string{"Copy <cc@example.net>"}
	req.Message.Bcc = []string{"bcc@example.net"}
	return req
}

func TestAcceptRecordsEnvelopeKindsInOrderAndWritesVersion2(t *testing.T) {
	repo := &memoryRepository{}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	if _, err := service.Accept(context.Background(), submission.App{ID: "app"}, ccRequest()); err != nil {
		t.Fatal(err)
	}
	record := repo.records[0]
	if record.RecipientCount != 4 || !reflect.DeepEqual(record.Recipients, []string{"to1@example.net", "to2@example.net", "cc@example.net", "bcc@example.net"}) || !reflect.DeepEqual(record.RecipientKinds, []string{"to", "to", "cc", "bcc"}) {
		t.Fatalf("count %d recipients %v kinds %v", record.RecipientCount, record.Recipients, record.RecipientKinds)
	}
	source, err := submission.DecodeSource(objects.objects[record.ObjectKey], false)
	if err != nil || source.Version != submission.SourceVersion || len(source.Message.Cc) != 1 || len(source.Message.Bcc) != 1 {
		t.Fatal("stored source", err)
	}
}

func TestRecipientPolicyCoversCcAndBcc(t *testing.T) {
	for _, denied := range []string{"cc@example.net", "bcc@example.net"} {
		repo := &memoryRepository{}
		objects := &memoryObjects{}
		service, err := New(repo, objects, []byte("0123456789abcdef0123456789abcdef"), denyAddress(denied))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = service.Accept(context.Background(), submission.App{ID: "app"}, ccRequest()); !errors.Is(err, submission.ErrInvalidMessage) || len(objects.objects) != 0 || len(repo.records) != 0 {
			t.Fatalf("%s accepted (%v)", denied, err)
		}
	}
}

func TestAllSuppressedCountsCcAndBcc(t *testing.T) {
	repo := &suppressionRepository{suppressed: map[string]bool{"to1@example.net": true, "to2@example.net": true, "cc@example.net": true}}
	service := testService(t, repo, &memoryObjects{})
	req := ccRequest()
	req.RejectAllSuppressed = true
	// The Bcc recipient is not suppressed, so the message is accepted.
	if _, err := service.Accept(context.Background(), submission.App{ID: "app"}, req); err != nil {
		t.Fatal(err)
	}
	repo.suppressed["bcc@example.net"] = true
	if _, err := service.Accept(context.Background(), submission.App{ID: "app"}, req); !errors.Is(err, submission.ErrAllRecipientsSuppressed) {
		t.Fatalf("want all-suppressed, got %v", err)
	}
}

func TestPreflightMeasuresTheRecipientHeaders(t *testing.T) {
	repo := &memoryRepository{}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	req := ccRequest()
	req.Message.To, req.Message.Cc, req.Message.Bcc = nil, nil, nil
	for i := 0; i < 100; i++ {
		req.Message.To = append(req.Message.To, fmt.Sprintf("\"%s\" <u%d@example.net>", strings.Repeat("n", 190), i))
	}
	if _, err := service.Accept(context.Background(), submission.App{ID: "app"}, req); !errors.Is(err, submission.ErrInvalidMessage) || len(objects.objects) != 0 || len(repo.records) != 0 {
		t.Fatal("over-cap recipient headers must be rejected before upload", err)
	}
	// Bcc addresses do not appear in any header, so they do not count.
	req = ccRequest()
	req.Message.To, req.Message.Cc, req.Message.Bcc = []string{"to@example.net"}, nil, nil
	for i := 0; i < 99; i++ {
		req.Message.Bcc = append(req.Message.Bcc, fmt.Sprintf("\"%s\" <u%d@example.org>", strings.Repeat("n", 190), i))
	}
	if _, err := service.Accept(context.Background(), submission.App{ID: "app"}, req); err != nil {
		t.Fatal("Bcc size must not count against the recipient headers", err)
	}
}

func TestBatchRecordsKindsAndCountsWholeEnvelope(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	service, err := NewWithFingerprintKeys(repo, &memoryObjects{}, [][]byte{[]byte(strings.Repeat("a", 32))}, allowRecipients(true))
	if err != nil {
		t.Fatal(err)
	}
	req := submission.BatchRequest{Adapter: "resend", Messages: []submission.Message{ccRequest().Message, validRequest().Message}}
	if _, err := service.AcceptBatch(context.Background(), submission.App{ID: "app"}, req); err != nil {
		t.Fatal(err)
	}
	first := repo.last.Items[0]
	if first.RecipientCount != 4 || !reflect.DeepEqual(first.RecipientKinds, []string{"to", "to", "cc", "bcc"}) {
		t.Fatalf("count %d kinds %v", first.RecipientCount, first.RecipientKinds)
	}
	// 26 To + 25 Cc is over Resend's 50-recipient item cap.
	big := validRequest().Message
	big.To, big.Cc = nil, nil
	for i := 0; i < 26; i++ {
		big.To = append(big.To, fmt.Sprintf("t%d@example.net", i))
	}
	for i := 0; i < 25; i++ {
		big.Cc = append(big.Cc, fmt.Sprintf("c%d@example.net", i))
	}
	if _, err := service.AcceptBatch(context.Background(), submission.App{ID: "app"}, submission.BatchRequest{Adapter: "resend", Messages: []submission.Message{big}}); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("Resend item cap must count the whole envelope", err)
	}
}
