package admin

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/customdomain"
)

// fakeMailDNS answers from maps. It flags any name that is not rooted and any
// context without a deadline. Names without an entry are not found.
type fakeMailDNS struct {
	txt    map[string][]string
	txtErr map[string]error
	ips    map[string][]string
	mx     map[string][]string
	names  []string
	bad    []string
	asked  []string
}

func (f *fakeMailDNS) note(ctx context.Context, name string, rootedName bool) string {
	if rootedName && !strings.HasSuffix(name, ".") {
		f.bad = append(f.bad, "unrooted "+name)
	}
	if _, ok := ctx.Deadline(); !ok {
		f.bad = append(f.bad, "no deadline "+name)
	}
	name = strings.TrimSuffix(name, ".")
	f.asked = append(f.asked, name)
	return name
}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fakeMailDNS) LookupTXT(ctx context.Context, name string) ([]string, error) {
	name = f.note(ctx, name, true)
	if err, ok := f.txtErr[name]; ok {
		return nil, err
	}
	if v, ok := f.txt[name]; ok {
		return v, nil
	}
	return nil, notFound(name)
}
func (f *fakeMailDNS) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	name = f.note(ctx, name, true)
	v, ok := f.mx[name]
	if !ok {
		return nil, notFound(name)
	}
	var out []*net.MX
	for _, h := range v {
		out = append(out, &net.MX{Host: h, Pref: 10})
	}
	return out, nil
}
func (f *fakeMailDNS) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	host = f.note(ctx, host, true)
	v, ok := f.ips[host]
	if !ok {
		return nil, notFound(host)
	}
	var out []net.IPAddr
	for _, ip := range v {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return out, nil
}
func (f *fakeMailDNS) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	f.note(ctx, addr, false)
	return f.names, nil
}

func TestParseDMARC(t *testing.T) {
	for _, tc := range []struct {
		name, record, p, sp, aspf string
		ok                        bool
	}{
		{"lowercase", "v=DMARC1; p=reject", "reject", "", "r", true},
		{"uppercase values", "v=DMARC1; P=Reject; ASPF=S", "reject", "", "s", true},
		{"sp", "v=DMARC1; p=quarantine; sp=none; aspf=r", "quarantine", "none", "r", true},
		{"trailing semicolon", "v=DMARC1; p=none;", "none", "", "r", true},
		{"spaces", "v = DMARC1 ; p = none", "none", "", "r", true},
		{"unknown tags", "v=DMARC1; p=none; rua=mailto:r@example.com; x=y; pct=50", "none", "", "r", true},
		{"missing p", "v=DMARC1; aspf=s", "", "", "", false},
		{"missing p with rua counts as none", "v=DMARC1; rua=mailto:r@example.com", "none", "", "r", true},
		{"invalid p", "v=DMARC1; p=block", "", "", "", false},
		{"invalid p with rua counts as none", "v=DMARC1; p=block; rua=mailto:r@example.com", "none", "", "r", true},
		{"invalid sp", "v=DMARC1; p=reject; sp=x", "", "", "", false},
		{"invalid sp with rua counts as none", "v=DMARC1; p=reject; sp=x; rua=mailto:r@example.com", "none", "", "r", true},
		{"invalid aspf falls back to relaxed", "v=DMARC1; p=reject; aspf=x", "reject", "", "r", true},
		{"invalid adkim is ignored", "v=DMARC1; p=reject; adkim=strict", "reject", "", "r", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sp, aspf, ok := parseDMARC(tc.record)
			if p != tc.p || sp != tc.sp || aspf != tc.aspf || ok != tc.ok {
				t.Fatalf("got %q %q %q %v", p, sp, aspf, ok)
			}
		})
	}
	for record, want := range map[string]bool{
		"v=DMARC1; p=none":  true,
		"V = DMARC1;p=none": true,
		"v=dmarc1; p=none":  false,
		"p=none; v=DMARC1":  false,
		"v=spf1 -all":       false,
		"":                  false,
	} {
		if isDMARCRecord(record) != want {
			t.Fatalf("isDMARCRecord(%q) != %v", record, want)
		}
	}
}

