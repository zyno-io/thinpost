package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/submission"
)

type allowRecipients bool

func (a allowRecipients) Allows(string) bool { return bool(a) }

type memoryObjects struct {
	objects map[string][]byte
	err     error
}

func (o *memoryObjects) Put(_ context.Context, key string, data []byte) error {
	if o.err != nil {
		return o.err
	}
	if o.objects == nil {
		o.objects = make(map[string][]byte)
	}
	o.objects[key] = append([]byte(nil), data...)
	return nil
}

type saved struct {
	record  Record
	receipt submission.Receipt
}
type memoryRepository struct {
	mu           sync.Mutex
	apps         map[[32]byte]submission.App
	records      []Record
	keys         map[string]saved
	err          error
	reserveErr   error
	reservations []int
}

func (r *memoryRepository) LookupKey(_ context.Context, digest [32]byte) (submission.App, error) {
	app, ok := r.apps[digest]
	if !ok {
		return submission.App{}, submission.ErrInvalidKey
	}
	return app, nil
}
func (r *memoryRepository) ReserveSubmission(_ context.Context, _, _ string, messages int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reservations = append(r.reservations, messages)
	return r.reserveErr
}
func (r *memoryRepository) Commit(_ context.Context, record Record) (submission.Receipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return submission.Receipt{}, r.err
	}
	if r.keys == nil {
		r.keys = make(map[string]saved)
	}
	scope := record.AppID + "|" + record.Adapter + "|" + record.IdempotencyKey
	if record.IdempotencyKey != "" {
		if old, ok := r.keys[scope]; ok {
			if !record.MatchesRequestHash(old.record.RequestHash[:]) {
				return submission.Receipt{}, submission.ErrIdempotencyConflict
			}
			replay := old.receipt
			replay.Replayed = true
			return replay, nil
		}
	}
	receipt := submission.Receipt{ID: record.ObjectKey, AcceptedAt: time.Now()}
	r.records = append(r.records, record)
	if record.IdempotencyKey != "" {
		r.keys[scope] = saved{record, receipt}
	}
	return receipt, nil
}
func validRequest() submission.Request {
	return submission.Request{Adapter: "resend", IdempotencyKey: "same-key", Message: submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "hello", Text: "body", EnhancedProtection: true}}
}
func TestAcceptanceStoresBeforeCommitAndScopesIdempotency(t *testing.T) {
	ctx := context.Background()
	repo := &memoryRepository{}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	req := validRequest()
	first, err := service.Accept(ctx, submission.App{ID: "app-a"}, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.Accept(ctx, submission.App{ID: "app-a"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != again.ID || !first.AcceptedAt.Equal(again.AcceptedAt) || !again.Replayed || len(repo.records) != 1 {
		t.Fatal("retry did not reuse committed receipt")
	}
	other, err := service.Accept(ctx, submission.App{ID: "app-b"}, req)
	if err != nil || other.ID == first.ID || len(repo.records) != 2 {
		t.Fatal("idempotency leaked across apps")
	}
	changed := req
	changed.Message.EnhancedProtection = false
	if _, err := service.Accept(ctx, submission.App{ID: "app-a"}, changed); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if repo.records[0].RequestHash == repo.records[1].RequestHash {
		t.Fatal("fingerprints correlated across apps")
	}
	if !repo.records[0].EnhancedProtection {
		t.Fatal("TLS policy lost from durable record")
	}
	var source struct {
		Version int
		Message submission.Message
	}
	if err := json.Unmarshal(objects.objects[repo.records[0].ObjectKey], &source); err != nil || source.Version != submission.SourceVersion || !source.Message.EnhancedProtection {
		t.Fatalf("bad stored object: %+v, %v", source, err)
	}
	if len(objects.objects) != 4 {
		t.Fatal("duplicate and failed-commit objects must remain available for safe orphan collection")
	}
}

func TestReplyToPreservesLegacyFingerprintAndConflictsWhenChanged(t *testing.T) {
	repo := &memoryRepository{}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	req := validRequest()
	// This frozen pre-reply-address JSON shape protects existing 24-hour
	// idempotency markers through an upgrade.
	got, err := json.Marshal(req.Message)
	if err != nil || string(got) != `{"From":"a@example.com","To":["b@example.net"],"Subject":"hello","Text":"body","HTML":"","EnhancedProtection":true}` {
		t.Fatal("legacy fingerprint input changed", err)
	}
	app := submission.App{ID: "app-a"}
	if _, err = service.Accept(context.Background(), app, req); err != nil {
		t.Fatal(err)
	}
	req.Message.ReplyTo = []string{"help@example.com"}
	if _, err = service.Accept(context.Background(), app, req); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("changed reply address did not conflict", err)
	}
	req.IdempotencyKey = "new-key"
	if _, err = service.Accept(context.Background(), app, req); err != nil {
		t.Fatal(err)
	}
	var source struct{ Message submission.Message }
	if err = json.Unmarshal(objects.objects[repo.records[len(repo.records)-1].ObjectKey], &source); err != nil || len(source.Message.ReplyTo) != 1 || source.Message.ReplyTo[0] != "help@example.com" {
		t.Fatal("reply address not durably stored", err)
	}
}

func TestAttachmentContentIsPartOfStoredSourceAndIdempotency(t *testing.T) {
	repo := &memoryRepository{}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	request := validRequest()
	request.Message.Attachments = []submission.Attachment{{Filename: "fixture.txt", ContentType: "text/plain", Content: []byte("synthetic attachment")}}
	app := submission.App{ID: "app-a"}
	first, err := service.Accept(context.Background(), app, request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.Accept(context.Background(), app, request)
	again.Replayed = false
	if err != nil || first != again {
		t.Fatal("identical attachment retry changed receipt", err)
	}
	var source struct{ Message submission.Message }
	if err = json.Unmarshal(objects.objects[repo.records[0].ObjectKey], &source); err != nil || len(source.Message.Attachments) != 1 || !bytes.Equal(source.Message.Attachments[0].Content, []byte("synthetic attachment")) {
		t.Fatal("attachment missing from stored source", err)
	}
	request.Message.Attachments[0].Content = []byte("different attachment")
	if _, err = service.Accept(context.Background(), app, request); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("changed attachment did not conflict", err)
	}
}

func TestTrackingOptionsAreIncludedInIdempotency(t *testing.T) {
	repo := &memoryRepository{}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	req := validRequest()
	req.Message.HTML = `<a href="https://example.net">link</a>`
	req.Message.Tracking = &submission.TrackingOptions{Opens: true}
	app := submission.App{ID: "app"}
	first, err := service.Accept(context.Background(), app, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.Accept(context.Background(), app, req)
	again.Replayed = false
	if err != nil || first != again {
		t.Fatal("identical tracking request did not reuse its receipt", err)
	}
	req.Message.Tracking = &submission.TrackingOptions{Clicks: true}
	if _, err = service.Accept(context.Background(), app, req); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("changed tracking options did not conflict", err)
	}
	req.IdempotencyKey = "invalid"
	req.Message.Tracking = &submission.TrackingOptions{}
	if _, err = service.Accept(context.Background(), app, req); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("empty tracking options were accepted", err)
	}
	req.Message.Tracking.Opens = true
	req.Message.HTML = ""
	if _, err = service.Accept(context.Background(), app, req); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("tracking without HTML was accepted", err)
	}
}
func TestFailuresCannotAcknowledge(t *testing.T) {
	ctx := context.Background()
	repo := &memoryRepository{}
	objects := &memoryObjects{err: errors.New("storage offline")}
	service := testService(t, repo, objects)
	if _, err := service.Accept(ctx, submission.App{ID: "app"}, validRequest()); err == nil || len(repo.records) != 0 {
		t.Fatal("storage failure reached commit")
	}
	objects.err = nil
	repo.err = errors.New("db offline")
	if _, err := service.Accept(ctx, submission.App{ID: "app"}, validRequest()); err == nil || len(repo.records) != 0 || len(objects.objects) != 1 {
		t.Fatal("failed commit incorrectly accepted or discarded object")
	}
}
func TestAuthenticationUsesKeyDigest(t *testing.T) {
	digest := sha256.Sum256([]byte("secret"))
	repo := &memoryRepository{apps: map[[32]byte]submission.App{digest: {ID: "app"}}}
	service := testService(t, repo, &memoryObjects{})
	app, err := service.Authenticate(context.Background(), "secret")
	if err != nil || app.ID != "app" {
		t.Fatal("credential lookup failed")
	}
	if _, err := service.Authenticate(context.Background(), "other"); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("unknown credential accepted")
	}
}

