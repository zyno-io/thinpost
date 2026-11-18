package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/submission"
)

type savedBatch struct {
	record  BatchRecord
	receipt submission.BatchReceipt
}

type memoryBatchRepository struct {
	*memoryRepository
	batches map[string]savedBatch
	calls   int
	last    BatchRecord
}

type failSecondObject struct{ calls int }

func (o *failSecondObject) Put(context.Context, string, []byte) error {
	o.calls++
	if o.calls == 2 {
		return errors.New("storage failed")
	}
	return nil
}

func (r *memoryBatchRepository) CommitBatch(_ context.Context, record BatchRecord) (submission.BatchReceipt, error) {
	r.calls++
	r.last = record
	if r.batches == nil {
		r.batches = make(map[string]savedBatch)
	}
	scope := record.AppID + "|" + record.Adapter + "|" + record.IdempotencyKey
	if old, ok := r.batches[scope]; ok && record.IdempotencyKey != "" {
		if !record.MatchesRequestHash(old.record.RequestHash[:]) {
			return submission.BatchReceipt{}, submission.ErrIdempotencyConflict
		}
		replay := old.receipt
		replay.Replayed = true
		return replay, nil
	}
	receipt := submission.BatchReceipt{IDs: make([]string, len(record.Items))}
	for i, item := range record.Items {
		key, err := objectkey.Parse(item.ObjectKey)
		if err != nil {
			return submission.BatchReceipt{}, err
		}
		receipt.IDs[i] = key.ID
	}
	if record.IdempotencyKey != "" {
		r.batches[scope] = savedBatch{record, receipt}
	}
	return receipt, nil
}

func batchRequest() submission.BatchRequest {
	return submission.BatchRequest{Adapter: "resend", IdempotencyKey: "batch-key", Messages: []submission.Message{
		{From: "a@example.com", To: []string{"one@example.net"}, Subject: "one", Text: "first", EnhancedProtection: true},
		{From: "a@example.com", To: []string{"two@example.net"}, Subject: "two", HTML: "<b>second</b>"},
	}}
}

func TestBatchTrackingOptionsAreIncludedInIdempotency(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service, err := NewWithFingerprintKeys(repo, objects, [][]byte{[]byte(strings.Repeat("a", 32))}, allowRecipients(true))
	if err != nil {
		t.Fatal(err)
	}
	app, req := submission.App{ID: "app"}, batchRequest()
	req.Messages[0].HTML = "<b>first</b>"
	req.Messages[0].Tracking = &submission.TrackingOptions{Opens: true}
	first, err := service.AcceptBatch(context.Background(), app, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.AcceptBatch(context.Background(), app, req)
	again.Replayed = false
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatal("identical tracking batch did not reuse its receipt", err)
	}
	req.Messages[0].Tracking = &submission.TrackingOptions{Clicks: true}
	if _, err = service.AcceptBatch(context.Background(), app, req); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("changed batch tracking options did not conflict", err)
	}
}

func TestBatchAcceptancePreservesOrderPolicyAndRetryFingerprint(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	oldKey := []byte(strings.Repeat("a", 32))
	newKey := []byte(strings.Repeat("b", 32))
	old, err := NewWithFingerprintKeys(repo, objects, [][]byte{oldKey}, allowRecipients(true))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := NewWithFingerprintKeys(repo, objects, [][]byte{newKey, oldKey}, allowRecipients(true))
	if err != nil {
		t.Fatal(err)
	}
	app, request := submission.App{ID: "app", KeyID: "key"}, batchRequest()
	first, err := old.AcceptBatch(context.Background(), app, request)
	if err != nil || len(first.IDs) != 2 || first.IDs[0] == first.IDs[1] {
		t.Fatal("batch failed", err)
	}
	saved := repo.batches["app|resend|batch-key"]
	if !saved.record.Items[0].EnhancedProtection || saved.record.Items[1].EnhancedProtection {
		t.Fatal("per-message TLS policy lost")
	}
	for i, item := range saved.record.Items {
		var source struct {
			Version int                `json:"version"`
			Message submission.Message `json:"message"`
		}
		if err := json.Unmarshal(objects.objects[item.ObjectKey], &source); err != nil || source.Version != submission.SourceVersion || !reflect.DeepEqual(source.Message, request.Messages[i]) {
			t.Fatal("stored batch source did not match its message", err)
		}
	}
	retry, err := rotated.AcceptBatch(context.Background(), app, request)
	if err != nil || !reflect.DeepEqual(retry.IDs, first.IDs) {
		t.Fatal("retry across fingerprint rotation changed ordered IDs", err)
	}
	request.Messages[1].EnhancedProtection = true
	if _, err = rotated.AcceptBatch(context.Background(), app, request); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("changed batch policy did not conflict", err)
	}
	request.Messages[1].EnhancedProtection = false
	request.Messages[0], request.Messages[1] = request.Messages[1], request.Messages[0]
	if _, err = rotated.AcceptBatch(context.Background(), app, request); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("reordered batch did not conflict", err)
	}
}

