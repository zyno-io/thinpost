package feedback

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/zyno-io/thinpost/internal/oplog"
)

const (
	deliveryID  = "00112233445566778899aabbccddeeff"
	messageID   = "ffeeddccbbaa99887766554433221100"
	appID       = "0123456789abcdef0123456789abcdef"
	bounceToken = "abcdef0123456789abcdef0123456789"
	senderHost  = "mail.sender.example"
)

type fakeRepo struct {
	mu        sync.Mutex
	reporters []string
	delivery  *Delivery
	// shared makes the message use the shared Message-ID: the delivery and its
	// siblings are found by the message ID instead of a delivery ID.
	shared    bool
	siblings  []Delivery
	records   map[string]bool
	counts    map[Result]int
	failCount bool
	failAll   error
	recorded  []string
}

func newRepo() *fakeRepo {
	hash := sha256.Sum256([]byte(bounceToken))
	return &fakeRepo{
		reporters: []string{"arf.mail.yahoo.com"},
		delivery:  &Delivery{ID: deliveryID, MessageID: messageID, AppID: appID, SenderDomain: senderHost, State: "accepted", BounceTokenHash: hash[:]},
		records:   map[string]bool{},
		counts:    map[Result]int{},
	}
}
func (r *fakeRepo) IsMailbox(context.Context, [32]byte) (bool, error) { return true, r.failAll }
func (r *fakeRepo) Reporters(context.Context) ([]string, error)       { return r.reporters, r.failAll }
func (r *fakeRepo) Candidates(_ context.Context, id string) ([]Delivery, error) {
	if r.failAll != nil {
		return nil, r.failAll
	}
	if r.delivery == nil {
		return nil, nil
	}
	if !r.shared {
		if r.delivery.ID != id {
			return nil, nil
		}
		return []Delivery{*r.delivery}, nil
	}
	if r.delivery.MessageID != id {
		return nil, nil
	}
	return append([]Delivery{*r.delivery}, r.siblings...), nil
}
func (r *fakeRepo) Record(_ context.Context, id, reporter string) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failAll != nil {
		return "", r.failAll
	}
	result := Accepted
	if r.records[id] {
		result = Duplicate
	}
	r.records[id] = true
	r.recorded = append(r.recorded, reporter)
	r.counts[result]++
	return result, nil
}
func (r *fakeRepo) Count(_ context.Context, result Result) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failCount || r.failAll != nil {
		return errors.New("count failed")
	}
	r.counts[result]++
	return nil
}

// signer is a reporter's DKIM identity with a fake DNS.
type signer struct {
	domain  string
	key     ed25519.PrivateKey
	records map[string][]string
	lookup  func(ctx context.Context, name string) ([]string, error)
}

