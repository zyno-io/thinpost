package smtpintake

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/submission"
)

const (
	appID  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	credID = "0123456789abcdef0123456789abcdef"
	secret = "tps_secret-value"
)

// fakeRepository is the intake's repository, with the calls it saw. It also
// serves as the acceptance repository so that a whole Submit runs without a
// database.
type fakeRepository struct {
	mu          sync.Mutex
	calls       []string
	verifyErr   error
	active      bool
	policy      rawmime.Policy
	verified    []string
	policyErr   error
	replays     map[string]submission.Receipt
	replayErr   error
	reserveErr  error
	commitErr   error
	records     []acceptance.Record
	candidates  []string
	touchErr    error
	lastDigest  [32]byte
	wantDigest  [32]byte
	commitCount int
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{active: true, verified: []string{"example.com"}, replays: map[string]submission.Receipt{}, wantDigest: sha256.Sum256([]byte(secret))}
}

func (r *fakeRepository) note(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *fakeRepository) VerifySMTPCredential(_ context.Context, id string, digest [32]byte) (string, bool, error) {
	r.note("verify")
	r.lastDigest = digest
	if r.verifyErr != nil {
		return "", false, r.verifyErr
	}
	if id != credID || !hmac.Equal(digest[:], r.wantDigest[:]) {
		return "", false, submission.ErrInvalidKey
	}
	return appID, r.active, nil
}

func (r *fakeRepository) TouchSMTPCredential(context.Context, string) error {
	r.note("touch")
	return r.touchErr
}

func (r *fakeRepository) SMTPPolicy(_ context.Context, _ string, candidates []string) (rawmime.Policy, []string, error) {
	r.note("policy")
	r.candidates = candidates
	return r.policy, r.verified, r.policyErr
}

func (r *fakeRepository) LookupSMTPReplay(_ context.Context, _, key string) (submission.Receipt, bool, error) {
	r.note("lookup")
	receipt, ok := r.replays[key]
	return receipt, ok, r.replayErr
}

func (r *fakeRepository) LookupKey(context.Context, [32]byte) (submission.App, error) {
	return submission.App{}, submission.ErrInvalidKey
}
func (r *fakeRepository) ReserveSubmission(context.Context, string, string, int) error { return nil }
func (r *fakeRepository) ReserveSMTPSubmission(context.Context, string, string, [32]byte) error {
	r.note("reserve")
	return r.reserveErr
}

func (r *fakeRepository) Commit(_ context.Context, record acceptance.Record) (submission.Receipt, error) {
	r.note("commit")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.commitErr != nil {
		return submission.Receipt{}, r.commitErr
	}
	r.commitCount++
	r.records = append(r.records, record)
	receipt := submission.Receipt{ID: fmt.Sprintf("%032x", r.commitCount), AcceptedAt: time.Now()}
	if record.IdempotencyKey != "" {
		r.replays[record.IdempotencyKey] = receipt
	}
	return receipt, nil
}

type memObjects struct{ puts int }

func (o *memObjects) Put(context.Context, string, []byte) error { o.puts++; return nil }

type allow bool

func (a allow) Allows(string) bool { return bool(a) }

type fakeSigning struct {
	err   error
	calls []string
}

func (s *fakeSigning) Ready(_ context.Context, app, domain string) error {
	s.calls = append(s.calls, app+"/"+domain)
	return s.err
}

type staticSecret struct {
	secret []byte
	err    error
	calls  int
}

func (s *staticSecret) Secret(context.Context) ([]byte, error) { s.calls++; return s.secret, s.err }

type rig struct {
	repo    *fakeRepository
	objects *memObjects
	signing *fakeSigning
	replay  *staticSecret
	service *Service
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{repo: newFakeRepository(), objects: &memObjects{}, signing: &fakeSigning{}, replay: &staticSecret{secret: []byte("0123456789abcdef0123456789abcdef")}}
	accept, err := acceptance.New(r.repo, r.objects, []byte("0123456789abcdef0123456789abcdef"), allow(true))
	if err != nil {
		t.Fatal(err)
	}
	r.service = &Service{Repository: r.repo, Acceptance: accept, Signing: r.signing, Replay: r.replay}
	return r
}

