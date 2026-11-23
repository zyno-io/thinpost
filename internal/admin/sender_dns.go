package admin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"blitiri.com.ar/go/spf"
	"golang.org/x/net/publicsuffix"

	"github.com/zyno-io/thinpost/internal/customdomain"
)

// MailAuthResolver serves the advisory SPF and DMARC check. *net.Resolver
// satisfies it; it is also blitiri.com.ar/go/spf's DNSResolver.
type MailAuthResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

const (
	senderDNSCheckTimeout = 8 * time.Second
	maxDMARCRecordBytes   = 1024
)

// DMARC statuses, SPF's extra result and envelope kinds, as the API reports
// them. The other SPF results are blitiri's own.
const (
	DMARCOK          = "ok"
	DMARCMonitoring  = "monitoring"
	DMARCMissing     = "missing"
	DMARCMultiple    = "multiple"
	DMARCInvalid     = "invalid"
	DMARCUnavailable = "unavailable"

	SPFNoSendingIP = "no_sending_ip"

	EnvelopeBounceDomain = "bounce_domain"
	EnvelopeStandard     = "standard"
)

// SenderDomainDNSCheck is the advisory SPF and DMARC check of a sender domain.
type SenderDomainDNSCheck struct {
	Domain string            `json:"domain"`
	DMARC  SenderDomainDMARC `json:"dmarc"`
	SPF    SenderDomainSPF   `json:"spf"`
}

// SenderDomainDMARC is what receivers find for the domain's DMARC policy.
type SenderDomainDMARC struct {
	Status       string `json:"status"`
	OrgDomain    string `json:"org_domain"`
	RecordDomain string `json:"record_domain,omitempty"`
	Record       string `json:"record,omitempty"`
	Policy       string `json:"policy,omitempty"`
	SPFAlignment string `json:"aspf,omitempty"`
}

// SenderDomainSPF is the SPF result of the app's sending IP at the envelope
// domain the domain's mail uses now.
type SenderDomainSPF struct {
	Result         string `json:"result"` // string(spf.Result) or SPFNoSendingIP
	EnvelopeDomain string `json:"envelope_domain,omitempty"`
	EnvelopeKind   string `json:"envelope_kind,omitempty"`
	Aligned        bool   `json:"aligned"`
}

// rootedResolver looks every name up rooted, so search domains never apply
// (the same rule as the bounce-domain check). Errors pass through unwrapped:
// the SPF library tells "not found" by a direct *net.DNSError assertion.
type rootedResolver struct{ r MailAuthResolver }

func rooted(name string) string {
	if strings.HasSuffix(name, ".") {
		return name
	}
	return name + "."
}

func (d rootedResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return d.r.LookupTXT(ctx, rooted(name))
}
func (d rootedResolver) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	return d.r.LookupMX(ctx, rooted(name))
}
func (d rootedResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return d.r.LookupIPAddr(ctx, rooted(host))
}
func (d rootedResolver) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	return d.r.LookupAddr(ctx, addr)
}

// checkSenderDomainDNS runs DMARC, then SPF, under one deadline.
func checkSenderDomainDNS(ctx context.Context, resolver MailAuthResolver, domain string, target customdomain.Target, bounces map[string]BounceDomain, now time.Time) SenderDomainDNSCheck {
	ctx, cancel := context.WithTimeout(ctx, senderDNSCheckTimeout)
	defer cancel()
	dns := rootedResolver{resolver}
	out := SenderDomainDNSCheck{Domain: domain}
	out.DMARC = checkSenderDMARC(ctx, dns, domain)
	out.SPF = checkSenderSPF(ctx, dns, domain, target, bounces, now)
	out.SPF.Aligned = out.SPF.Result == string(spf.Pass) && identifierAligned(out.SPF.EnvelopeDomain, domain, out.DMARC.SPFAlignment == "s")
	return out
}