func testService(t *testing.T, repo Repository, objects Objects) *Service {
	t.Helper()
	service, err := New(repo, objects, []byte("0123456789abcdef0123456789abcdef"), allowRecipients(true))
	if err != nil {
		t.Fatal(err)
	}
	return service
}
func TestFingerprintRequiresSecret(t *testing.T) {
	if _, err := New(&memoryRepository{}, &memoryObjects{}, []byte("short"), allowRecipients(true)); err == nil {
		t.Fatal("accepted a short fingerprint key")
	}
}

func TestDeniedRecipientIsNotStored(t *testing.T) {
	objects := &memoryObjects{}
	service, err := New(&memoryRepository{}, objects, []byte("0123456789abcdef0123456789abcdef"), allowRecipients(false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Accept(context.Background(), submission.App{ID: "app"}, validRequest()); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("denied recipient accepted", err)
	}
	if len(objects.objects) != 0 {
		t.Fatal("denied submission stored")
	}
	if _, err = New(&memoryRepository{}, objects, []byte("0123456789abcdef0123456789abcdef"), nil); err == nil {
		t.Fatal("missing policy accepted")
	}
}

func TestFingerprintRotationKeepsRetriesScopedAndRejectsChanges(t *testing.T) {
	oldKey := []byte("old-fingerprint-key-0123456789abcdef")
	newKey := []byte("new-fingerprint-key-0123456789abcdef")
	repo := &memoryRepository{}
	objects := &memoryObjects{}
	old, err := NewWithFingerprintKeys(repo, objects, [][]byte{oldKey, newKey}, allowRecipients(true))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := NewWithFingerprintKeys(repo, objects, [][]byte{newKey, oldKey}, allowRecipients(true))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	app := submission.App{ID: "app"}
	request := validRequest()
	first, err := old.Accept(ctx, app, request)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := rotated.Accept(ctx, app, request)
	retry.Replayed = false
	if err != nil || retry != first {
		t.Fatal("retry after rotation failed", err)
	}
	request.IdempotencyKey = "second"
	second, err := rotated.Accept(ctx, app, request)
	if err != nil {
		t.Fatal(err)
	}
	retry, err = old.Accept(ctx, app, request)
	retry.Replayed = false
	if err != nil || retry != second {
		t.Fatal("mixed replicas failed to recognize new key", err)
	}
	request.Message.Text = "different body"
	if _, err = old.Accept(ctx, app, request); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("changed message was accepted", err)
	}
	if _, err = NewWithFingerprintKeys(repo, objects, [][]byte{oldKey, oldKey}, allowRecipients(true)); err == nil {
		t.Fatal("duplicate keys accepted")
	}
}