var fence = submission.Fence{WorkerID: "worker-1", ControllerID: "controller-1", Epoch: 3}

const dated = "From: sender@example.com\r\nTo: rcpt@example.net\r\nSubject: Hi\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\n\r\nbody\r\n"
const withID = "From: sender@example.com\r\nTo: rcpt@example.net\r\nMessage-ID: <one@client.example.org>\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\n\r\nbody\r\n"
const neither = "From: sender@example.com\r\nTo: rcpt@example.net\r\n\r\nbody\r\n"

func message(body string) Message {
	return Message{TransferID: "t", CredentialID: credID, Secret: secret, MailFrom: "sender@example.com", Rcpts: []string{"rcpt@example.net"}, ClientIP: netip.MustParseAddr("192.0.2.9"), TLS: true, Body: []byte(body)}
}

func (r *rig) submit(body string) Result {
	return r.service.Submit(context.Background(), fence, message(body))
}

func (r *rig) callsString() string { return strings.Join(r.repo.calls, ",") }

func TestVerifyMapsTheCredentialOnly(t *testing.T) {
	r := newRig(t)
	ip := netip.MustParseAddr("192.0.2.9")
	if got := r.service.Verify(context.Background(), credID, secret, ip); got != AuthOK || r.callsString() != "verify,touch" {
		t.Fatal(got, r.callsString())
	}
	// A paused app is not an authentication failure.
	r.repo.active = false
	if got := r.service.Verify(context.Background(), credID, secret, ip); got != AuthOK {
		t.Fatal("a paused app's credential must authenticate", got)
	}
	// Touch is best effort.
	r.repo.touchErr = errors.New("lock timeout")
	if got := r.service.Verify(context.Background(), credID, secret, ip); got != AuthOK {
		t.Fatal(got)
	}
	for err, want := range map[error]AuthResult{
		submission.ErrInvalidKey: AuthInvalid, postgres.ErrRuntimeClosed: AuthUnavailable, errors.New("db down"): AuthTempfail,
	} {
		r.repo.verifyErr = err
		if got := r.service.Verify(context.Background(), credID, secret, ip); got != want {
			t.Fatal(err, got)
		}
	}
	r.repo.verifyErr = nil
	if got := r.service.Verify(context.Background(), credID, "wrong", ip); got != AuthInvalid {
		t.Fatal(got)
	}
	if got := r.service.Verify(context.Background(), "x", secret, ip); got != AuthInvalid {
		t.Fatal(got)
	}
}