func newSigner(t *testing.T, domain string) *signer {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := &signer{domain: domain, key: key, records: map[string][]string{}}
	s.records["sel._domainkey."+domain] = []string{"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)}
	return s
}

func (s *signer) resolve(ctx context.Context, name string) ([]string, error) {
	if s.lookup != nil {
		return s.lookup(ctx, name)
	}
	if rec, ok := s.records[name]; ok {
		return rec, nil
	}
	return nil, errors.New("no such host")
}

func (s *signer) sign(t *testing.T, message string) []byte {
	t.Helper()
	var out bytes.Buffer
	err := dkim.Sign(&out, strings.NewReader(message), &dkim.SignOptions{Domain: s.domain, Selector: "sel", Signer: s.key, HeaderKeys: []string{"From", "To", "Subject", "Date", "Content-Type"}})
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// original builds the embedded original of a Thinpost message as a reporter sees it.
func original(messageID, returnPath string) string {
	var b strings.Builder
	if returnPath != "" {
		b.WriteString("Return-Path: " + returnPath + "\r\n")
	}
	b.WriteString("Received: from mail.sender.example by mx.example.net; Wed, 11 Nov 2026 12:00:00 +0000\r\n")
	b.WriteString("From: Sender <sender@sender.example>\r\n")
	b.WriteString("To: redacted@example.net\r\n")
	b.WriteString("Subject: Your receipt\r\n")
	if messageID != "" {
		b.WriteString("Message-ID: " + messageID + "\r\n")
	}
	return b.String()
}

type arfOptions struct {
	feedbackType string
	originalType string // message/rfc822 or text/rfc822-headers
	original     string
	omitOriginal bool
	omitReport   bool
	body         string
	wrapAlt      int // number of nested multipart/alternative wrappers around the text part
	extraParts   int
}

func arf(o arfOptions) string {
	if o.feedbackType == "" {
		o.feedbackType = "abuse"
	}
	if o.originalType == "" {
		o.originalType = "message/rfc822"
	}
	var b strings.Builder
	b.WriteString("From: \"Yahoo! Mail AntiSpam Feedback\" <feedback@arf.mail.yahoo.com>\r\nTo: b+0123456789abcdef0123456789abcdef@mx.example.net\r\nSubject: FW: Your receipt\r\nDate: Wed, 11 Nov 2026 12:05:00 +0000\r\nMIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/report; report-type=feedback-report; boundary=\"outer\"\r\n\r\n")
	b.WriteString("--outer\r\nContent-Type: text/plain\r\n\r\nThis is an email abuse report.\r\n")
	for i := 0; i < o.wrapAlt; i++ {
		fmt.Fprintf(&b, "--outer\r\nContent-Type: multipart/alternative; boundary=\"alt%d\"\r\n\r\n--alt%d\r\nContent-Type: text/plain\r\n\r\nx\r\n--alt%d--\r\n", i, i, i)
	}
	for i := 0; i < o.extraParts; i++ {
		b.WriteString("--outer\r\nContent-Type: text/plain\r\n\r\nextra\r\n")
	}
	if !o.omitReport {
		b.WriteString("--outer\r\nContent-Type: message/feedback-report\r\n\r\nFeedback-Type: " + o.feedbackType + "\r\nUser-Agent: Test/1.0\r\nVersion: 1\r\nOriginal-Rcpt-To: victim@example.net\r\n")
	}
	if !o.omitOriginal {
		b.WriteString("--outer\r\nContent-Type: " + o.originalType + "\r\n\r\n" + o.original + "\r\n")
		if o.body != "" {
			b.WriteString(o.body + "\r\n")
		}
	}
	b.WriteString("--outer--\r\n")
	return b.String()
}

func goodOriginal() string {
	return original("<"+deliveryID+"@"+senderHost+">", "<b+"+bounceToken+"@mx.example.net>")
}

type harness struct {
	repo   *fakeRepo
	signer *signer
	svc    *Service
	logs   []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{repo: newRepo(), signer: newSigner(t, "arf.mail.yahoo.com")}
	h.svc = &Service{Repository: h.repo, LookupTXT: h.signer.resolve, Budget: 500 * time.Millisecond}
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) { h.logs = append(h.logs, fmt.Sprintf(format, args...)) }))
	return h
}

func (h *harness) process(t *testing.T, signed bool, message string) error {
	t.Helper()
	body := []byte(message)
	if signed {
		body = h.signer.sign(t, message)
	}
	return h.svc.Process(context.Background(), body)
}

func (h *harness) expect(t *testing.T, result Result) {
	t.Helper()
	if h.repo.counts[result] != 1 {
		t.Fatalf("want one %s, counts %v", result, h.repo.counts)
	}
	total := 0
	for _, n := range h.repo.counts {
		total += n
	}
	if total != 1 {
		t.Fatalf("want exactly one counter, got %v", h.repo.counts)
	}
}

func TestAcceptedYahooShapedReport(t *testing.T) {
	h := newHarness(t)
	full := arf(arfOptions{original: goodOriginal(), body: "Hello, victim@example.net, this is the body."})
	if err := h.process(t, true, full); err != nil {
		t.Fatal(err)
	}
	h.expect(t, Accepted)
	if len(h.repo.recorded) != 1 || h.repo.recorded[0] != "arf.mail.yahoo.com" || !h.repo.records[deliveryID] {
		t.Fatalf("not recorded for the verified signer: %v", h.repo.recorded)
	}
	joined := strings.Join(h.logs, "\n")
	if joined != "feedback channel=arf result=accepted app="+appID+" message="+messageID+" delivery="+deliveryID {
		t.Fatalf("log %q", joined)
	}
	for _, forbidden := range []string{"@", "victim", "yahoo", "receipt"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("log contains %q", forbidden)
		}
	}
}

func TestAcceptedJMRPShapedRedactedReport(t *testing.T) {
	h := newHarness(t)
	// A redacted original: only headers, no recipient, as text/rfc822-headers.
	redacted := strings.Replace(goodOriginal(), "To: redacted@example.net\r\n", "To: \r\n", 1)
	message := strings.Replace(arf(arfOptions{originalType: "text/rfc822-headers", original: redacted}), "Original-Rcpt-To: victim@example.net\r\n", "", 1)
	h.repo.reporters = []string{"mail.yahoo.com"} // an allowlisted parent covers a subdomain signer
	if err := h.process(t, true, message); err != nil {
		t.Fatal(err)
	}
	h.expect(t, Accepted)
}

