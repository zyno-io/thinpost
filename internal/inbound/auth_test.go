package inbound

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// fakeDNS answers from tables. A name in block waits for the context (a DNS
// server that never answers); delay slows every answer.
type fakeDNS struct {
	mu    sync.Mutex
	txt   map[string][]string
	ips   map[string][]net.IPAddr
	mx    map[string][]*net.MX
	block map[string]bool
	temp  map[string]bool
	delay time.Duration
	calls int
}

func (f *fakeDNS) wait(ctx context.Context, name string) error {
	f.mu.Lock()
	f.calls++
	block, temp, delay := f.block[name], f.temp[name], f.delay
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if temp {
		return &net.DNSError{Err: "server failure", Name: name, IsTemporary: true}
	}
	return nil
}
func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}
func (f *fakeDNS) LookupTXT(ctx context.Context, name string) ([]string, error) {
	if err := f.wait(ctx, name); err != nil {
		return nil, err
	}
	if v, ok := f.txt[name]; ok {
		return v, nil
	}
	return nil, notFound(name)
}
func (f *fakeDNS) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	if err := f.wait(ctx, name); err != nil {
		return nil, err
	}
	if v, ok := f.mx[name]; ok {
		return v, nil
	}
	return nil, notFound(name)
}
func (f *fakeDNS) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if err := f.wait(ctx, host); err != nil {
		return nil, err
	}
	if v, ok := f.ips[host]; ok {
		return v, nil
	}
	return nil, notFound(host)
}
func (f *fakeDNS) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	return nil, notFound(addr)
}

func testEnvelope(from, helo, ip string) envelope {
	e := envelope{id: strings.Repeat("a", 32), mailFrom: from, helo: helo, rcpts: []string{"user@mail.test"}}
	if ip != "" {
		e.clientIP = netip.MustParseAddr(ip)
	}
	return e
}

func publicKeyRecord(t *testing.T, key *rsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)
}

