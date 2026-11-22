package preparation

import (
	"bytes"
	"context"
	"encoding/json"
	"net/mail"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

func useSource(t *testing.T, objects *fakeObjects, version int, message submission.Message) {
	t.Helper()
	source, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{version, message})
	if err != nil {
		t.Fatal(err)
	}
	objects.source = source
}

func listMessage() submission.Message {
	return submission.Message{From: "a@example.com", To: []string{"b@example.net", "c@example.net"}, Cc: []string{"Copy <cc@example.net>"}, Bcc: []string{"bcc@example.net"}, Subject: "hello", Text: "body", EnhancedProtection: true}
}

func TestVersion2SourceListsToAndCcAndNeverBccOnEveryCopy(t *testing.T) {
	service, repo, objects, _ := setup(t)
	useSource(t, objects, 2, listMessage())
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.deliveries) != 4 {
		t.Fatalf("%d deliveries, want one per To, Cc and Bcc recipient", len(repo.deliveries))
	}
	for i, delivery := range repo.deliveries {
		if delivery.RecipientIndex != i {
			t.Fatal("recipient index out of envelope order")
		}
		data := objects.message(t, delivery.ObjectKey)
		parsed, err := mail.ReadMessage(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		to, toErr := parsed.Header.AddressList("To")
		cc, ccErr := parsed.Header.AddressList("Cc")
		if toErr != nil || ccErr != nil || len(to) != 2 || len(cc) != 1 || cc[0].Address != "cc@example.net" {
			t.Fatal("To and Cc lists missing on copy", i, toErr, ccErr)
		}
		if _, ok := parsed.Header["Bcc"]; ok || bytes.Contains(data, []byte("bcc@example.net")) {
			t.Fatal("Bcc recipient visible on copy", i)
		}
	}
}

func TestVersion1SourceKeepsSingleRecipientTo(t *testing.T) {
	service, repo, objects, _ := setup(t)
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, delivery := range repo.deliveries {
		data := objects.message(t, delivery.ObjectKey)
		parsed, err := mail.ReadMessage(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if to, err := parsed.Header.AddressList("To"); err != nil || len(to) != 1 {
			t.Fatal("version 1 copy must list only its own recipient", err)
		}
	}
}

func TestVersion1SourceWithCcOrBccIsInvalid(t *testing.T) {
	service, repo, objects, _ := setup(t)
	useSource(t, objects, 1, listMessage())
	if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 {
		t.Fatal("a version 1 source with Cc or Bcc must not prepare", err)
	}
}