func TestSubmissionReservationPrecedesUpload(t *testing.T) {
	repo := &memoryRepository{reserveErr: submission.ErrRateLimited}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	_, err := service.Accept(context.Background(), submission.App{ID: "app", KeyID: "key"}, validRequest())
	if !errors.Is(err, submission.ErrRateLimited) || len(objects.objects) != 0 || len(repo.records) != 0 {
		t.Fatalf("denied reservation reached storage or commit: %v", err)
	}
	if len(repo.reservations) != 1 || repo.reservations[0] != 1 {
		t.Fatal("single submission did not reserve one message")
	}
}

func TestAcceptanceLogsOnlyIDsAndStatusAfterCommit(t *testing.T) {
	var lines []string
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }))
	repo := &memoryRepository{}
	service := testService(t, repo, &memoryObjects{})
	app := submission.App{ID: "0123456789abcdef0123456789abcdef"}
	if _, err := service.Accept(context.Background(), app, validRequest()); err != nil {
		t.Fatal(err)
	}
	// The in-memory receipt ID is an object key rather than a 32-hex message ID.
	if len(lines) != 1 || lines[0] != "message app="+app.ID+" message=invalid status=accepted" {
		t.Fatalf("unexpected log lines %q", lines)
	}
	repo.err = errors.New("commit failed")
	lines = nil
	req := validRequest()
	req.IdempotencyKey = "other"
	if _, err := service.Accept(context.Background(), app, req); err == nil || len(lines) != 0 {
		t.Fatalf("logged without commit: %q %v", lines, err)
	}
	for _, line := range lines {
		for _, forbidden := range []string{"example", "hello", "@"} {
			if strings.Contains(line, forbidden) {
				t.Fatalf("log contains %q: %s", forbidden, line)
			}
		}
	}
}

func TestIdempotentReplayIsLoggedOnce(t *testing.T) {
	var lines []string
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }))
	service := testService(t, &memoryRepository{}, &memoryObjects{})
	app := submission.App{ID: "0123456789abcdef0123456789abcdef"}
	for i := 0; i < 2; i++ {
		if _, err := service.Accept(context.Background(), app, validRequest()); err != nil {
			t.Fatal(err)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("expected one accepted line, got %q", lines)
	}
}