func TestBatchRejectsBeforeUploadAndDoesNotCommitAfterStorageFailure(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	req := batchRequest()
	req.Messages[1].From = "bad\naddress"
	if _, err := service.AcceptBatch(context.Background(), submission.App{ID: "a"}, req); !errors.Is(err, submission.ErrInvalidMessage) || len(objects.objects) != 0 || repo.calls != 0 {
		t.Fatal("invalid second item was not rejected before upload", err)
	}
	denied, err := New(repo, objects, []byte(strings.Repeat("a", 32)), allowRecipients(false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = denied.AcceptBatch(context.Background(), submission.App{ID: "a"}, batchRequest()); !errors.Is(err, submission.ErrInvalidMessage) || len(objects.objects) != 0 || repo.calls != 0 {
		t.Fatal("recipient policy did not reject the whole batch before upload", err)
	}
	objects.err = errors.New("storage failed")
	if _, err = service.AcceptBatch(context.Background(), submission.App{ID: "a"}, batchRequest()); err == nil || repo.calls != 0 {
		t.Fatal("storage failure reached database commit", err)
	}
	partial := &failSecondObject{}
	service = testService(t, repo, partial)
	if _, err = service.AcceptBatch(context.Background(), submission.App{ID: "a"}, batchRequest()); err == nil || partial.calls != 2 || repo.calls != 0 {
		t.Fatal("partial object upload reached database commit", err)
	}
}

func TestBatchReservationPrecedesAnyUpload(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{reserveErr: submission.ErrRateLimited}}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	message := validRequest().Message
	_, err := service.AcceptBatch(context.Background(), submission.App{ID: "app", KeyID: "key"}, submission.BatchRequest{Adapter: "resend", Messages: []submission.Message{message, message}})
	if !errors.Is(err, submission.ErrRateLimited) || len(objects.objects) != 0 {
		t.Fatalf("denied batch reached storage: %v", err)
	}
	if len(repo.reservations) != 1 || repo.reservations[0] != 2 {
		t.Fatal("batch did not reserve every message")
	}
}

func TestSendGridBatchUsesItsOwnAdapterAndRejectsUnsupportedKeys(t *testing.T) {
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	req := batchRequest()
	req.Adapter = "sendgrid"
	if _, err := service.AcceptBatch(context.Background(), submission.App{ID: "app", KeyID: "key"}, req); !errors.Is(err, submission.ErrInvalidMessage) || repo.calls != 0 || len(objects.objects) != 0 {
		t.Fatal("unsupported SendGrid idempotency reached storage", err)
	}
	req.IdempotencyKey = ""
	receipt, err := service.AcceptBatch(context.Background(), submission.App{ID: "app", KeyID: "key"}, req)
	if err != nil || len(receipt.IDs) != 2 || repo.last.Adapter != "sendgrid" || repo.last.IdempotencyKey != "" || !repo.last.Items[0].EnhancedProtection || repo.last.Items[1].EnhancedProtection {
		t.Fatal("SendGrid batch lost its adapter or policy", err)
	}
	for i, item := range repo.last.Items {
		var source struct {
			Message submission.Message `json:"message"`
		}
		if err = json.Unmarshal(objects.objects[item.ObjectKey], &source); err != nil || !reflect.DeepEqual(source.Message, req.Messages[i]) {
			t.Fatal("personalization stored incorrectly", i, err)
		}
	}
	before := len(objects.objects)
	req.Adapter = "unknown"
	if _, err = service.AcceptBatch(context.Background(), submission.App{ID: "app", KeyID: "key"}, req); !errors.Is(err, submission.ErrInvalidMessage) || len(objects.objects) != before {
		t.Fatal("unknown adapter accepted", err)
	}
}

func TestBatchLogsEachAcceptedMessageID(t *testing.T) {
	var lines []string
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }))
	repo := &memoryBatchRepository{memoryRepository: &memoryRepository{}}
	service, err := NewWithFingerprintKeys(repo, &memoryObjects{}, [][]byte{[]byte(strings.Repeat("a", 32))}, allowRecipients(true))
	if err != nil {
		t.Fatal(err)
	}
	app := submission.App{ID: "0123456789abcdef0123456789abcdef", KeyID: "key"}
	receipt, err := service.AcceptBatch(context.Background(), app, batchRequest())
	if err != nil || len(receipt.IDs) != 2 || len(lines) != 2 {
		t.Fatalf("unexpected result %v %v %q", receipt, err, lines)
	}
	for i, id := range receipt.IDs {
		if want := "message app=" + app.ID + " message=" + id + " status=accepted"; lines[i] != want {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want)
		}
	}
}

func TestIdempotentBatchReplayIsLoggedOnce(t *testing.T) {
	var lines []string
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }))
	service, err := NewWithFingerprintKeys(&memoryBatchRepository{memoryRepository: &memoryRepository{}}, &memoryObjects{}, [][]byte{[]byte(strings.Repeat("a", 32))}, allowRecipients(true))
	if err != nil {
		t.Fatal(err)
	}
	app := submission.App{ID: "0123456789abcdef0123456789abcdef"}
	for i := 0; i < 2; i++ {
		if _, err = service.AcceptBatch(context.Background(), app, batchRequest()); err != nil {
			t.Fatal(err)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("expected one line per message, got %q", lines)
	}
}
