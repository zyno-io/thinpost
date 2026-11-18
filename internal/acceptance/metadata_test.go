package acceptance

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

func TestAcceptRecordsMetadata(t *testing.T) {
	repo := &memoryRepository{}
	service := testService(t, repo, &memoryObjects{})
	req := validRequest()
	req.Message.From = "Sender Name <Sender@Mail.EXAMPLE.com>"
	req.Message.To = []string{"First Person <First@Example.NET>", "second@example.net", "second@example.net"}
	req.Message.Subject = "Invoice é 2024"
	if _, err := service.Accept(context.Background(), submission.App{ID: "app"}, req); err != nil {
		t.Fatal(err)
	}
	record := repo.records[0]
	if record.Subject != req.Message.Subject || record.SenderDomain != "mail.example.com" || record.RecipientCount != 3 {
		t.Fatal("subject, sender domain or count not recorded")
	}
	if record.SenderAddress != "Sender@Mail.EXAMPLE.com" || record.SenderName != "Sender Name" {
		t.Fatal("sender not recorded")
	}
	// Display names are stripped; order, case of the address and duplicates are kept.
	if want := []string{"First@Example.NET", "second@example.net", "second@example.net"}; !reflect.DeepEqual(record.Recipients, want) {
		t.Fatal("recipients differ from envelope addresses")
	}
}

func TestAcceptRejectsUnrecordableSubject(t *testing.T) {
	repo := &memoryRepository{}
	service := testService(t, repo, &memoryObjects{})
	req := validRequest()
	req.Message.Subject = strings.Repeat("a", 999)
	if _, err := service.Accept(context.Background(), submission.App{ID: "app"}, req); err == nil {
		t.Fatal("oversized subject accepted")
	}
	if len(repo.records) != 0 {
		t.Fatal("rejected message reached the repository")
	}
}

func TestAcceptRecordsBareSenderWithoutName(t *testing.T) {
	repo := &memoryRepository{}
	service := testService(t, repo, &memoryObjects{})
	req := validRequest()
	req.Message.From = "Sender@Example.com"
	if _, err := service.Accept(context.Background(), submission.App{ID: "app"}, req); err != nil {
		t.Fatal(err)
	}
	if record := repo.records[0]; record.SenderAddress != "Sender@Example.com" || record.SenderName != "" {
		t.Fatal("bare sender recorded wrongly")
	}
}

func TestSenderMetadataValidation(t *testing.T) {
	long := strings.Repeat("n", 999)
	msg := submission.Message{From: strings.Repeat("a", 990) + "@example.com", To: []string{"b@example.net"}, Subject: "s"}
	if _, err := newRecordedMetadata(msg); err == nil {
		t.Error("oversized address accepted")
	}
	// Display names never reject mail accepted before schema 31: they are
	// cleaned, or not recorded when still out of bounds.
	for from, want := range map[string]string{
		`"` + long + `" <a@example.com>`:                "",
		"=?UTF-8?q?Acme=0D=0ABilling?= <a@example.com>": "Acme  Billing",
		"=?UTF-8?q?Acme=00Billing?= <a@example.com>":    "Acme Billing",
		"=?UTF-8?q?Acme=FFBilling?= <a@example.com>":    "Acme\uFFFDBilling",
		"=?UTF-8?q?=0A?= <a@example.com>":               "",
	} {
		msg := submission.Message{From: from, To: []string{"b@example.net"}, Subject: "s"}
		if m, err := newRecordedMetadata(msg); err != nil || m.SenderName != want || m.SenderAddress != "a@example.com" {
			t.Errorf("%q: got %q, %v", from, m.SenderName, err)
		}
	}
	// RFC 2047 names are decoded before storage.
	msg = submission.Message{From: "=?UTF-8?q?Z=C3=B6e?= <a@example.com>", To: []string{"b@example.net"}, Subject: "s"}
	if m, err := newRecordedMetadata(msg); err != nil || m.SenderName != "Zöe" {
		t.Fatal("encoded name not decoded", err)
	}
}

func TestBatchRecordsPerItemMetadata(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	service, err := NewWithFingerprintKeys(repo, &memoryObjects{}, [][]byte{[]byte(strings.Repeat("a", 32))}, allowRecipients(true))
	if err != nil {
		t.Fatal(err)
	}
	req := submission.BatchRequest{Adapter: "sendgrid", Messages: []submission.Message{
		{From: "a@Example.com", To: []string{"One <one@example.net>"}, Subject: "first", Text: "x"},
		{From: "B <b@other.example.org>", To: []string{"two@example.net", "three@example.net"}, Subject: "second", Text: "y"},
	}}
	if _, err = service.AcceptBatch(context.Background(), submission.App{ID: "app"}, req); err != nil {
		t.Fatal(err)
	}
	items := repo.last.Items
	if len(items) != 2 || items[0].Subject != "first" || items[0].SenderDomain != "example.com" || !reflect.DeepEqual(items[0].Recipients, []string{"one@example.net"}) ||
		items[1].Subject != "second" || items[1].SenderDomain != "other.example.org" || !reflect.DeepEqual(items[1].Recipients, []string{"two@example.net", "three@example.net"}) {
		t.Fatal("per-item metadata incorrect")
	}
	if items[0].SenderAddress != "a@Example.com" || items[0].SenderName != "" || items[1].SenderAddress != "b@other.example.org" || items[1].SenderName != "B" {
		t.Fatal("per-item sender incorrect")
	}
}