func TestListUnsubscribeKeepsLegacyEncodingAndConflictsWhenChanged(t *testing.T) {
	repo := &memoryRepository{}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	req := validRequest()
	// Absent U1 fields must not appear in the fingerprint input or stored source.
	got, err := json.Marshal(req.Message)
	if err != nil || string(got) != `{"From":"a@example.com","To":["b@example.net"],"Subject":"hello","Text":"body","HTML":"","EnhancedProtection":true}` {
		t.Fatal("legacy encoding changed", err)
	}
	app := submission.App{ID: "app-a"}
	if _, err = service.Accept(context.Background(), app, req); err != nil {
		t.Fatal(err)
	}
	req.Message.ListUnsubscribe = "<https://app.example.com/u/1>"
	req.Message.ListUnsubscribePost = true
	if _, err = service.Accept(context.Background(), app, req); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("changed List-Unsubscribe did not conflict", err)
	}
	req.IdempotencyKey = "new-key"
	if _, err = service.Accept(context.Background(), app, req); err != nil {
		t.Fatal(err)
	}
	var source struct{ Message submission.Message }
	if err = json.Unmarshal(objects.objects[repo.records[len(repo.records)-1].ObjectKey], &source); err != nil || source.Message.ListUnsubscribe != req.Message.ListUnsubscribe || !source.Message.ListUnsubscribePost {
		t.Fatal("List-Unsubscribe not durably stored", err)
	}
}

func escapedSource(t *testing.T, m submission.Message) int {
	t.Helper()
	body, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{1, m})
	if err != nil {
		t.Fatal(err)
	}
	return len(body)
}

// Go escapes "<" as six bytes, so 3 MiB of text passes preflight yet encodes to
// 18 MiB. It must be a clean rejection after one reservation, never an upload.
func TestEscapingExpandedSourceIsRejectedBeforeUpload(t *testing.T) {
	repo := &memoryRepository{}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	req := validRequest()
	req.Message.Text = strings.Repeat("<", 3<<20)
	if err := preflight(req.Message, false); err != nil {
		t.Fatal("fixture no longer passes preflight", err)
	}
	if _, err := service.Accept(context.Background(), submission.App{ID: "app", KeyID: "key"}, req); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("expected ErrInvalidMessage, got %v", err)
	}
	if len(repo.reservations) != 1 || len(objects.objects) != 0 || len(repo.records) != 0 {
		t.Fatal("oversized source reserved wrongly or reached storage", repo.reservations, len(objects.objects))
	}
}

// A fingerprint within the slack of the limit passes the early check, so the
// final source guard is the one that rejects a source just above 16 MiB. A long
// app ID makes the fingerprint larger than the source.
func TestSourceGuardRejectsJustOverLimitAfterEarlyCheck(t *testing.T) {
	for _, tc := range []struct {
		over int
		ok   bool
	}{{1, false}, {0, true}} {
		repo := &memoryRepository{}
		objects := &memoryObjects{}
		service := testService(t, repo, objects)
		req := validRequest()
		req.IdempotencyKey = ""
		req.Message.Text = ""
		base := escapedSource(t, req.Message)
		target := maxSourceBytes + tc.over
		n := (target - base) / 6
		req.Message.Text = strings.Repeat("<", n) + strings.Repeat("x", target-base-6*n)
		if got := escapedSource(t, req.Message); got != target {
			t.Fatalf("fixture size %d want %d", got, target)
		}
		app := submission.App{ID: strings.Repeat("a", 1500), KeyID: "key"}
		_, err := service.Accept(context.Background(), app, req)
		if tc.ok != (err == nil) || !tc.ok && !errors.Is(err, submission.ErrInvalidMessage) {
			t.Fatalf("over %d: %v", tc.over, err)
		}
		if tc.ok != (len(objects.objects) == 1) || len(repo.reservations) != 1 {
			t.Fatalf("over %d: objects %d reservations %v", tc.over, len(objects.objects), repo.reservations)
		}
	}
}

func TestThreadingHeadersKeepLegacyEncodingAndConflictWhenChanged(t *testing.T) {
	repo := &memoryRepository{}
	objects := &memoryObjects{}
	service := testService(t, repo, objects)
	req := validRequest()
	app := submission.App{ID: "app-a"}
	if _, err := service.Accept(context.Background(), app, req); err != nil {
		t.Fatal(err)
	}
	req.Message.InReplyTo = "<a@example.com>"
	req.Message.References = "<z@example.com> <a@example.com>"
	if _, err := service.Accept(context.Background(), app, req); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("changed threading headers did not conflict", err)
	}
	req.IdempotencyKey = "new-key"
	if _, err := service.Accept(context.Background(), app, req); err != nil {
		t.Fatal(err)
	}
	var source struct{ Message submission.Message }
	if err := json.Unmarshal(objects.objects[repo.records[len(repo.records)-1].ObjectKey], &source); err != nil || source.Message.InReplyTo != req.Message.InReplyTo || source.Message.References != req.Message.References {
		t.Fatal("threading headers not durably stored", err)
	}
}
