package bounceingest

import (
	"context"
	"errors"
	"testing"
)

type repository struct {
	exists         bool
	err, commitErr error
	records        []Record
}

func (r *repository) CheckBounce(_ context.Context, _ [32]byte, fingerprint [32]byte) (Status, error) {
	if r.err != nil {
		return Unknown, r.err
	}
	if !r.exists {
		return Unknown, nil
	}
	for _, record := range r.records {
		if record.Fingerprint == fingerprint {
			return Duplicate, nil
		}
	}
	return New, nil
}
func (r *repository) CommitBounce(_ context.Context, record Record) error {
	r.records = append(r.records, record)
	return r.commitErr
}

type objects struct {
	err    error
	writes int
}

func (o *objects) PutMIME(context.Context, string, []byte) error { o.writes++; return o.err }

func TestForwardRequiresStorageAndDeduplicatesFingerprint(t *testing.T) {
	r := &repository{exists: true}
	o := &objects{}
	s := &Service{Repository: r, Objects: o}
	const token = "0123456789abcdef0123456789abcdef"
	const request = "abcdef0123456789abcdef0123456789"
	body := []byte("Subject: delivery failure\r\n\r\nUntrusted body")
	if err := s.Forward(context.Background(), token, request, body); err != nil {
		t.Fatal(err)
	}
	if err := s.Forward(context.Background(), token, request, body); err != nil {
		t.Fatal(err)
	}
	if len(r.records) != 1 || o.writes != 1 {
		t.Fatal("retry uploaded or committed a duplicate")
	}
	r.exists = false
	if err := s.Forward(context.Background(), token, request, body); err == nil || o.writes != 1 {
		t.Fatal("unknown token uploaded")
	}
	r.exists = true
	o.err = errors.New("object write failed")
	if err := s.Forward(context.Background(), token, request, append(body, 'x')); err == nil || len(r.records) != 1 {
		t.Fatal("failed object write committed")
	}
	o.err = nil
	r.commitErr = errors.New("database commit unavailable")
	if err := s.Forward(context.Background(), token, request, append(body, 'y')); err == nil {
		t.Fatal("database failure accepted")
	}
}

type feedbackHandler struct {
	mailbox      bool
	err, procErr error
	processed    int
}

func (f *feedbackHandler) Mailbox(context.Context, [32]byte) (bool, error) { return f.mailbox, f.err }
func (f *feedbackHandler) Process(context.Context, []byte) error           { f.processed++; return f.procErr }

func TestForwardDispatchesFeedbackMailboxByTokenHash(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	const request = "abcdef0123456789abcdef0123456789"
	body := []byte("Subject: abuse report\r\n\r\nx")
	// A feedback token never touches the bounce repository or object storage.
	r, o, f := &repository{exists: true}, &objects{}, &feedbackHandler{mailbox: true}
	s := &Service{Repository: r, Objects: o, Feedback: f}
	if err := s.Forward(context.Background(), token, request, body); err != nil || f.processed != 1 || len(r.records) != 0 || o.writes != 0 {
		t.Fatal("feedback token took the bounce path", err)
	}
	// An error from the feedback side (commit failure, gate closed) is returned so Edge answers 451.
	f.procErr = errors.New("closed")
	if err := s.Forward(context.Background(), token, request, body); err == nil {
		t.Fatal("feedback error was swallowed")
	}
	f.err = errors.New("closed")
	if err := s.Forward(context.Background(), token, request, body); err == nil {
		t.Fatal("mailbox lookup error was swallowed")
	}
	// A delivery token with a feedback handler configured takes the unchanged bounce path.
	f.err, f.procErr, f.mailbox, f.processed = nil, nil, false, 0
	if err := s.Forward(context.Background(), token, request, body); err != nil || f.processed != 0 || len(r.records) != 1 || o.writes != 1 {
		t.Fatal("delivery token did not take the bounce path", err)
	}
	// An unknown token keeps today's behavior: rejected, nothing stored.
	r.exists = false
	if err := s.Forward(context.Background(), token, request, []byte("Subject: other\r\n\r\ny")); err == nil || o.writes != 1 {
		t.Fatal("unknown token accepted")
	}
}