func TestDuplicateCountsDuplicate(t *testing.T) {
	h := newHarness(t)
	message := arf(arfOptions{original: goodOriginal()})
	for i := 0; i < 2; i++ {
		if err := h.process(t, true, message); err != nil {
			t.Fatal(err)
		}
	}
	if h.repo.counts[Accepted] != 1 || h.repo.counts[Duplicate] != 1 {
		t.Fatalf("counts %v", h.repo.counts)
	}
}

func TestRejections(t *testing.T) {
	good := goodOriginal()
	tests := []struct {
		name   string
		mutate func(h *harness) string
		signed bool
		want   Result
	}{
		{"not a report", func(*harness) string { return "From: a@b.example\r\nSubject: hi\r\n\r\nplain" }, true, Unparsable},
		{"wrong report type", func(*harness) string {
			return strings.Replace(arf(arfOptions{original: good}), "multipart/report; report-type=feedback-report", "multipart/mixed", 1)
		}, true, Unparsable},
		{"missing original", func(*harness) string { return arf(arfOptions{omitOriginal: true}) }, true, Unparsable},
		{"missing feedback part", func(*harness) string { return arf(arfOptions{original: good, omitReport: true}) }, true, Unparsable},
		{"non-abuse type", func(*harness) string { return arf(arfOptions{original: good, feedbackType: "not-spam"}) }, true, Ignored},
		{"fraud type", func(*harness) string { return arf(arfOptions{original: good, feedbackType: "fraud"}) }, true, Ignored},
		{"unsigned", func(*harness) string { return arf(arfOptions{original: good}) }, false, Unauthenticated},
		{"no reporters", func(h *harness) string { h.repo.reporters = nil; return arf(arfOptions{original: good}) }, true, Unauthenticated},
		{"wrong reporter", func(h *harness) string {
			h.repo.reporters = []string{"other.example"}
			return arf(arfOptions{original: good})
		}, true, Unauthenticated},
		{"suffix lookalike", func(h *harness) string {
			h.repo.reporters = []string{"yahoo.com.evil"}
			return arf(arfOptions{original: good})
		}, true, Unauthenticated},
		{"bad message id shape", func(*harness) string {
			return arf(arfOptions{original: original("<not-a-delivery@"+senderHost+">", "")})
		}, true, Uncorrelated},
		{"no message id", func(*harness) string { return arf(arfOptions{original: original("", "")}) }, true, Uncorrelated},
		{"unknown delivery", func(*harness) string {
			return arf(arfOptions{original: original("<"+strings.Repeat("1", 32)+"@"+senderHost+">", "")})
		}, true, Uncorrelated},
		{"sender domain mismatch", func(*harness) string {
			return arf(arfOptions{original: original("<"+deliveryID+"@other.example>", "")})
		}, true, Uncorrelated},
		{"pre-schema-28 message", func(h *harness) string { h.repo.delivery.SenderDomain = ""; return arf(arfOptions{original: good}) }, true, Uncorrelated},
		{"return-path conflict", func(*harness) string {
			return arf(arfOptions{original: original("<"+deliveryID+"@"+senderHost+">", "<b+"+strings.Repeat("a", 32)+"@mx.example.net>")})
		}, true, Uncorrelated},
		{"return-path token with no bounce address", func(h *harness) string { h.repo.delivery.BounceTokenHash = nil; return arf(arfOptions{original: good}) }, true, Uncorrelated},
		{"delivery not accepted", func(h *harness) string { h.repo.delivery.State = "failed"; return arf(arfOptions{original: good}) }, true, NotDelivered},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			message := tt.mutate(h)
			if err := h.process(t, tt.signed, message); err != nil {
				t.Fatal(err)
			}
			h.expect(t, tt.want)
			if len(h.repo.records) != 0 {
				t.Fatal("a rejected report recorded a complaint")
			}
		})
	}
}