func TestSenderDomainDNSCheck(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	target := customdomain.Target{Name: "smtp.example.net", Address: "203.0.113.10", Reported: []string{"smtp.example.net"}}
	spfOK := map[string][]string{"smtp.example.net": {"v=spf1 ip4:203.0.113.10 -all"}}
	oldHelo := "smtp-old.example.net"
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	merge := func(maps ...map[string][]string) map[string][]string {
		out := map[string][]string{}
		for _, m := range maps {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	active := BounceDomain{Name: "bounces.example.com", State: BounceDomainActive, LastOKAt: ago(time.Hour)}
	bounceSPF := map[string][]string{"bounces.example.com": {"v=spf1 ip4:203.0.113.10 -all"}}

	for _, tc := range []struct {
		name    string
		domain  string
		dns     fakeMailDNS
		target  customdomain.Target
		bounces map[string]BounceDomain
		check   func(t *testing.T, c SenderDomainDNSCheck, f *fakeMailDNS)
	}{
		{
			name: "own reject record", domain: "example.com", target: target,
			dns: fakeMailDNS{txt: merge(spfOK, map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=reject"}})},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				want := SenderDomainDMARC{Status: DMARCOK, OrgDomain: "example.com", RecordDomain: "example.com", Record: "v=DMARC1; p=reject", Policy: "reject", SPFAlignment: "r"}
				if c.DMARC != want {
					t.Fatalf("%+v", c.DMARC)
				}
				if c.SPF != (SenderDomainSPF{Result: "pass", EnvelopeDomain: "smtp.example.net", EnvelopeKind: EnvelopeStandard}) {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
		{
			name: "inherited with sp=none is monitoring", domain: "mail.example.com", target: target,
			dns: fakeMailDNS{txt: map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=reject; sp=none"}}},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.DMARC.Status != DMARCMonitoring || c.DMARC.RecordDomain != "example.com" || c.DMARC.OrgDomain != "example.com" || c.DMARC.Policy != "none" {
					t.Fatalf("%+v", c.DMARC)
				}
			},
		},
		{
			name: "inherited without sp uses p", domain: "mail.example.com", target: target,
			dns: fakeMailDNS{txt: map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=quarantine"}}},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.DMARC.Status != DMARCOK || c.DMARC.Policy != "quarantine" {
					t.Fatalf("%+v", c.DMARC)
				}
			},
		},
		{
			name: "multiple does not fall back", domain: "mail.example.com", target: target,
			dns: fakeMailDNS{txt: map[string][]string{
				"_dmarc.mail.example.com": {"v=DMARC1; p=none", "v=DMARC1; p=reject"},
				"_dmarc.example.com":      {"v=DMARC1; p=reject"},
			}},
			check: func(t *testing.T, c SenderDomainDNSCheck, f *fakeMailDNS) {
				if c.DMARC.Status != DMARCMultiple || c.DMARC.RecordDomain != "mail.example.com" || c.DMARC.Record != "" {
					t.Fatalf("%+v", c.DMARC)
				}
				for _, name := range f.asked {
					if name == "_dmarc.example.com" {
						t.Fatal("looked past multiple records")
					}
				}
			},
		},
		{
			name: "invalid does not fall back", domain: "mail.example.com", target: target,
			dns: fakeMailDNS{txt: map[string][]string{
				"_dmarc.mail.example.com": {"v=DMARC1; p=block"},
				"_dmarc.example.com":      {"v=DMARC1; p=reject"},
			}},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.DMARC.Status != DMARCInvalid || c.DMARC.Record != "v=DMARC1; p=block" || c.DMARC.RecordDomain != "mail.example.com" {
					t.Fatalf("%+v", c.DMARC)
				}
			},
		},
		{
			name: "other TXT records are ignored, then org fallback", domain: "mail.example.com", target: target,
			dns: fakeMailDNS{txt: map[string][]string{
				"_dmarc.mail.example.com": {"verification=abc", "v=spf1 -all"},
				"_dmarc.example.com":      {"v=DMARC1; p=none"},
			}},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.DMARC.Status != DMARCMonitoring || c.DMARC.RecordDomain != "example.com" {
					t.Fatalf("%+v", c.DMARC)
				}
			},
		},
		{
			name: "missing everywhere", domain: "mail.example.com", target: target,
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.DMARC != (SenderDomainDMARC{Status: DMARCMissing, OrgDomain: "example.com"}) {
					t.Fatalf("%+v", c.DMARC)
				}
			},
		},
		{
			name: "temporary error does not fall back", domain: "mail.example.com", target: target,
			dns: fakeMailDNS{
				txtErr: map[string]error{"_dmarc.mail.example.com": &net.DNSError{Err: "server misbehaving", IsTemporary: true}},
				txt:    map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=reject"}},
			},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.DMARC.Status != DMARCUnavailable || c.DMARC.RecordDomain != "" {
					t.Fatalf("%+v", c.DMARC)
				}
			},
		},
		{
			name: "a long record is cut", domain: "example.com", target: target,
			dns: fakeMailDNS{txt: map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=none; rua=mailto:" + strings.Repeat("a", 1500) + "@example.com"}}},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.DMARC.Status != DMARCMonitoring || len(c.DMARC.Record) != maxDMARCRecordBytes {
					t.Fatalf("%s %d", c.DMARC.Status, len(c.DMARC.Record))
				}
			},
		},
		{
			name: "SPF passes through include", domain: "example.com", target: target,
			dns: fakeMailDNS{txt: map[string][]string{
				"smtp.example.net": {"v=spf1 include:_spf.example.org -all"},
				"_spf.example.org": {"v=spf1 ip4:203.0.113.0/24 -all"},
			}},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF.Result != "pass" || c.SPF.Aligned {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
		{
			name: "active bounce domain aligns relaxed", domain: "example.com", target: target,
			bounces: map[string]BounceDomain{"example.com": active},
			dns:     fakeMailDNS{txt: merge(bounceSPF, map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=none"}})},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF != (SenderDomainSPF{Result: "pass", EnvelopeDomain: "bounces.example.com", EnvelopeKind: EnvelopeBounceDomain, Aligned: true}) {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
		{
			name: "aspf=s does not align a bounce subdomain", domain: "example.com", target: target,
			bounces: map[string]BounceDomain{"example.com": active},
			dns:     fakeMailDNS{txt: merge(bounceSPF, map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=none; aspf=s"}})},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF.Result != "pass" || c.SPF.Aligned {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
		{
			name: "pending subdomain bounce domain falls back to the parent's", domain: "mail.example.com", target: target,
			bounces: map[string]BounceDomain{
				"mail.example.com": {Name: "b2.example.com", State: BounceDomainPending},
				"example.com":      active,
			},
			dns: fakeMailDNS{txt: bounceSPF},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF.EnvelopeDomain != "bounces.example.com" || c.SPF.EnvelopeKind != EnvelopeBounceDomain {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
		{
			name: "stale check means standard", domain: "example.com", target: target,
			bounces: map[string]BounceDomain{"example.com": {Name: "bounces.example.com", State: BounceDomainActive, LastOKAt: ago(4 * time.Hour)}},
			dns:     fakeMailDNS{txt: spfOK},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF.EnvelopeKind != EnvelopeStandard || c.SPF.EnvelopeDomain != "smtp.example.net" {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
		{
			name: "bounce domain last seen pointing at another HELO name means standard", domain: "example.com", target: target,
			bounces: map[string]BounceDomain{"example.com": {Name: "bounces.example.com", State: BounceDomainActive, LastOKAt: ago(time.Hour), ObservedTarget: &oldHelo}},
			dns:     fakeMailDNS{txt: spfOK},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF.EnvelopeKind != EnvelopeStandard || c.SPF.EnvelopeDomain != "smtp.example.net" {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
		{
			name: "failing bounce domain means standard", domain: "example.com", target: target,
			bounces: map[string]BounceDomain{"example.com": {Name: "bounces.example.com", State: BounceDomainFailing, LastOKAt: ago(time.Hour)}},
			dns:     fakeMailDNS{txt: spfOK},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF.EnvelopeKind != EnvelopeStandard {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
		{
			name: "a HELO name under the same organizational domain aligns", domain: "example.com",
			target: customdomain.Target{Name: "smtp.example.com", Address: "203.0.113.10"},
			dns:    fakeMailDNS{txt: map[string][]string{"smtp.example.com": {"v=spf1 ip4:203.0.113.10 -all"}}},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF.Result != "pass" || !c.SPF.Aligned || c.SPF.EnvelopeKind != EnvelopeStandard {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
		{
			name: "no sending IP still checks DMARC", domain: "example.com", target: customdomain.Target{},
			dns: fakeMailDNS{txt: map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=reject"}}},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF != (SenderDomainSPF{Result: SPFNoSendingIP}) || c.DMARC.Status != DMARCOK {
					t.Fatalf("%+v", c)
				}
			},
		},
		{
			name: "SPF fails for another IP", domain: "example.com", target: target,
			dns: fakeMailDNS{txt: map[string][]string{"smtp.example.net": {"v=spf1 ip4:198.51.100.1 -all"}}},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF.Result != "fail" || c.SPF.Aligned {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
		{
			name: "two SPF records is a permerror", domain: "example.com", target: target,
			dns: fakeMailDNS{txt: map[string][]string{"smtp.example.net": {"v=spf1 -all", "v=spf1 +all"}}},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF.Result != "permerror" {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
		{
			name: "no SPF record", domain: "example.com", target: target,
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF.Result != "none" {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
		{
			name: "a lookup failure is a temperror", domain: "example.com", target: target,
			dns: fakeMailDNS{txtErr: map[string]error{"smtp.example.net": &net.DNSError{Err: "timeout", IsTimeout: true, IsTemporary: true}}},
			check: func(t *testing.T, c SenderDomainDNSCheck, _ *fakeMailDNS) {
				if c.SPF.Result != "temperror" {
					t.Fatalf("%+v", c.SPF)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkSenderDomainDNS(context.Background(), &tc.dns, tc.domain, tc.target, tc.bounces, now)
			if got.Domain != tc.domain {
				t.Fatal(got.Domain)
			}
			if len(tc.dns.bad) > 0 {
				t.Fatal(tc.dns.bad)
			}
			tc.check(t, got, &tc.dns)
		})
	}
}

func TestSenderDomainDNSCancelledIsTemperror(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dns := &fakeMailDNS{txt: map[string][]string{"smtp.example.net": {"v=spf1 ip4:203.0.113.10 -all"}}}
	got := checkSenderDomainDNS(ctx, dns, "example.com", customdomain.Target{Name: "smtp.example.net", Address: "203.0.113.10"}, nil, time.Now())
	if got.SPF.Result != "temperror" {
		t.Fatalf("%+v", got.SPF)
	}
}

func dnsCheckPath(app, domain string) string {
	return "/api/v1/apps/" + app + "/domains/" + domain + "/check-dns"
}

func TestSenderDomainDNSRoute(t *testing.T) {
	app := strings.Repeat("a", 32)
	newHandler := func() (*Handler, *bounceFake) {
		h, store, _ := newBounceHandler()
		store.target = customdomain.Target{Name: "smtp.example.net", Address: "203.0.113.10"}
		h.MailAuthDNS = &fakeMailDNS{txt: map[string][]string{
			"smtp.example.net":   {"v=spf1 ip4:203.0.113.10 -all"},
			"_dmarc.example.com": {"v=DMARC1; p=reject"},
		}}
		return h, store
	}

	t.Run("reports both checks", func(t *testing.T) {
		h, store := newHandler()
		for _, name := range []string{"example.com", "EXAMPLE.com", "pending.example"} {
			w := send(h, "POST", dnsCheckPath(app, name), "", "valid")
			if w.Code != 200 {
				t.Fatal(name, w.Code, w.Body.String())
			}
		}
		w := send(h, "POST", dnsCheckPath(app, "EXAMPLE.com"), "", "valid")
		for _, want := range []string{`"domain":"example.com"`, `"status":"ok"`, `"policy":"reject"`, `"result":"pass"`, `"envelope_kind":"standard"`, `"aligned":false`} {
			if !strings.Contains(w.Body.String(), want) {
				t.Fatal(want, w.Body.String())
			}
		}
		if store.began != 0 || len(store.actors) != 0 {
			t.Fatal("recorded something", store.began, store.actors)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		h, _ := newHandler()
		for name, tc := range map[string]struct {
			method, path, body string
			want               int
		}{
			"unknown domain": {"POST", dnsCheckPath(app, "example.org"), "", 404},
			"GET":            {"GET", dnsCheckPath(app, "example.com"), "", 405},
			"body":           {"POST", dnsCheckPath(app, "example.com"), `{}`, 400},
			"query":          {"POST", dnsCheckPath(app, "example.com") + "?x=1", "", 400},
			"malformed name": {"POST", dnsCheckPath(app, "not_a_domain"), "", 400},
		} {
			if w := send(h, tc.method, tc.path, tc.body, "valid"); w.Code != tc.want {
				t.Fatal(name, w.Code, w.Body.String())
			}
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		h, _ := newHandler()
		h.MailAuthDNS = nil
		if w := send(h, "POST", dnsCheckPath(app, "example.com"), "", "valid"); w.Code != 503 {
			t.Fatal("no resolver", w.Code)
		}
		h, _ = newHandler()
		h.Store = &fakeStore{}
		if w := send(h, "POST", dnsCheckPath(app, "example.com"), "", "valid"); w.Code != 503 {
			t.Fatal("no bounce store", w.Code)
		}
	})

	t.Run("operators only", func(t *testing.T) {
		h, _ := newHandler()
		h.Authenticator = fakeAuth{admin: false}
		if w := send(h, "POST", dnsCheckPath(app, "example.com"), "", "valid"); w.Code != 403 {
			t.Fatal(w.Code)
		}
		h.Authenticator = fakeAuth{admin: true}
		if w := send(h, "POST", dnsCheckPath(app, "example.com"), "", ""); w.Code != 401 {
			t.Fatal(w.Code)
		}
	})
}
