package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"
)

// AuthBudget bounds all DNS work of one message's authentication. A message
// whose checks do not finish in time reports temperror; it is never refused.
const AuthBudget = 5 * time.Second

// maxDKIMSignatures bounds DKIM verifications per message.
const maxDKIMSignatures = 5

// Resolver is the DNS the checks use. *net.Resolver satisfies it.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// Authentication results are reported with the message and never enforced: the
// receiving app decides what a failure means.
type SPFResult struct {
	Result string `json:"result"`
	Domain string `json:"domain,omitempty"`
	// Identity is "mailfrom", or "helo" for the null reverse path.
	Identity string `json:"identity,omitempty"`
}
type DKIMResult struct {
	Result string `json:"result"`
	Domain string `json:"domain,omitempty"`
}
type DMARCResult struct {
	Result string `json:"result"`
	Domain string `json:"domain,omitempty"`
	Policy string `json:"policy,omitempty"`
}

// ARCResult only reports whether ARC headers are present: chains are not
// evaluated.
type ARCResult struct {
	Present   bool `json:"present"`
	Evaluated bool `json:"evaluated"`
}

// Authentication is the per-method outcome stored with the message.
type Authentication struct {
	SPF   SPFResult    `json:"spf"`
	DKIM  []DKIMResult `json:"dkim"`
	DMARC DMARCResult  `json:"dmarc"`
	ARC   ARCResult    `json:"arc"`
}

// DKIMSummary is the aggregate stored in the dkim_result column: pass if any
// signature passed, otherwise temperror if any was temporarily unverifiable,
// otherwise fail, and none without signatures.
func (a Authentication) DKIMSummary() string {
	if len(a.DKIM) == 0 {
		return "none"
	}
	summary := "fail"
	for _, r := range a.DKIM {
		switch r.Result {
		case "pass":
			return "pass"
		case "temperror":
			summary = "temperror"
		}
	}
	return summary
}

func (a Authentication) JSON() []byte {
	if a.DKIM == nil {
		a.DKIM = []DKIMResult{}
	}
	// At most maxDKIMSignatures entries, each with a validated DNS name (at most
	// 253 bytes), plus the SPF and DMARC results: the document is a few KiB at
	// worst, far below the column's 16 KiB bound (TestWorstCaseAuthenticationFitsTheColumn).
	if len(a.DKIM) > maxDKIMSignatures {
		a.DKIM = a.DKIM[:maxDKIMSignatures]
	}
	out, err := json.Marshal(a)
	if err != nil {
		return []byte(`{}`)
	}
	return out
}

func spfResult(r spf.Result) string {
	switch r {
	case spf.Pass:
		return "pass"
	case spf.Fail:
		return "fail"
	case spf.SoftFail:
		return "softfail"
	case spf.Neutral:
		return "neutral"
	case spf.TempError:
		return "temperror"
	case spf.PermError:
		return "permerror"
	}
	return "none"
}

// budgetError is what a lookup returns once the budget is spent: a temporary
// DNS error, so the libraries report temperror.
func budgetError(name string) error {
	return &net.DNSError{Err: "lookup budget exceeded", Name: name, IsTimeout: true, IsTemporary: true}
}

// budgeted refuses lookups once the context is done, whatever the resolver does.
type budgeted struct {
	ctx context.Context
	r   Resolver
}

func (b budgeted) check(name string) error {
	if b.ctx.Err() != nil {
		return budgetError(name)
	}
	return nil
}
func (b budgeted) LookupTXT(ctx context.Context, name string) ([]string, error) {
	if err := b.check(name); err != nil {
		return nil, err
	}
	out, err := b.r.LookupTXT(b.ctx, name)
	if err != nil && b.ctx.Err() != nil {
		return nil, budgetError(name)
	}
	return out, err
}
func (b budgeted) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	if err := b.check(name); err != nil {
		return nil, err
	}
	out, err := b.r.LookupMX(b.ctx, name)
	if err != nil && b.ctx.Err() != nil {
		return nil, budgetError(name)
	}
	return out, err
}
func (b budgeted) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if err := b.check(host); err != nil {
		return nil, err
	}
	out, err := b.r.LookupIPAddr(b.ctx, host)
	if err != nil && b.ctx.Err() != nil {
		return nil, budgetError(host)
	}
	return out, err
}
func (b budgeted) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	if err := b.check(addr); err != nil {
		return nil, err
	}
	out, err := b.r.LookupAddr(b.ctx, addr)
	if err != nil && b.ctx.Err() != nil {
		return nil, budgetError(addr)
	}
	return out, err
}

func domainOf(address string) string {
	if i := strings.LastIndexByte(address, '@'); i >= 0 {
		return strings.ToLower(address[i+1:])
	}
	return ""
}