func TestReturnPathRules(t *testing.T) {
	msgID := "<" + deliveryID + "@" + senderHost + ">"
	own := "b+" + bounceToken + "@mx.example.net"
	accepted := map[string]string{
		"absent (redacted by the reporter)": "",
		"own bounce address":                "<" + own + ">",
		"own address, upper case":           "<B+" + strings.ToUpper(bounceToken) + "@MX.EXAMPLE.NET>",
		"SRS0 forwarded":                    "<SRS0=HHHH=TT=mx.example.net=" + "b+" + bounceToken + "@forwarder.example>",
		"SRS1 forwarded":                    "<SRS1=HHH=fwd.example==HHHH=TT=mx.example.net=" + "b+" + bounceToken + "@forwarder2.example>",
	}
	for name, rp := range accepted {
		h := newHarness(t)
		if err := h.process(t, true, arf(arfOptions{original: original(msgID, rp)})); err != nil {
			t.Fatal(err)
		}
		if h.repo.counts[Accepted] != 1 {
			t.Errorf("%s: %v", name, h.repo.counts)
		}
	}
	rejected := map[string]string{
		"null reverse path":     "<>",
		"foreign address":       "<bounce@elsewhere.example>",
		"attacker bounce shape": "<b+" + strings.Repeat("a", 32) + "@mx.example.net>",
		"SRS of a foreign":      "<SRS0=HHHH=TT=other.example=user@forwarder.example>",
		"SRS with other token":  "<SRS0=HHHH=TT=mx.example.net=b+" + strings.Repeat("c", 32) + "@forwarder.example>",
		"unparsable":            "garbage",
	}
	for name, rp := range rejected {
		h := newHarness(t)
		if err := h.process(t, true, arf(arfOptions{original: original(msgID, rp)})); err != nil {
			t.Fatal(err)
		}
		if h.repo.counts[Uncorrelated] != 1 || len(h.repo.records) != 0 {
			t.Errorf("%s: %v", name, h.repo.counts)
		}
	}
}

func TestMalformedOriginalLineDoesNotHideCorrelation(t *testing.T) {
	h := newHarness(t)
	bad := "X-Bad\x01 header without colon\r\nX-Ctl: a\x00b\r\n" + goodOriginal() + "Folded: one\r\n two\r\n"
	if err := h.process(t, true, arf(arfOptions{original: bad})); err != nil {
		t.Fatal(err)
	}
	h.expect(t, Accepted)
	// A folded Message-ID value is unfolded.
	h = newHarness(t)
	folded := strings.Replace(goodOriginal(), "Message-ID: <", "Message-ID:\r\n <", 1)
	if err := h.process(t, true, arf(arfOptions{original: folded})); err != nil {
		t.Fatal(err)
	}
	h.expect(t, Accepted)
}

func TestNoDNSQueryForAnUnlistedSigner(t *testing.T) {
	h := newHarness(t)
	h.repo.reporters = []string{"other.example"}
	queried := 0
	h.signer.lookup = func(context.Context, string) ([]string, error) {
		queried++
		return nil, errors.New("must not be called")
	}
	if err := h.process(t, true, arf(arfOptions{original: goodOriginal()})); err != nil {
		t.Fatal(err)
	}
	h.expect(t, Unauthenticated)
	if queried != 0 {
		t.Fatal("DNS was queried for a signer that is not allowlisted")
	}
}

