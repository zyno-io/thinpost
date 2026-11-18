package acceptance

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/submission"
)

type suppressionRepository struct {
	memoryRepository
	suppressed map[string]bool
	checked    [][]string
	checkErr   error
}

func (r *suppressionRepository) SuppressedRecipients(_ context.Context, _ string, lowers []string) (int, error) {
	r.checked = append(r.checked, lowers)
	n := 0
	for _, l := range lowers {
		if r.suppressed[l] {
			n++
		}
	}
	return n, r.checkErr
}

func TestRejectAllSuppressedOnlyWhenEveryDistinctRecipientIsSuppressed(t *testing.T) {
	ctx := context.Background()
	repo := &suppressionRepository{suppressed: map[string]bool{"a@example.net": true}}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	req := validRequest()
	req.Message.To = []string{"A@Example.net", "a@example.net"}
	req.RejectAllSuppressed = true
	req.IdempotencyKey = ""
	_, err := service.Accept(ctx, submission.App{ID: "app-a"}, req)
	if !errors.Is(err, submission.ErrAllRecipientsSuppressed) {
		t.Fatalf("want 406 error, got %v", err)
	}
	if len(repo.records) != 0 || len(objects.objects) != 0 {
		t.Fatal("rejection stored something")
	}
	if len(repo.reservations) != 1 {
		t.Fatal("the check must follow the rate reservation")
	}
	if len(repo.checked) != 1 || len(repo.checked[0]) != 1 || repo.checked[0][0] != "a@example.net" {
		t.Fatalf("recipients not deduplicated and lowercased: %v", repo.checked)
	}
	// Partial suppression is accepted.
	req.Message.To = []string{"a@example.net", "b@example.net"}
	if _, err = service.Accept(ctx, submission.App{ID: "app-a"}, req); err != nil || len(repo.records) != 1 {
		t.Fatalf("partial suppression must be accepted: %v", err)
	}
	// Without the flag the list is not consulted.
	repo.checked = nil
	req.RejectAllSuppressed = false
	req.Message.To = []string{"a@example.net"}
	if _, err = service.Accept(ctx, submission.App{ID: "app-a"}, req); err != nil || len(repo.checked) != 0 {
		t.Fatalf("flag off: err %v checked %v", err, repo.checked)
	}
	// A read failure fails closed before anything is stored.
	req.RejectAllSuppressed = true
	repo.checkErr = errors.New("db")
	before := len(repo.records)
	if _, err = service.Accept(ctx, submission.App{ID: "app-a"}, req); err == nil || len(repo.records) != before || strings.Contains(err.Error(), "example") {
		t.Fatalf("read failure: %v", err)
	}
}

// Display names never affect matching: policy, suppression and the recorded
// recipient rows all key on the bare address.
func TestRecipientNamesDoNotAffectSuppressionOrRecordedRecipients(t *testing.T) {
	ctx := context.Background()
	repo := &suppressionRepository{suppressed: map[string]bool{"a@example.net": true}}
	service := testService(t, repo, &memoryObjects{})
	req := validRequest()
	req.Message.To = []string{`"Lovelace, Ada" <A@Example.net>`, `"Another Name" <a@example.net>`}
	req.RejectAllSuppressed = true
	req.IdempotencyKey = ""
	if _, err := service.Accept(ctx, submission.App{ID: "app-a"}, req); !errors.Is(err, submission.ErrAllRecipientsSuppressed) {
		t.Fatalf("named recipients evaded suppression: %v", err)
	}
	if len(repo.checked) != 1 || len(repo.checked[0]) != 1 || repo.checked[0][0] != "a@example.net" {
		t.Fatalf("suppression lookup used names: %v", repo.checked)
	}
	recipients, err := envelopeRecipients([]string{`"Zoë, Z" <z@example.net>`, "Bob <b@example.net>"})
	if err != nil || len(recipients) != 2 || recipients[0] != "z@example.net" || recipients[1] != "b@example.net" {
		t.Fatalf("recorded recipients kept names: %v %v", recipients, err)
	}
}