func TestSubmitOutcomeTable(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*rig, *Message)
		body  string
		want  Outcome
	}{
		{"accepted", nil, dated, Accepted},
		{"wrong secret", func(_ *rig, m *Message) { m.Secret = "no" }, dated, CredentialInvalid},
		{"unknown credential", func(_ *rig, m *Message) { m.CredentialID = strings.Repeat("9", 32) }, dated, CredentialInvalid},
		{"gate closed", func(r *rig, _ *Message) { r.repo.verifyErr = postgres.ErrRuntimeClosed }, dated, Unavailable},
		{"database error", func(r *rig, _ *Message) { r.repo.verifyErr = errors.New("down") }, dated, Tempfail},
		{"non-ASCII header", nil, "From: sénder@example.com\r\nTo: rcpt@example.net\r\n\r\nbody\r\n", ContentNotSupported},
		{"bare CR", nil, "From: sender@example.com\r\nTo: rcpt@example.net\r\n\r\nbo\rdy\r\n", ContentNotSupported},
		{"header without a colon", nil, "From sender@example.com\r\n\r\nbody\r\n", MessageInvalid},
		{"duplicate hold header", nil, "From: sender@example.com\r\nX-Thinpost-Hold: true\r\nX-Thinpost-Hold: true\r\n\r\nb\r\n", MessageInvalid},
		{"require-tls header", nil, "From: sender@example.com\r\nX-Thinpost-Require-TLS: true\r\n\r\nb\r\n", MessageInvalid},
		{"invalid enhanced protection", nil, "From: sender@example.com\r\nX-Thinpost-Enhanced-Protection: maybe\r\n\r\nb\r\n", MessageInvalid},
		{"enhanced protection without TLS", func(_ *rig, m *Message) { m.TLS = false }, "From: sender@example.com\r\nX-Thinpost-Enhanced-Protection: true\r\n\r\nb\r\n", EncryptionRequired},
		{"enhanced protection with TLS", nil, "From: sender@example.com\r\nX-Thinpost-Enhanced-Protection: true\r\n\r\nb\r\n", Accepted},
		{"over the raw bound", nil, "From: sender@example.com\r\n\r\n" + strings.Repeat("a\r\n", 3_000_000), TooLarge},
		{"bad recipient", func(_ *rig, m *Message) { m.Rcpts = []string{"not an address"} }, dated, MessageInvalid},
		{"no recipients", func(_ *rig, m *Message) { m.Rcpts = nil }, dated, MessageInvalid},
		{"paused app", func(r *rig, _ *Message) { r.repo.active = false }, dated, AppPaused},
		{"from not permitted", func(r *rig, _ *Message) { r.repo.verified = nil }, dated, FromNotPermitted},
		{"policy misconfigured", func(r *rig, _ *Message) {
			r.repo.policy = rawmime.Policy{Mode: rawmime.FromDomain, Domain: "unverified.example.org"}
		}, dated, Misconfigured},
		{"signing not ready", func(r *rig, _ *Message) { r.signing.err = errors.New("no key") }, dated, Misconfigured},
		{"policy lookup fails", func(r *rig, _ *Message) { r.repo.policyErr = errors.New("db") }, dated, Tempfail},
		{"rate limited", func(r *rig, _ *Message) { r.repo.reserveErr = submission.ErrRateLimited }, dated, RateLimited},
		{"paused at reservation", func(r *rig, _ *Message) { r.repo.reserveErr = submission.ErrAppPaused }, dated, AppPaused},
		{"revoked at reservation", func(r *rig, _ *Message) { r.repo.reserveErr = submission.ErrInvalidKey }, dated, CredentialInvalid},
		{"quota", func(r *rig, _ *Message) { r.repo.commitErr = submission.ErrQuotaExceeded }, dated, QuotaExceeded},
		{"backlog", func(r *rig, _ *Message) { r.repo.commitErr = submission.ErrBacklogExceeded }, dated, Backlog},
		{"lease lost", func(r *rig, _ *Message) { r.repo.commitErr = delivery.ErrLeaseInvalid }, dated, Tempfail},
		{"idempotency conflict", func(r *rig, _ *Message) { r.repo.commitErr = submission.ErrIdempotencyConflict }, dated, Tempfail},
		{"commit invalid", func(r *rig, _ *Message) { r.repo.commitErr = submission.ErrInvalidMessage }, dated, MessageInvalid},
		{"commit invalid request", func(r *rig, _ *Message) { r.repo.commitErr = submission.ErrInvalidRequest }, dated, MessageInvalid},
		{"gate closed at commit", func(r *rig, _ *Message) { r.repo.commitErr = postgres.ErrRuntimeClosed }, dated, Unavailable},
		{"storage error", func(r *rig, _ *Message) { r.repo.commitErr = context.DeadlineExceeded }, dated, Tempfail},
		{"replay secret unavailable", func(r *rig, _ *Message) { r.replay.err = errors.New("keyring") }, dated, Tempfail},
		{"replay lookup fails", func(r *rig, _ *Message) { r.repo.replayErr = errors.New("db") }, dated, Tempfail},
		{"allowlist", nil, dated, RecipientNotPermitted},
	}
	for _, c := range cases {
		r := newRig(t)
		m := message(c.body)
		if c.name == "allowlist" {
			accept, err := acceptance.New(r.repo, r.objects, []byte("0123456789abcdef0123456789abcdef"), allow(false))
			if err != nil {
				t.Fatal(err)
			}
			r.service.Acceptance = accept
		}
		if c.setup != nil {
			c.setup(r, &m)
		}
		got := r.service.Submit(context.Background(), fence, m)
		if got.Outcome != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got.Outcome, c.want)
		}
		if (got.Outcome == Accepted) != (got.MessageID != "") {
			t.Fatalf("%s: message ID %q for %v", c.name, got.MessageID, got.Outcome)
		}
	}
}

