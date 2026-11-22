package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/message"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/receivedtrace"
	"github.com/zyno-io/thinpost/internal/submission"
)

// smtpRepository adds the optional SMTP capability to the memory repository.
type smtpRepository struct {
	*memoryRepository
	reserveCalls []smtpReservation
	reserveErr   error
}

type smtpReservation struct {
	app, credential string
	digest          [32]byte
}

func (r *smtpRepository) ReserveSMTPSubmission(_ context.Context, appID, credentialID string, digest [32]byte) error {
	r.reserveCalls = append(r.reserveCalls, smtpReservation{appID, credentialID, digest})
	return r.reserveErr
}

var smtpApp = submission.App{ID: "app-a", SMTPCredentialID: "0123456789abcdef0123456789abcdef", SMTPCredentialDigest: sha256.Sum256([]byte("secret"))}

var smtpFence = submission.Fence{WorkerID: "worker-1", ControllerID: "controller-1", Epoch: 7}

// rawRequest decides a From for raw and returns a request ready for AcceptRaw.
func rawRequest(t *testing.T, raw string, mailFrom string, policy rawmime.Policy, rcpts ...string) RawRequest {
	t.Helper()
	scanned, err := rawmime.Scan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := rawmime.DecideFrom(scanned, mailFrom, policy, func(d string) bool { return d == "example.com" })
	if err != nil {
		t.Fatal(err)
	}
	recipients, kinds, err := rawmime.Kinds(rcpts, scanned)
	if err != nil {
		t.Fatal(err)
	}
	req := RawRequest{Raw: []byte(raw), Scan: scanned, MailFrom: mailFrom, Recipients: recipients, From: decision}
	for _, kind := range kinds {
		req.Kinds = append(req.Kinds, string(kind))
	}
	return req
}

const plainRaw = "From: Sender <sender@example.com>\r\nTo: rcpt@example.net\r\nSubject: Hello\r\nMessage-ID: <abc@client.example.org>\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\n\r\nbody\r\n"