func TestSignatureMustCoverContentTypeAndUseASCII(t *testing.T) {
	h := newHarness(t)
	message := arf(arfOptions{original: goodOriginal()})
	var out bytes.Buffer
	if err := dkim.Sign(&out, strings.NewReader(message), &dkim.SignOptions{Domain: h.signer.domain, Selector: "sel", Signer: h.signer.key, HeaderKeys: []string{"From", "To", "Subject", "Date"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Process(context.Background(), out.Bytes()); err != nil {
		t.Fatal(err)
	}
	h.expect(t, Unauthenticated)
	// A non-ASCII lookalike of the allowlisted domain is never matched (the
	// Kelvin sign lowercases to an ASCII k).
	other := newHarness(t)
	other.repo.reporters = []string{"kelvin.example"}
	lookalike := newSigner(t, "\u212Aelvin.example")
	body := lookalike.sign(t, message)
	other.svc.LookupTXT = lookalike.resolve
	if err := other.svc.Process(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	other.expect(t, Unauthenticated)
}

func TestSignatureFromTamperedOrWrongKeyIsUnauthenticated(t *testing.T) {
	h := newHarness(t)
	signed := h.signer.sign(t, arf(arfOptions{original: goodOriginal()}))
	tampered := bytes.Replace(signed, []byte("Subject: FW: Your receipt"), []byte("Subject: FW: Another"), 1)
	if err := h.svc.Process(context.Background(), tampered); err != nil {
		t.Fatal(err)
	}
	h.expect(t, Unauthenticated)

	other := newHarness(t)
	imposter := newSigner(t, "arf.mail.yahoo.com") // different key than the published record
	body := imposter.sign(t, arf(arfOptions{original: goodOriginal()}))
	if err := other.svc.Process(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	other.expect(t, Unauthenticated)
}

func TestDNSBudgetAndTemporaryFailureRetry(t *testing.T) {
	h := newHarness(t)
	h.signer.lookup = func(ctx context.Context, name string) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	err := h.process(t, true, arf(arfOptions{original: goodOriginal()}))
	if err == nil {
		t.Fatal("a DNS timeout for an allowlisted signer must retry, not discard")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("DKIM lookup was not bounded: %v", time.Since(start))
	}
	if len(h.repo.counts) != 0 {
		t.Fatalf("timeout counted: %v", h.repo.counts)
	}
	// A timeout for a signer that is not allowlisted is just unauthenticated.
	h.repo.reporters = []string{"other.example"}
	if err = h.process(t, true, arf(arfOptions{original: goodOriginal()})); err != nil {
		t.Fatal(err)
	}
	h.expect(t, Unauthenticated)
}

func TestMIMEBounds(t *testing.T) {
	good := goodOriginal()
	h := newHarness(t)
	// Nested multipart parts are tolerated up to depth 3; here two siblings at depth 2.
	if err := h.process(t, true, arf(arfOptions{original: good, wrapAlt: 2})); err != nil {
		t.Fatal(err)
	}
	h.expect(t, Accepted)

	tooMany := newHarness(t)
	if err := tooMany.process(t, true, arf(arfOptions{original: good, extraParts: 16})); err != nil {
		t.Fatal(err)
	}
	tooMany.expect(t, Unparsable)

	deep := newHarness(t)
	nested := "--outer\r\nContent-Type: multipart/mixed; boundary=\"d1\"\r\n\r\n--d1\r\nContent-Type: multipart/mixed; boundary=\"d2\"\r\n\r\n--d2\r\nContent-Type: multipart/mixed; boundary=\"d3\"\r\n\r\n--d3\r\nContent-Type: text/plain\r\n\r\nx\r\n--d3--\r\n--d2--\r\n--d1--\r\n"
	message := strings.Replace(arf(arfOptions{original: good}), "--outer\r\nContent-Type: text/plain", nested+"--outer\r\nContent-Type: text/plain", 1)
	if err := deep.process(t, true, message); err != nil {
		t.Fatal(err)
	}
	deep.expect(t, Unparsable)

	bigHeader := newHarness(t)
	huge := "X-Padding: " + strings.Repeat("a", maxHeaderBlock) + "\r\n" + good
	if err := bigHeader.process(t, true, arf(arfOptions{original: huge})); err != nil {
		t.Fatal(err)
	}
	bigHeader.expect(t, Unparsable)

	bigTop := newHarness(t)
	topMessage := "X-Padding: " + strings.Repeat("a", maxHeaderBlock) + "\r\n" + arf(arfOptions{original: good})
	if err := bigTop.process(t, false, topMessage); err != nil {
		t.Fatal(err)
	}
	bigTop.expect(t, Unparsable)

	// A large original body behind small headers is fine: only headers are read.
	big := newHarness(t)
	if err := big.process(t, true, arf(arfOptions{original: good, body: strings.Repeat("filler line\r\n", 100000)})); err != nil {
		t.Fatal(err)
	}
	big.expect(t, Accepted)
}

func TestClosedGateIsRetryable(t *testing.T) {
	h := newHarness(t)
	h.repo.failAll = errors.New("runtime closed")
	if err := h.process(t, true, arf(arfOptions{original: goodOriginal()})); err == nil {
		t.Fatal("a closed gate must make the sender retry")
	}
	// Even an unparsable report needs its counter: a failed counter is an error.
	h = newHarness(t)
	h.repo.failCount = true
	if err := h.process(t, false, "garbage"); err == nil {
		t.Fatal("a counter failure must be retryable")
	}
	// Only a commit that did not confirm may leak no detail.
	h = newHarness(t)
	h.repo.failAll = errors.New("pq: secret detail victim@example.net")
	err := h.process(t, true, arf(arfOptions{original: goodOriginal()}))
	if err == nil || strings.Contains(err.Error(), "victim") {
		t.Fatalf("error leaks detail: %v", err)
	}
}

// A second, unsigned top-level Content-Type must not re-frame a signed report
// (here: skipping the reporter's parts, and its Return-Path, as preamble).
func TestPrependedContentTypeRejected(t *testing.T) {
	msgID := "<" + deliveryID + "@" + senderHost + ">"
	planted := "\r\n--evil\r\nContent-Type: message/feedback-report\r\n\r\nFeedback-Type: abuse\r\n\r\n" +
		"--evil\r\nContent-Type: text/rfc822-headers\r\n\r\nMessage-ID: " + msgID + "\r\n\r\n--evil--\r\n"
	h := newHarness(t)
	signed := h.signer.sign(t, arf(arfOptions{original: original(msgID, "<x@attacker.example>"), body: planted}))
	for _, prefix := range []string{"Content-Type", "content-type"} {
		h.repo.counts = map[Result]int{}
		evil := append([]byte(prefix+": multipart/report; report-type=feedback-report; boundary=\"evil\"\r\n"), signed...)
		if err := h.svc.Process(context.Background(), evil); err != nil {
			t.Fatal(err)
		}
		if h.repo.counts[Accepted] != 0 || len(h.repo.records) != 0 || h.repo.counts[Unparsable] != 1 {
			t.Fatalf("%s: prepended Content-Type accepted: %v", prefix, h.repo.counts)
		}
	}
}

// A message prepared with the shared Message-ID is correlated by its message
// ID; the Return-Path token picks the reporting recipient.
func TestSharedMessageID(t *testing.T) {
	sharedID := "<" + messageID + "@" + senderHost + ">"
	siblingToken := strings.Repeat("5", 32)
	siblingHash := sha256.Sum256([]byte(siblingToken))
	setup := func(t *testing.T, siblings bool) *harness {
		h := newHarness(t)
		h.repo.shared = true
		if siblings {
			h.repo.siblings = []Delivery{{ID: "99887766554433221100ffeeddccbbaa", MessageID: messageID, AppID: appID, SenderDomain: senderHost, State: "accepted", BounceTokenHash: siblingHash[:]}}
		}
		return h
	}
	ownReturn := "<b+" + bounceToken + "@mx.example.net>"
	t.Run("single recipient, no Return-Path", func(t *testing.T) {
		h := setup(t, false)
		if err := h.process(t, true, arf(arfOptions{original: original(sharedID, "")})); err != nil {
			t.Fatal(err)
		}
		h.expect(t, Accepted)
	})
	t.Run("multi recipient, Return-Path picks the delivery", func(t *testing.T) {
		h := setup(t, true)
		if err := h.process(t, true, arf(arfOptions{original: original(sharedID, ownReturn)})); err != nil {
			t.Fatal(err)
		}
		h.expect(t, Accepted)
		if !h.repo.records[deliveryID] || len(h.repo.records) != 1 {
			t.Fatal("wrong delivery recorded", h.repo.records)
		}
	})
	t.Run("a sibling's token picks the sibling", func(t *testing.T) {
		h := setup(t, true)
		if err := h.process(t, true, arf(arfOptions{original: original(sharedID, "<b+"+siblingToken+"@mx.example.net>")})); err != nil {
			t.Fatal(err)
		}
		h.expect(t, Accepted)
		if !h.repo.records["99887766554433221100ffeeddccbbaa"] || len(h.repo.records) != 1 {
			t.Fatal("wrong delivery recorded", h.repo.records)
		}
	})
	for name, tc := range map[string]struct {
		siblings   bool
		returnPath string
		id         string
	}{
		"multi recipient, no Return-Path": {true, "", sharedID},
		"foreign token":                   {true, "<b+" + strings.Repeat("a", 32) + "@mx.example.net>", sharedID},
		"null Return-Path":                {true, "<>", sharedID},
		"domain mismatch":                 {false, "", "<" + messageID + "@other.example>"},
		"delivery-ID form of shared":      {false, ownReturn, "<" + deliveryID + "@" + senderHost + ">"},
	} {
		t.Run(name, func(t *testing.T) {
			h := setup(t, tc.siblings)
			if err := h.process(t, true, arf(arfOptions{original: original(tc.id, tc.returnPath)})); err != nil {
				t.Fatal(err)
			}
			h.expect(t, Uncorrelated)
			if len(h.repo.records) != 0 {
				t.Fatal("recorded", h.repo.records)
			}
		})
	}
	t.Run("message-ID form of a legacy message", func(t *testing.T) {
		h := newHarness(t)
		if err := h.process(t, true, arf(arfOptions{original: original(sharedID, ownReturn)})); err != nil {
			t.Fatal(err)
		}
		h.expect(t, Uncorrelated)
	})
}