// A replay hit is answered before the pause, rate, policy, signing and size
// checks, so a retry after a lost acknowledgement succeeds whatever changed.
func TestReplayLookupPrecedesEveryOtherCheck(t *testing.T) {
	r := newRig(t)
	first := r.submit(withID)
	if first.Outcome != Accepted || r.repo.commitCount != 1 {
		t.Fatal(first)
	}
	// Now everything that could refuse a fresh message refuses.
	r.repo.active = false
	r.repo.reserveErr = submission.ErrRateLimited
	r.repo.verified = nil
	r.repo.policyErr = errors.New("policy changed")
	r.repo.commitErr = submission.ErrQuotaExceeded
	r.signing.err = errors.New("signing broken")
	r.repo.calls = nil
	r.signing.calls = nil
	again := r.submit(withID)
	if again.Outcome != Accepted || again.MessageID != first.MessageID {
		t.Fatal("a replay must return the original ID", again)
	}
	if strings.Contains(r.callsString(), "policy") || strings.Contains(r.callsString(), "reserve") || strings.Contains(r.callsString(), "commit") || len(r.signing.calls) != 0 {
		t.Fatalf("a replay reached later stages: %s %v", r.callsString(), r.signing.calls)
	}
	if r.callsString() != "verify,lookup" {
		t.Fatal(r.callsString())
	}
	// A fresh message on the paused app is refused.
	if fresh := r.submit(strings.Replace(withID, "one@", "two@", 1)); fresh.Outcome != AppPaused {
		t.Fatal("a fresh message on a paused app", fresh)
	}
	// A replay with a revoked credential is not answered: the credential comes first.
	r.repo.verifyErr = submission.ErrInvalidKey
	if got := r.submit(withID); got.Outcome != CredentialInvalid {
		t.Fatal(got)
	}
}

func TestReplayKindsAndKeys(t *testing.T) {
	r := newRig(t)
	for body, prefix := range map[string]string{withID: "smtp1:", dated: "smtp-c1:", neither: ""} {
		r.repo.records = nil
		if got := r.submit(body); got.Outcome != Accepted {
			t.Fatal(got)
		}
		key := r.repo.records[0].IdempotencyKey
		if prefix == "" && (key != "" || r.repo.records[0].IdempotencyTTL != 0) {
			t.Fatal("a message with neither header has an idempotency row", key)
		}
		if prefix != "" && (!strings.HasPrefix(key, prefix) || len(key) != len(prefix)+64) {
			t.Fatal(prefix, key)
		}
		if prefix == "smtp1:" && r.repo.records[0].IdempotencyTTL != 24*time.Hour || prefix == "smtp-c1:" && r.repo.records[0].IdempotencyTTL != 10*time.Minute {
			t.Fatal("window", r.repo.records[0].IdempotencyTTL)
		}
	}
	// A Date-only retry replays; a message with neither never does.
	r.repo.commitCount = 0
	r.repo.replays = map[string]submission.Receipt{}
	a, b := r.submit(dated), r.submit(dated)
	if a.MessageID != b.MessageID || r.repo.commitCount != 1 {
		t.Fatal("kind C retry", a, b)
	}
	c, d := r.submit(neither), r.submit(neither)
	if c.MessageID == d.MessageID || c.MessageID == a.MessageID {
		t.Fatal("neither header: two messages", c, d)
	}
	// Two cron alerts that differ only in Date are two messages.
	other := strings.Replace(dated, "12:00:00", "12:05:00", 1)
	if e := r.submit(other); e.MessageID == a.MessageID {
		t.Fatal("different Date, same message")
	}
	// A Message-ID reused with different content has its own row.
	one := r.submit(withID)
	reused := strings.Replace(withID, "body", "different body", 1)
	two := r.submit(reused)
	if one.MessageID == two.MessageID || r.submit(withID).MessageID != one.MessageID || r.submit(reused).MessageID != two.MessageID {
		t.Fatal("Message-ID reuse with different content", one, two)
	}
}