// orgDomain is the organizational domain of name; name itself when it has
// none (a public suffix) or the list cannot say.
func orgDomain(name string) string {
	org, err := publicsuffix.EffectiveTLDPlusOne(name)
	if err != nil {
		return name
	}
	return org
}

// checkSenderDMARC finds the DMARC record for the domain: its own, or only
// when it has none, its organizational domain's (RFC 7489 6.6.3).
func checkSenderDMARC(ctx context.Context, dns MailAuthResolver, domain string) SenderDomainDMARC {
	org := orgDomain(domain)
	out := lookupDMARC(ctx, dns, domain, domain, org)
	if out.Status == DMARCMissing && org != domain {
		out = lookupDMARC(ctx, dns, org, domain, org)
	}
	return out
}

// lookupDMARC reads the record at _dmarc.name for mail from domain.
func lookupDMARC(ctx context.Context, dns MailAuthResolver, name, domain, org string) SenderDomainDMARC {
	out := SenderDomainDMARC{OrgDomain: org}
	txt, err := dns.LookupTXT(ctx, "_dmarc."+name)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			out.Status = DMARCMissing
		} else {
			out.Status = DMARCUnavailable
		}
		return out
	}
	var records []string
	for _, record := range txt {
		if isDMARCRecord(record) {
			records = append(records, record)
		}
	}
	switch len(records) {
	case 0:
		out.Status = DMARCMissing
		return out
	case 1:
	default:
		out.Status, out.RecordDomain = DMARCMultiple, name
		return out
	}
	out.RecordDomain = name
	out.Record = cutRecord(records[0])
	p, sp, aspf, ok := parseDMARC(records[0])
	if !ok {
		out.Status = DMARCInvalid
		return out
	}
	out.Policy, out.SPFAlignment = p, aspf
	if name != domain && sp != "" {
		out.Policy = sp
	}
	out.Status = DMARCOK
	if out.Policy == "none" {
		out.Status = DMARCMonitoring
	}
	return out
}

func cutRecord(record string) string {
	if len(record) > maxDMARCRecordBytes {
		record = record[:maxDMARCRecordBytes]
	}
	return strings.ToValidUTF8(record, "")
}

// isDMARCRecord reports whether the TXT string is a DMARC record: its first
// tag is v=DMARC1 (the value is case-sensitive in the ABNF).
func isDMARCRecord(txt string) bool {
	first, _, _ := strings.Cut(txt, ";")
	key, value, ok := strings.Cut(first, "=")
	return ok && strings.EqualFold(strings.TrimSpace(key), "v") && strings.TrimSpace(value) == "DMARC1"
}