func rawService(t *testing.T, repo Repository, objects Objects, allow bool) *Service {
	t.Helper()
	service, err := New(repo, objects, []byte("0123456789abcdef0123456789abcdef"), allowRecipients(allow))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestAcceptRawRequiresTheSMTPPrincipal(t *testing.T) {
	repo := &smtpRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := rawService(t, repo, objects, true)
	req := rawRequest(t, plainRaw, "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
	for name, app := range map[string]submission.App{
		"no credential": {ID: "app-a"},
		"a key":         {ID: "app-a", KeyID: "key"},
		"both":          {ID: "app-a", KeyID: "key", SMTPCredentialID: smtpApp.SMTPCredentialID},
		"operator":      {ID: "app-a", Operator: "admin", SMTPCredentialID: smtpApp.SMTPCredentialID},
		"no app":        {SMTPCredentialID: smtpApp.SMTPCredentialID},
	} {
		if _, err := service.AcceptRaw(context.Background(), app, smtpFence, req); !errors.Is(err, submission.ErrInvalidMessage) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A repository without the SMTP capability is unavailable.
	plain := rawService(t, &memoryRepository{}, objects, true)
	if _, err := plain.AcceptRaw(context.Background(), smtpApp, smtpFence, req); !errors.Is(err, submission.ErrUnavailable) {
		t.Fatal(err)
	}
	if len(repo.reserveCalls) != 0 || len(objects.objects) != 0 || len(repo.records) != 0 {
		t.Fatal("a refused principal reserved, stored or committed")
	}
}

func TestAcceptRawRefusesTheWholeMessageForOneDisallowedRecipient(t *testing.T) {
	repo := &smtpRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := rawService(t, repo, objects, false)
	req := rawRequest(t, plainRaw, "sender@example.com", rawmime.Policy{}, "rcpt@example.net", "other@example.org")
	if _, err := service.AcceptRaw(context.Background(), smtpApp, smtpFence, req); !errors.Is(err, submission.ErrRecipientNotPermitted) {
		t.Fatal(err)
	}
	if len(repo.reserveCalls) != 0 || len(objects.objects) != 0 || len(repo.records) != 0 {
		t.Fatal("a refused recipient was charged, stored or committed")
	}
}

type selectiveRecipients map[string]bool

func (s selectiveRecipients) Allows(address string) bool { return s[address] }

func TestAcceptRawChecksEveryRecipient(t *testing.T) {
	repo := &smtpRepository{memoryRepository: &memoryRepository{}}
	service, err := New(repo, &memoryObjects{}, []byte("0123456789abcdef0123456789abcdef"), selectiveRecipients{"rcpt@example.net": true})
	if err != nil {
		t.Fatal(err)
	}
	// The second recipient is the one outside the list, and it is a Bcc.
	req := rawRequest(t, plainRaw, "sender@example.com", rawmime.Policy{}, "rcpt@example.net", "hidden@example.org")
	if _, err = service.AcceptRaw(context.Background(), smtpApp, smtpFence, req); !errors.Is(err, submission.ErrRecipientNotPermitted) || len(repo.records) != 0 {
		t.Fatal("a Bcc outside the allowlist was accepted", err)
	}
	req = rawRequest(t, plainRaw, "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
	if _, err = service.AcceptRaw(context.Background(), smtpApp, smtpFence, req); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptRawReservesBeforeStoringAndMapsErrors(t *testing.T) {
	for _, reserveErr := range []error{submission.ErrRateLimited, submission.ErrAppPaused, submission.ErrInvalidKey} {
		repo := &smtpRepository{memoryRepository: &memoryRepository{}, reserveErr: reserveErr}
		objects := &memoryObjects{}
		service := rawService(t, repo, objects, true)
		req := rawRequest(t, plainRaw, "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
		if _, err := service.AcceptRaw(context.Background(), smtpApp, smtpFence, req); !errors.Is(err, reserveErr) {
			t.Fatal(reserveErr, err)
		}
		if len(repo.reserveCalls) != 1 || repo.reserveCalls[0] != (smtpReservation{"app-a", smtpApp.SMTPCredentialID, smtpApp.SMTPCredentialDigest}) || len(objects.objects) != 0 || len(repo.records) != 0 {
			t.Fatal("reservation", repo.reserveCalls)
		}
	}
}

func TestAcceptRawTooLargeIsChargedButNeverStored(t *testing.T) {
	repo := &smtpRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := rawService(t, repo, objects, true)
	// Every bare line feed gains a carriage return in the prepared message, so
	// this fits the 7.5 MiB raw bound and still prepares to more than 8 MiB.
	big := "From: sender@example.com\r\nTo: rcpt@example.net\r\n\r\n" + strings.Repeat("\n", 4_500_000)
	req := rawRequest(t, big, "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
	if _, err := service.AcceptRaw(context.Background(), smtpApp, smtpFence, req); !errors.Is(err, submission.ErrMessageTooLarge) {
		t.Fatal(err)
	}
	if len(repo.reserveCalls) != 1 || len(objects.objects) != 0 || len(repo.records) != 0 {
		t.Fatal("too large", len(repo.reserveCalls), len(objects.objects), len(repo.records))
	}
	// Over the raw bound is refused without being charged.
	huge := req
	huge.Raw = bytes.Repeat([]byte("a"), rawmime.MaxRawBytes+1)
	if _, err := service.AcceptRaw(context.Background(), smtpApp, smtpFence, huge); !errors.Is(err, submission.ErrMessageTooLarge) || len(repo.reserveCalls) != 1 {
		t.Fatal("over the raw bound", err)
	}
}

func TestAcceptRawStoresASourceThatDecodesAndCommitsTheRecord(t *testing.T) {
	repo := &smtpRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := rawService(t, repo, objects, true)
	digest := sha256.Sum256([]byte("digest"))

	// A kept From: from, from_field and original_from stay empty.
	req := rawRequest(t, plainRaw, "sender@example.com", rawmime.Policy{}, "rcpt@example.net", "hidden@example.org")
	req.Replay, req.ReplayDigest, req.EnhancedProtection = ReplayMessageID, digest, true
	receipt, err := service.AcceptRaw(context.Background(), smtpApp, smtpFence, req)
	if err != nil || receipt.Replayed || receipt.Held {
		t.Fatal(receipt, err)
	}
	record := repo.records[0]
	if record.Adapter != submission.AdapterSMTP || !record.Raw || record.Fence != smtpFence || record.SMTPCredentialID != smtpApp.SMTPCredentialID || record.SMTPCredentialDigest != smtpApp.SMTPCredentialDigest ||
		record.KeyID != "" || record.Operator != "" || record.From != `"Sender" <sender@example.com>` || record.SenderAddress != "sender@example.com" || record.SenderName != "Sender" || record.SenderDomain != "example.com" ||
		record.Subject != "Hello" || record.RecipientCount != 2 || strings.Join(record.RecipientKinds, ",") != "to,bcc" || record.OriginalFromAddress != "" || !record.EnhancedProtection || record.Hold ||
		record.IdempotencyKey != "smtp1:"+hex.EncodeToString(digest[:]) || record.IdempotencyTTL != 24*time.Hour || record.RequestHash != digest {
		t.Fatalf("record: %#v", record)
	}
	source, err := submission.DecodeSource(objects.objects[record.ObjectKey], true)
	if err != nil || source.Version != submission.RawSourceVersion || !bytes.Equal(source.Raw, []byte(plainRaw)) {
		t.Fatal("source", err)
	}
	h := source.RawHeader
	if h.From != "" || h.FromField != "" || h.FromRewritten || h.OriginalFrom != "" || h.SenderDomain != "example.com" || h.MailFrom != "sender@example.com" ||
		strings.Join(h.Recipients, ",") != "rcpt@example.net,hidden@example.org" || strings.Join(h.Kinds, ",") != "to,bcc" || h.StripSender {
		t.Fatalf("header for a kept From: %#v", h)
	}

	// A rewritten From: the decision's field is stored with the unquoted original.
	policy := rawmime.Policy{Mode: rawmime.FromAddress, Address: "noreply@example.com"}
	req = rawRequest(t, "From: \"Doe, Jane\" <jane@elsewhere.example.org>\r\nTo: rcpt@example.net\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\n\r\nbody\r\n", "jane@elsewhere.example.org", policy, "rcpt@example.net")
	req.Replay, req.ReplayDigest, req.Hold = ReplayContent, digest, true
	receipt, err = service.AcceptRaw(context.Background(), smtpApp, smtpFence, req)
	if err != nil || !receipt.Held {
		t.Fatal(receipt, err)
	}
	record = repo.records[1]
	if record.OriginalFromAddress != "jane@elsewhere.example.org" || !record.Hold || record.SenderAddress != "noreply@example.com" || record.SenderName != "Doe, Jane" ||
		record.IdempotencyKey != "smtp-c1:"+hex.EncodeToString(digest[:]) || record.IdempotencyTTL != 10*time.Minute || record.RequestHash != digest {
		t.Fatalf("record: %#v", record)
	}
	source, err = submission.DecodeSource(objects.objects[record.ObjectKey], false)
	if err != nil {
		t.Fatal(err)
	}
	h = source.RawHeader
	if !h.FromRewritten || h.From != req.From.Value || h.FromField != req.From.Field || h.OriginalFrom != "jane@elsewhere.example.org" || !h.StripSender || h.SenderDomain != "example.com" {
		t.Fatalf("header for a rewritten From: %#v", h)
	}
	if !strings.HasSuffix(h.FromField, "\r\n") || strings.ReplaceAll(h.FromField, "\r\n", "") != "From: "+h.From {
		t.Fatal("from_field is not the wire form of from")
	}

	// Neither header: no idempotency key and no window, and a hold is returned
	// again when the commit reports a replay.
	req = rawRequest(t, "From: sender@example.com\r\nTo: rcpt@example.net\r\n\r\nbody\r\n", "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
	if _, err = service.AcceptRaw(context.Background(), smtpApp, smtpFence, req); err != nil {
		t.Fatal(err)
	}
	if record = repo.records[2]; record.IdempotencyKey != "" || record.IdempotencyTTL != 0 || record.RequestHash != [32]byte{} {
		t.Fatalf("record without a replay identity: %#v", record)
	}
	if record.Subject != "" {
		t.Fatal("a message without a Subject must commit an empty subject")
	}
}

func TestAcceptRawReplayKeepsTheHoldChoice(t *testing.T) {
	repo := &smtpRepository{memoryRepository: &memoryRepository{}}
	service := rawService(t, repo, &memoryObjects{}, true)
	digest := sha256.Sum256([]byte("held"))
	raw := "From: sender@example.com\r\nTo: rcpt@example.net\r\nX-Thinpost-Hold: true\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\n\r\nbody\r\n"
	req := rawRequest(t, raw, "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
	req.Replay, req.ReplayDigest, req.Hold = ReplayContent, digest, req.Scan.Hold
	if !req.Hold {
		t.Fatal("the scan did not see the hold header")
	}
	first, err := service.AcceptRaw(context.Background(), smtpApp, smtpFence, req)
	if err != nil || !first.Held || first.Replayed {
		t.Fatal(first, err)
	}
	again, err := service.AcceptRaw(context.Background(), smtpApp, smtpFence, req)
	if err != nil || !again.Replayed || !again.Held || again.ID != first.ID {
		t.Fatal("a replay of a held message must report it held", again, err)
	}
}

func TestAcceptRawLogsAcceptanceAndHold(t *testing.T) {
	var lines []string
	restore := oplog.SetOutput(func(format string, args ...any) { lines = append(lines, format) })
	defer restore()
	repo := &smtpRepository{memoryRepository: &memoryRepository{}}
	service := rawService(t, repo, &memoryObjects{}, true)
	req := rawRequest(t, plainRaw, "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
	req.Hold = true
	if _, err := service.AcceptRaw(context.Background(), smtpApp, smtpFence, req); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("log lines: %v", lines)
	}
}

func TestAcceptRawValidatesTheRequest(t *testing.T) {
	repo := &smtpRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := rawService(t, repo, objects, true)
	good := rawRequest(t, plainRaw, "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
	good.Replay, good.ReplayDigest = ReplayMessageID, sha256.Sum256([]byte("x"))
	for name, mutate := range map[string]func(*RawRequest){
		"no scan":             func(r *RawRequest) { r.Scan = nil },
		"scan of other bytes": func(r *RawRequest) { r.Raw = append([]byte(nil), r.Raw...); r.Raw = r.Raw[:len(r.Raw)-1] },
		"no recipients":       func(r *RawRequest) { r.Recipients, r.Kinds = nil, nil },
		"kinds mismatch":      func(r *RawRequest) { r.Kinds = nil },
		"no domain":           func(r *RawRequest) { r.From.Domain = "" },
		"digest missing":      func(r *RawRequest) { r.ReplayDigest = [32]byte{} },
		"unknown replay kind": func(r *RawRequest) { r.Replay = 9 },
		"unknown kind":        func(r *RawRequest) { r.Kinds = []string{"reply"} },
	} {
		req := good
		mutate(&req)
		if _, err := service.AcceptRaw(context.Background(), smtpApp, smtpFence, req); !errors.Is(err, submission.ErrInvalidMessage) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(objects.objects) != 0 || len(repo.records) != 0 {
		t.Fatal("an invalid request was stored or committed")
	}
}

// Acceptance measures a raw message as written and with Thinpost's one-click
// headers in place of its own, because the app can switch one-click on before
// preparation. A message that fits only without them is refused (552) up front,
// never prepared into an object the edge would reject.
func TestAcceptRawBudgetsTheOneClickHeaders(t *testing.T) {
	const head = "From: sender@example.com\r\nTo: rcpt@example.net\r\nSubject: Hello\r\nMessage-ID: <abc@client.example.org>\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\n\r\n"
	signer, err := rawmime.SizingSigner(message.MaxDKIMRSABits)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := receivedtrace.Field(receivedtrace.SizingHost(), receivedtrace.WithSMTP, sizingMessageID, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	empty := rawRequest(t, head, "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
	base, err := rawmime.PreparedSize(empty.Raw, rawmime.PrepareInput{Scan: empty.Scan, From: empty.From, MessageID: sizingMessageID, Date: time.Unix(0, 0).UTC(), Selector: sizingSelector, Signer: signer, Received: trace})
	if err != nil {
		t.Fatal(err)
	}
	// Each bare line feed becomes CRLF: two bytes. Leave 300 bytes, which holds
	// without the one-click headers (about 700 bytes) and not with them.
	lines := (rawmime.MaxPreparedMessage - rawmime.SizeSlack - base.Total - 300) / 2
	repo := &smtpRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := rawService(t, repo, objects, true)
	req := rawRequest(t, head+strings.Repeat("\n", lines), "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
	if _, err := service.AcceptRaw(context.Background(), smtpApp, smtpFence, req); !errors.Is(err, submission.ErrMessageTooLarge) || len(objects.objects) != 0 || len(repo.records) != 0 {
		t.Fatal("a message that cannot take the one-click headers was accepted", err)
	}
	// 600 bytes less fits both ways.
	req = rawRequest(t, head+strings.Repeat("\n", lines-500), "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
	if _, err := service.AcceptRaw(context.Background(), smtpApp, smtpFence, req); err != nil {
		t.Fatal(err)
	}
}

// A raw message with its own List-Unsubscribe fields is accepted as is; the
// stored source keeps them for forwarding.
func TestAcceptRawKeepsTheClientsUnsubscribeFields(t *testing.T) {
	repo := &smtpRepository{memoryRepository: &memoryRepository{}}
	objects := &memoryObjects{}
	service := rawService(t, repo, objects, true)
	raw := "List-Unsubscribe: <https://app.example.com/u>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n" + plainRaw
	req := rawRequest(t, raw, "sender@example.com", rawmime.Policy{}, "rcpt@example.net")
	if !req.Scan.ListUnsubscribe {
		t.Fatal("the scan missed the fields")
	}
	if _, err := service.AcceptRaw(context.Background(), smtpApp, smtpFence, req); err != nil || len(objects.objects) != 1 {
		t.Fatal(err)
	}
}