func TestHeldReplayReturnsHeld(t *testing.T) {
	r := newRig(t)
	held := "From: sender@example.com\r\nTo: rcpt@example.net\r\nX-Thinpost-Hold: true\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\n\r\nbody\r\n"
	first := r.submit(held)
	if first.Outcome != Accepted || !first.Held {
		t.Fatal(first)
	}
	again := r.submit(held)
	if again.Outcome != Accepted || !again.Held || again.MessageID != first.MessageID {
		t.Fatal("a replay of a held message must report held", again)
	}
	if plain := r.submit(dated); plain.Held {
		t.Fatal("a message without the header is not held")
	}
	// And after the app is paused, still.
	r.repo.active = false
	if again = r.submit(held); again.Outcome != Accepted || !again.Held {
		t.Fatal(again)
	}
}

func TestCandidateDomainsCoverFromMailFromAndSender(t *testing.T) {
	r := newRig(t)
	body := "From: a@Alpha.example.com\r\nSender: s@sender.example.org\r\nTo: rcpt@example.net\r\n\r\nb\r\n"
	m := message(body)
	m.MailFrom = "bounce@mailfrom.example.net"
	r.service.Submit(context.Background(), fence, m)
	got := strings.Join(r.repo.candidates, ",")
	for _, want := range []string{"alpha.example.com", "mailfrom.example.net", "sender.example.org"} {
		if !strings.Contains(got, want) {
			t.Fatalf("candidates %q lack %s", got, want)
		}
	}
}

func TestSubmitLogsTheOutcomeWithoutContent(t *testing.T) {
	var lines []string
	restore := oplog.SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	defer restore()
	r := newRig(t)
	m := message(dated)
	m.TransferID = strings.Repeat("e", 32)
	r.service.Submit(context.Background(), fence, m)
	m.Secret = "wrong"
	r.service.Submit(context.Background(), fence, m)
	if len(lines) < 2 || !strings.Contains(strings.Join(lines, "\n"), "submission result=accepted app="+appID+" transfer="+m.TransferID) || !strings.Contains(strings.Join(lines, "\n"), "result=credential_invalid app=invalid") {
		t.Fatal(lines)
	}
	for _, line := range lines {
		for _, leak := range []string{"example.net", "example.com", secret, "wrong", "body"} {
			if strings.Contains(line, leak) {
				t.Fatalf("log line leaks %q: %s", leak, line)
			}
		}
	}
}