func validDNSName(name string) bool {
	if len(name) == 0 || len(name) > 253 || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// Authenticate runs SPF, DKIM and DMARC in parallel under one DNS budget. A
// check that has not finished when the budget ends reports temperror. It never
// returns an error and never blocks past the budget.
func Authenticate(ctx context.Context, resolver Resolver, budget time.Duration, e envelope, raw []byte, parsed *Parsed) Authentication {
	if budget <= 0 {
		budget = AuthBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	dns := budgeted{ctx: ctx, r: resolver}
	result := Authentication{ARC: ARCResult{Present: parsed.ARCPresent}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		result.SPF = runWithBudget(ctx, SPFResult{Result: "temperror"}, func() SPFResult { return checkSPF(ctx, dns, e) })
	}()
	go func() {
		defer wg.Done()
		result.DKIM = runWithBudget(ctx, []DKIMResult{{Result: "temperror"}}, func() []DKIMResult { return checkDKIM(ctx, dns, raw) })
	}()
	wg.Wait()
	// The DMARC check may outlive its budget in the background, so it gets its
	// own copy of the inputs rather than the struct the caller keeps writing.
	inputs := Authentication{SPF: result.SPF, DKIM: result.DKIM}
	result.DMARC = runWithBudget(ctx, DMARCResult{Result: "temperror"}, func() DMARCResult { return checkDMARC(ctx, dns, e, parsed, inputs) })
	return result
}

// runWithBudget returns fn's value, or the fallback once the budget ends first.
// A library that ignores the context keeps running in the background, but every
// lookup it makes fails fast through the budgeted resolver.
func runWithBudget[T any](ctx context.Context, fallback T, fn func() T) T {
	done := make(chan T, 1)
	go func() { done <- fn() }()
	select {
	case v := <-done:
		return v
	case <-ctx.Done():
		select {
		case v := <-done:
			return v
		default:
			return fallback
		}
	}
}

func checkSPF(ctx context.Context, dns budgeted, e envelope) SPFResult {
	if !e.clientIP.IsValid() {
		return SPFResult{Result: "none"}
	}
	helo := strings.ToLower(e.helo)
	sender, identity, domain := e.mailFrom, "mailfrom", domainOf(e.mailFrom)
	if e.mailFrom == "" {
		// RFC 7208 4.3: the null reverse path is checked as postmaster@HELO.
		identity, domain = "helo", helo
		sender = "postmaster@" + helo
	}
	if !validDNSName(domain) || identity == "helo" && !validDNSName(helo) {
		return SPFResult{Result: "none"}
	}
	r, err := spf.CheckHostWithSender(net.IP(e.clientIP.AsSlice()), helo, sender, spf.WithContext(ctx), spf.WithResolver(dns))
	if ctx.Err() != nil {
		r = spf.TempError
	}
	_ = err
	return SPFResult{Result: spfResult(r), Domain: domain, Identity: identity}
}

func checkDKIM(ctx context.Context, dns budgeted, raw []byte) []DKIMResult {
	lookup := func(name string) ([]string, error) { return dns.LookupTXT(ctx, name) }
	verifications, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{LookupTXT: lookup, MaxVerifications: maxDKIMSignatures})
	if err != nil && !errors.Is(err, dkim.ErrTooManySignatures) {
		if ctx.Err() != nil {
			return []DKIMResult{{Result: "temperror"}}
		}
		// A malformed header block: no usable signature.
		return nil
	}
	out := make([]DKIMResult, 0, len(verifications))
	for _, v := range verifications {
		// The d= tag is attacker-controlled: keep it only if it is a DNS name.
		r := DKIMResult{}
		if domain := strings.ToLower(v.Domain); validDNSName(domain) {
			r.Domain = domain
		}
		switch {
		case v.Err == nil:
			r.Result = "pass"
		case ctx.Err() != nil || dkim.IsTempFail(v.Err):
			r.Result = "temperror"
		default:
			r.Result = "fail"
		}
		if len(out) < maxDKIMSignatures {
			out = append(out, r)
		}
	}
	return out
}

// aligned implements DMARC identifier alignment (RFC 7489 3.1). Relaxed mode
// compares organizational domains; strict compares exactly.
func aligned(identifier, from string, strict bool) bool {
	identifier, from = strings.ToLower(identifier), strings.ToLower(from)
	if identifier == "" || from == "" {
		return false
	}
	if strict {
		return identifier == from
	}
	a, errA := publicsuffix.EffectiveTLDPlusOne(identifier)
	b, errB := publicsuffix.EffectiveTLDPlusOne(from)
	return errA == nil && errB == nil && a == b
}

func checkDMARC(ctx context.Context, dns budgeted, e envelope, parsed *Parsed, auth Authentication) DMARCResult {
	if parsed.FromCount > 1 {
		return DMARCResult{Result: "permerror"}
	}
	from := domainOf(parsed.FromAddress)
	if !validDNSName(from) {
		return DMARCResult{Result: "none"}
	}
	lookup := &dmarc.LookupOptions{LookupTXT: func(name string) ([]string, error) { return dns.LookupTXT(ctx, name) }}
	policyDomain := from
	record, err := dmarc.LookupWithOptions(from, lookup)
	if errors.Is(err, dmarc.ErrNoPolicy) {
		if org, perr := publicsuffix.EffectiveTLDPlusOne(from); perr == nil && org != from {
			policyDomain = org
			record, err = dmarc.LookupWithOptions(org, lookup)
		}
	}
	switch {
	case errors.Is(err, dmarc.ErrNoPolicy):
		return DMARCResult{Result: "none", Domain: from}
	case err != nil && (ctx.Err() != nil || dmarc.IsTempFail(err)):
		return DMARCResult{Result: "temperror", Domain: from}
	case err != nil || record == nil:
		return DMARCResult{Result: "permerror", Domain: from}
	}
	policy := string(record.Policy)
	if policyDomain != from && record.SubdomainPolicy != "" {
		policy = string(record.SubdomainPolicy)
	}
	result := DMARCResult{Result: "fail", Domain: from, Policy: policy}
	if auth.SPF.Result == "pass" && auth.SPF.Identity == "mailfrom" && aligned(auth.SPF.Domain, from, record.SPFAlignment == dmarc.AlignmentStrict) {
		result.Result = "pass"
	}
	for _, d := range auth.DKIM {
		if d.Result == "pass" && aligned(d.Domain, from, record.DKIMAlignment == dmarc.AlignmentStrict) {
			result.Result = "pass"
		}
	}
	if result.Result == "fail" && (auth.SPF.Result == "temperror" || auth.DKIMSummary() == "temperror") {
		// A temporary error in an input means the verdict is not yet known.
		result.Result = "temperror"
	}
	return result
}