// parseDMARC reads the tags the check reports, as a receiver would (RFC 7489
// 6.3 and 6.6.3). Tag names and the values of p, sp and aspf are
// case-insensitive. An invalid aspf is discarded in favour of relaxed. A
// record with no valid p, or with an invalid sp, counts as p=none when it has
// a rua tag; without one, receivers apply no DMARC and ok is false.
func parseDMARC(record string) (p, sp, aspf string, ok bool) {
	aspf = "r"
	spInvalid, hasRUA := false, false
	for i, tag := range strings.Split(record, ";") {
		key, value, found := strings.Cut(tag, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if i == 0 || !found || key == "v" {
			continue
		}
		value = strings.ToLower(strings.TrimSpace(value))
		switch key {
		case "p", "sp":
			valid := value == "none" || value == "quarantine" || value == "reject"
			switch {
			case key == "p" && valid:
				p = value
			case key == "sp" && valid:
				sp = value
			case key == "sp":
				spInvalid = true
			}
		case "aspf":
			if value == "r" || value == "s" {
				aspf = value
			}
		case "rua":
			hasRUA = value != ""
		}
	}
	if p == "" || spInvalid {
		if !hasRUA {
			return "", "", "", false
		}
		p, sp = "none", ""
	}
	return p, sp, aspf, true
}

// bounceDomainInUse is the bounce domain the app's mail from domain would use
// now: the most specific one, of the domain or a parent sender domain, that
// is active and passed a check within the freshness window (the claim's
// customReturnPath, without its check of receiving domains). A row whose last
// check saw the CNAME point at another name than target (the HELO name mail
// is sent from now) is skipped, as the claim skips it.
func bounceDomainInUse(domain, target string, bounces map[string]BounceDomain, now time.Time) (string, bool) {
	for name := domain; name != ""; _, name, _ = strings.Cut(name, ".") {
		b, ok := bounces[name]
		if !ok || b.State != BounceDomainActive || b.LastOKAt == nil || now.Sub(*b.LastOKAt) >= customdomain.Freshness {
			continue
		}
		if b.ObservedTarget != nil && *b.ObservedTarget != target {
			continue
		}
		return b.Name, true
	}
	return "", false
}

// checkSenderSPF evaluates the sending IP at the envelope domain in use.
func checkSenderSPF(ctx context.Context, dns MailAuthResolver, domain string, target customdomain.Target, bounces map[string]BounceDomain, now time.Time) SenderDomainSPF {
	addr, err := netip.ParseAddr(target.Address)
	if target.Name == "" || err != nil {
		return SenderDomainSPF{Result: SPFNoSendingIP}
	}
	out := SenderDomainSPF{EnvelopeDomain: target.Name, EnvelopeKind: EnvelopeStandard}
	if name, ok := bounceDomainInUse(domain, target.Name, bounces, now); ok {
		out.EnvelopeDomain, out.EnvelopeKind = name, EnvelopeBounceDomain
	}
	result, _ := spf.CheckHostWithSender(net.IP(addr.Unmap().AsSlice()), target.Name, "postmaster@"+out.EnvelopeDomain, spf.WithContext(ctx), spf.WithResolver(dns))
	if ctx.Err() != nil {
		result = spf.TempError
	}
	out.Result = string(result)
	return out
}

// identifierAligned is DMARC identifier alignment (RFC 7489 3.1): relaxed
// compares organizational domains, strict compares exactly.
func identifierAligned(identifier, from string, strict bool) bool {
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

// senderDomainDNSRoute serves POST /apps/{appId}/domains/{domain}/check-dns:
// an advisory read of the domain's SPF and DMARC. Nothing is stored, logged
// or audited. rest is the path after "domains". It reports false for a path
// it does not own.
func (h *Handler) senderDomainDNSRoute(w http.ResponseWriter, r *http.Request, appID string, rest []string) bool {
	if len(rest) != 2 || rest[1] != "check-dns" {
		return false
	}
	store, ok := h.Store.(BounceDomainStore)
	if !ok {
		fail(w, http.StatusServiceUnavailable)
		return true
	}
	domain, ok := NormalizeDomain(rest[0])
	if !ok || r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest)
		return true
	}
	if r.Method != http.MethodPost {
		method(w, "POST")
		return true
	}
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		fail(w, http.StatusBadRequest)
		return true
	}
	if h.MailAuthDNS == nil {
		fail(w, http.StatusServiceUnavailable)
		return true
	}
	ctx := r.Context()
	domains, err := h.Store.ListDomains(ctx, appID)
	if err != nil {
		storeError(w, err)
		return true
	}
	found := false
	for _, d := range domains {
		found = found || d.Name == domain
	}
	if !found {
		fail(w, http.StatusNotFound)
		return true
	}
	target, err := store.BounceTarget(ctx, appID)
	if err != nil {
		storeError(w, err)
		return true
	}
	bounces, err := store.ListBounceDomains(ctx, appID)
	if err != nil {
		storeError(w, err)
		return true
	}
	respond(w, http.StatusOK, checkSenderDomainDNS(ctx, h.MailAuthDNS, domain, target, bounces, time.Now()))
	return true
}