// The digest, spelled out by hand: this pins the exact construction.
func TestDigestConstruction(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	raw := []byte("From: a@example.com\r\n\r\nx\r\n")
	rawSum := sha256.Sum256(raw)
	lp := func(s string) []byte { return append(binary.AppendUvarint(nil, uint64(len(s))), s...) }
	common := append(lp(appID), lp("bounce@example.com")...)
	common = binary.AppendUvarint(common, 2)
	common = append(common, lp("A@example.net")...)
	common = append(common, lp("b@example.org")...)

	m := hmac.New(sha256.New, key)
	m.Write([]byte("thinpost-smtp-replay-v1\x00msgid\x00"))
	m.Write(common)
	m.Write(lp("<id@client.example.org>"))
	m.Write(rawSum[:])
	var wantM [32]byte
	copy(wantM[:], m.Sum(nil))
	c := hmac.New(sha256.New, key)
	c.Write([]byte("thinpost-smtp-replay-v1\x00content\x00"))
	c.Write(common)
	c.Write(rawSum[:])
	var wantC [32]byte
	copy(wantC[:], c.Sum(nil))

	keys, ok := RecipientKeys([]string{"b@Example.ORG", "A@EXAMPLE.net", "b@example.org"})
	if !ok || strings.Join(keys, ",") != "A@example.net,b@example.org" {
		t.Fatal(keys, ok)
	}
	if got := Digest(key, acceptance.ReplayMessageID, appID, "Bounce@Example.com", keys, "<id@client.example.org>", raw); got == wantM {
		t.Fatal("the local part of MAIL FROM is case-sensitive")
	}
	if got := Digest(key, acceptance.ReplayMessageID, appID, "bounce@EXAMPLE.com", keys, "<id@client.example.org>", raw); got != wantM {
		t.Fatalf("kind M digest differs from the specification: %x vs %x", got, wantM)
	}
	if got := Digest(key, acceptance.ReplayContent, appID, "bounce@example.com", keys, "ignored", raw); got != wantC {
		t.Fatal("kind C digest differs from the specification")
	}
	// Kinds never collide, the secret matters, and every field matters.
	if wantM == wantC {
		t.Fatal("kind M and C collide")
	}
	base := Digest(key, acceptance.ReplayContent, appID, "bounce@example.com", keys, "", raw)
	for name, other := range map[string][32]byte{
		"another secret":   Digest([]byte("fedcba9876543210fedcba9876543210"), acceptance.ReplayContent, appID, "bounce@example.com", keys, "", raw),
		"another app":      Digest(key, acceptance.ReplayContent, strings.Repeat("b", 32), "bounce@example.com", keys, "", raw),
		"another sender":   Digest(key, acceptance.ReplayContent, appID, "other@example.com", keys, "", raw),
		"null sender":      Digest(key, acceptance.ReplayContent, appID, "<>", keys, "", raw),
		"another envelope": Digest(key, acceptance.ReplayContent, appID, "bounce@example.com", keys[:1], "", raw),
		"other bytes":      Digest(key, acceptance.ReplayContent, appID, "bounce@example.com", keys, "", append(raw, 'x')),
	} {
		if other == base {
			t.Fatalf("the digest ignores %s", name)
		}
	}
	if Digest(key, acceptance.ReplayContent, appID, "", keys, "", raw) != Digest(key, acceptance.ReplayContent, appID, "<>", keys, "", raw) {
		t.Fatal("the two spellings of the null path must agree")
	}
	// Field boundaries are length-prefixed: moving a byte between fields changes the digest.
	if Digest(key, acceptance.ReplayContent, "ab", "c@example.com", nil, "", raw) == Digest(key, acceptance.ReplayContent, "a", "bc@example.com", nil, "", raw) {
		t.Fatal("ambiguous encoding")
	}
}

func TestReplayKindOf(t *testing.T) {
	for body, want := range map[string]acceptance.ReplayKind{withID: acceptance.ReplayMessageID, dated: acceptance.ReplayContent, neither: acceptance.ReplayNone,
		"From: a@example.com\r\nMessage-ID: bad id\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\n\r\nx\r\n": acceptance.ReplayContent} {
		scanned, err := rawmime.Scan([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if got := ReplayKindOf(scanned); got != want {
			t.Fatalf("%q: %v want %v", body, got, want)
		}
	}
}