func signed(t *testing.T, key *rsa.PrivateKey, domain, message string) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := dkim.Sign(&out, strings.NewReader(message), &dkim.SignOptions{Domain: domain, Selector: "s1", Signer: key, HeaderKeys: []string{"From", "To", "Subject"}}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

const plainMessage = "From: Alice <alice@example.org>\r\nTo: user@mail.test\r\nSubject: hi\r\nMessage-ID: <x@example.org>\r\n\r\nhello\r\n"

func run(t *testing.T, dns Resolver, budget time.Duration, e envelope, raw []byte) Authentication {
	t.Helper()
	return Authenticate(context.Background(), dns, budget, e, raw, Parse(raw))
}

func TestSPFResults(t *testing.T) {
	dns := &fakeDNS{txt: map[string][]string{
		"pass.example":   {"v=spf1 ip4:192.0.2.1 -all"},
		"soft.example":   {"v=spf1 ~all"},
		"broken.example": {"v=spf1 ip4:not-an-ip -all"},
		"loop.example":   {"v=spf1 include:loop.example -all"},
		"mx.test":        {"v=spf1 ip4:192.0.2.1 -all"},
	}}
	for name, tc := range map[string]struct {
		from, helo, ip, want, identity string
	}{
		"pass":       {"a@pass.example", "mx.test", "192.0.2.1", "pass", "mailfrom"},
		"fail":       {"a@pass.example", "mx.test", "192.0.2.9", "fail", "mailfrom"},
		"softfail":   {"a@soft.example", "mx.test", "192.0.2.9", "softfail", "mailfrom"},
		"no record":  {"a@none.example", "mx.test", "192.0.2.9", "none", "mailfrom"},
		"permerror":  {"a@broken.example", "mx.test", "192.0.2.1", "permerror", "mailfrom"},
		"lookup cap": {"a@loop.example", "mx.test", "192.0.2.1", "permerror", "mailfrom"},
		"null path":  {"", "mx.test", "192.0.2.1", "pass", "helo"},
		"no ip":      {"a@pass.example", "mx.test", "", "none", ""},
		"bad helo":   {"", "[192.0.2.1]", "192.0.2.1", "none", ""},
	} {
		got := run(t, dns, time.Second, testEnvelope(tc.from, tc.helo, tc.ip), []byte(plainMessage)).SPF
		if got.Result != tc.want || tc.identity != "" && got.Identity != tc.identity {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestDKIMResultsAndSignatureLimit(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dns := &fakeDNS{txt: map[string][]string{"s1._domainkey.example.org": {publicKeyRecord(t, &key.PublicKey)}}}
	good := signed(t, key, "example.org", plainMessage)
	if got := run(t, dns, time.Second, testEnvelope("", "", ""), good); len(got.DKIM) != 1 || got.DKIM[0] != (DKIMResult{"pass", "example.org"}) || got.DKIMSummary() != "pass" {
		t.Fatalf("valid signature: %+v", got.DKIM)
	}
	tampered := bytes.Replace(good, []byte("hello"), []byte("jello"), 1)
	if got := run(t, dns, time.Second, testEnvelope("", "", ""), tampered); got.DKIMSummary() != "fail" {
		t.Fatalf("tampered body: %+v", got.DKIM)
	}
	if got := run(t, dns, time.Second, testEnvelope("", "", ""), []byte(plainMessage)); got.DKIMSummary() != "none" || len(got.DKIM) != 0 {
		t.Fatalf("unsigned: %+v", got.DKIM)
	}
	// A key lookup that fails temporarily is a temperror, not a failure.
	flaky := &fakeDNS{txt: dns.txt, temp: map[string]bool{"s1._domainkey.example.org": true}}
	if got := run(t, flaky, time.Second, testEnvelope("", "", ""), good); got.DKIMSummary() != "temperror" {
		t.Fatalf("temporary DNS failure: %+v", got.DKIM)
	}
	// At most five signatures are verified.
	many := good
	for i := 0; i < 7; i++ {
		many = signed(t, key, "example.org", string(many))
	}
	got := run(t, dns, 3*time.Second, testEnvelope("", "", ""), many)
	if len(got.DKIM) > maxDKIMSignatures || len(got.DKIM) == 0 {
		t.Fatalf("signature limit: %d", len(got.DKIM))
	}
}

func TestDMARCAlignment(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	record := publicKeyRecord(t, &key.PublicKey)
	dns := &fakeDNS{txt: map[string][]string{
		"s1._domainkey.example.org":      {record},
		"s1._domainkey.news.example.org": {record},
		"s1._domainkey.other.example":    {record},
		"_dmarc.example.org":             {"v=DMARC1; p=reject; sp=quarantine"},
		"_dmarc.strict.example":          {"v=DMARC1; p=none; adkim=s; aspf=s"},
		"example.org":                    {"v=spf1 ip4:192.0.2.1 -all"},
		"strict.example":                 {"v=spf1 ip4:192.0.2.1 -all"},
		"bounces.example.org":            {"v=spf1 ip4:192.0.2.1 -all"},
		"mx.test":                        {"v=spf1 -all"},
		"other.example":                  {"v=spf1 ip4:192.0.2.1 -all"},
		"_dmarc.broken.example":          {"v=DMARC1; p=bogus"},
		"_dmarc.nodkim.example":          {"v=DMARC1; p=reject"},
		"nodkim.example":                 {"v=spf1 ip4:192.0.2.1 -all"},
	}}
	from := func(domain string) string {
		return "From: A <a@" + domain + ">\r\nTo: user@mail.test\r\nSubject: s\r\n\r\nbody\r\n"
	}
	for name, tc := range map[string]struct {
		message string
		mail    string
		ip      string
		result  string
		policy  string
		domain  string
	}{
		"spf aligned":            {from("example.org"), "a@example.org", "192.0.2.1", "pass", "reject", "example.org"},
		"spf relaxed subdomain":  {from("example.org"), "a@bounces.example.org", "192.0.2.1", "pass", "reject", "example.org"},
		"spf pass but unaligned": {from("example.org"), "a@other.example", "192.0.2.1", "fail", "reject", "example.org"},
		"nothing passes":         {from("example.org"), "a@example.org", "192.0.2.9", "fail", "reject", "example.org"},
		"org domain fallback":    {from("news.example.org"), "a@example.org", "192.0.2.1", "pass", "quarantine", "news.example.org"},
		"strict spf subdomain":   {from("strict.example"), "a@sub.strict.example", "192.0.2.1", "fail", "none", "strict.example"},
		"no record":              {from("norecord.example"), "a@norecord.example", "192.0.2.1", "none", "", "norecord.example"},
		"bad record":             {from("broken.example"), "a@broken.example", "192.0.2.1", "permerror", "", "broken.example"},
		"two from fields":        {"From: a@example.org\r\nFrom: b@example.org\r\n\r\nx\r\n", "a@example.org", "192.0.2.1", "permerror", "", ""},
		"no from":                {"To: user@mail.test\r\n\r\nx\r\n", "a@example.org", "192.0.2.1", "none", "", ""},
	} {
		got := run(t, dns, 2*time.Second, testEnvelope(tc.mail, "mx.test", tc.ip), []byte(tc.message)).DMARC
		if got.Result != tc.result || got.Policy != tc.policy || got.Domain != tc.domain {
			t.Errorf("%s: %+v", name, got)
		}
	}
	// DKIM alignment alone is enough, relaxed or strict.
	msg := signed(t, key, "news.example.org", from("example.org"))
	got := run(t, dns, 2*time.Second, testEnvelope("a@nowhere.example", "mx.test", "192.0.2.9"), msg)
	if got.DMARC.Result != "pass" || got.DKIMSummary() != "pass" {
		t.Fatalf("relaxed DKIM alignment: %+v", got)
	}
	// Strict alignment compares exact domains.
	msg = signed(t, key, "other.example", from("strict.example"))
	got = run(t, dns, 2*time.Second, testEnvelope("a@nowhere.example", "mx.test", "192.0.2.9"), msg)
	if got.DMARC.Result != "fail" {
		t.Fatalf("a DKIM pass for another domain aligned: %+v", got)
	}
}

func TestARCPresenceOnly(t *testing.T) {
	msg := "ARC-Seal: i=1; a=rsa-sha256; d=example.org; s=s1; cv=none; b=abc\r\nFrom: a@example.org\r\n\r\nx\r\n"
	got := run(t, &fakeDNS{}, time.Second, testEnvelope("", "", ""), []byte(msg))
	if !got.ARC.Present || got.ARC.Evaluated {
		t.Fatalf("%+v", got.ARC)
	}
	if got = run(t, &fakeDNS{}, time.Second, testEnvelope("", "", ""), []byte(plainMessage)); got.ARC.Present {
		t.Fatal("ARC invented")
	}
}

// DNS that never answers ends every check as temperror within the budget; it is
// never an error and never blocks past it.
func TestBudgetExhaustionGivesTemperror(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dns := &fakeDNS{
		txt:   map[string][]string{"example.org": {"v=spf1 -all"}, "_dmarc.example.org": {"v=DMARC1; p=reject"}},
		block: map[string]bool{"example.org": true, "s1._domainkey.example.org": true, "_dmarc.example.org": true},
	}
	msg := signed(t, key, "example.org", plainMessage)
	started := time.Now()
	got := run(t, dns, 200*time.Millisecond, testEnvelope("a@example.org", "mx.test", "192.0.2.1"), msg)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("authentication outlived its budget: %v", elapsed)
	}
	if got.SPF.Result != "temperror" || got.DKIMSummary() != "temperror" || got.DMARC.Result != "temperror" {
		t.Fatalf("%+v", got)
	}
	// The stored columns accept every value produced.
	if !validResult(got.SPF.Result, "none", "pass", "fail", "softfail", "neutral", "temperror", "permerror") || !validResult(got.DKIMSummary(), "none", "pass", "fail", "temperror", "permerror") || !validResult(got.DMARC.Result, "none", "pass", "fail", "temperror", "permerror") {
		t.Fatalf("result outside the schema's enum: %+v", got)
	}
}

func validResult(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// A slow but answering resolver within the budget still gives real results.
func TestSlowDNSWithinBudget(t *testing.T) {
	dns := &fakeDNS{txt: map[string][]string{"example.org": {"v=spf1 ip4:192.0.2.1 -all"}}, delay: 20 * time.Millisecond}
	got := run(t, dns, 2*time.Second, testEnvelope("a@example.org", "mx.test", "192.0.2.1"), []byte(plainMessage))
	if got.SPF.Result != "pass" {
		t.Fatalf("%+v", got.SPF)
	}
}

func TestAuthenticationJSONShape(t *testing.T) {
	var empty Authentication
	var decoded map[string]any
	if err := json.Unmarshal(empty.JSON(), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["dkim"].([]any); !ok {
		t.Fatalf("dkim must be an array: %s", empty.JSON())
	}
	a := Authentication{SPF: SPFResult{Result: "pass", Domain: "example.org", Identity: "mailfrom"}, DKIM: []DKIMResult{{"pass", "example.org"}}, DMARC: DMARCResult{Result: "pass", Domain: "example.org", Policy: "reject"}, ARC: ARCResult{Present: true}}
	if got := string(a.JSON()); got != `{"spf":{"result":"pass","domain":"example.org","identity":"mailfrom"},"dkim":[{"result":"pass","domain":"example.org"}],"dmarc":{"result":"pass","domain":"example.org","policy":"reject"},"arc":{"present":true,"evaluated":false}}` {
		t.Fatal(got)
	}
	// Even the largest result fits the column's 16 KiB bound.
	big := Authentication{SPF: SPFResult{Result: "temperror", Domain: strings.Repeat("a", 253), Identity: "helo"}, DMARC: DMARCResult{Result: "temperror", Domain: strings.Repeat("a", 253), Policy: "quarantine"}}
	for i := 0; i < maxDKIMSignatures; i++ {
		big.DKIM = append(big.DKIM, DKIMResult{"temperror", strings.Repeat("a", 253)})
	}
	if len(big.JSON()) > 16384 {
		t.Fatal("authentication JSON exceeds the column bound")
	}
}

// A hostile d= tag cannot reach the database: only DNS names are kept, and the
// document is bounded however many entries there are.
func TestHostileDKIMDomainsAreDropped(t *testing.T) {
	sig := "DKIM-Signature: v=1; a=rsa-sha256; d=a\x00b.example; s=s1; c=simple/simple; h=from; bh=AAAA; b=AAAA\r\nFrom: a@example.org\r\n\r\nx\r\n"
	got := run(t, &fakeDNS{}, time.Second, testEnvelope("", "", ""), []byte(sig))
	for _, d := range got.DKIM {
		if d.Domain != "" {
			t.Fatalf("a hostile domain was kept: %q", d.Domain)
		}
	}
	if strings.Contains(string(got.JSON()), `\u0000`) {
		t.Fatalf("%s", got.JSON())
	}
	extra := Authentication{}
	for i := 0; i < 50; i++ {
		extra.DKIM = append(extra.DKIM, DKIMResult{Result: "fail", Domain: "example.org"})
	}
	var decoded struct{ DKIM []DKIMResult }
	if err := json.Unmarshal(extra.JSON(), &decoded); err != nil || len(decoded.DKIM) != maxDKIMSignatures {
		t.Fatalf("entries are not capped: %d %v", len(decoded.DKIM), err)
	}
}

func TestWorstCaseAuthenticationFitsTheColumn(t *testing.T) {
	long := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	if len(long) != 253 || !validDNSName(long) {
		t.Fatal("fixture")
	}
	worst := Authentication{SPF: SPFResult{Result: "softfail", Domain: long, Identity: "mailfrom"}, DMARC: DMARCResult{Result: "temperror", Domain: long, Policy: "quarantine"}, ARC: ARCResult{Present: true}}
	for i := 0; i < maxDKIMSignatures+3; i++ {
		worst.DKIM = append(worst.DKIM, DKIMResult{Result: "temperror", Domain: long})
	}
	if n := len(worst.JSON()); n > 4096 {
		t.Fatalf("worst case is %d bytes", n)
	}
}
