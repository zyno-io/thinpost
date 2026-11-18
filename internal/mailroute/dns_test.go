package mailroute

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/zyno-io/thinpost/internal/delivery"
)

type wireTestDNS struct {
	*SystemDNS
	addressLookups int
}

// Opt in only where the system resolver is expected to reach the public DNS.
// This exercises the actual resolv.conf and is not a DNSSEC validation test.
func TestSystemDNSLiveNullMX(t *testing.T) {
	if os.Getenv("THINPOST_TEST_LIVE_DNS") != "1" {
		t.Skip("live DNS test not enabled")
	}
	r := &Resolver{DNS: &SystemDNS{}, Filter: filter{}}
	if _, err := r.ForRecipient(context.Background(), "test@example.com", "delivery", 0); !errors.Is(err, delivery.ErrNullMX) {
		t.Fatal("live resolver did not preserve example.com's null MX", err)
	}
}

func TestSystemDNSLiveTCP(t *testing.T) {
	if os.Getenv("THINPOST_TEST_LIVE_DNS") != "1" {
		t.Skip("live DNS test not enabled")
	}
	config, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil || len(config.Servers) == 0 {
		t.Fatal("system DNS configuration unavailable", err)
	}
	request := new(dns.Msg)
	request.SetQuestion("example.com.", dns.TypeMX)
	client := &dns.Client{Net: "tcp"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range config.Servers {
		response, _, queryErr := client.ExchangeContext(ctx, request, net.JoinHostPort(server, config.Port))
		if queryErr == nil && response != nil && response.Rcode == dns.RcodeSuccess && !response.Truncated {
			for _, answer := range response.Answer {
				if mx, ok := answer.(*dns.MX); ok && strings.EqualFold(mx.Hdr.Name, "example.com.") && mx.Preference == 0 && mx.Mx == "." {
					return
				}
			}
		}
	}
	t.Fatal("system resolvers did not return example.com's null MX over TCP")
}

// The caller selects stable test names for its DNS environment. These tests
// issue DNS queries only; they do not connect to or send mail to the domains.
func TestSystemDNSLiveConfiguredNegatives(t *testing.T) {
	if os.Getenv("THINPOST_TEST_LIVE_DNS") != "1" {
		t.Skip("live DNS test not enabled")
	}
	nodata := os.Getenv("THINPOST_TEST_LIVE_NODATA_NAME")
	nxdomain := os.Getenv("THINPOST_TEST_LIVE_NXDOMAIN_NAME")
	if nodata == "" || nxdomain == "" {
		t.Skip("live DNS negative test names not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := &SystemDNS{}
	if mx, err := r.LookupMX(ctx, nodata); err != nil || len(mx) != 0 {
		t.Fatal("configured NODATA name did not return an empty MX result", err)
	}
	if _, err := r.LookupMX(ctx, nxdomain); !errors.Is(err, delivery.ErrDomainNotFound) {
		t.Fatal("configured nonexistent name did not return NXDOMAIN", err)
	}
}

func (d *wireTestDNS) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	d.addressLookups++
	return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
}

func TestSystemDNSPreservesMXWireSemantics(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := tcp.Addr().(*net.TCPAddr).Port
	udp, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}
	var fallbackQueries atomic.Int32
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(request)
		response.RecursionAvailable = true
		name := request.Question[0].Name
		if name == "truncated.example." && w.RemoteAddr().Network() == "udp" {
			response.Truncated = true
		} else {
			switch name {
			case "null.example.", "target.example.":
				response.Answer = []dns.RR{&dns.MX{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeMX, Class: dns.ClassINET}, Mx: ".", Preference: 0}}
			case "alias.example.":
				response.Answer = []dns.RR{&dns.CNAME{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET}, Target: "target.example."}}
			case "loop.example.":
				response.Answer = []dns.RR{&dns.CNAME{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET}, Target: name}}
			case "missing.example.":
				response.Rcode = dns.RcodeNameError
			case "alias-missing.example.":
				response.Rcode = dns.RcodeNameError
				response.Answer = []dns.RR{&dns.CNAME{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET}, Target: "missing.example."}}
			case "referral-missing.example.":
				response.Rcode = dns.RcodeNameError
				response.Ns = []dns.RR{&dns.NS{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeNS, Class: dns.ClassINET}, Ns: "ns.example."}}
			case "incomplete.example.":
				response.RecursionAvailable = false
			case "incomplete-missing.example.":
				response.RecursionAvailable = false
				response.Rcode = dns.RcodeNameError
			case "contradictory-missing.example.":
				response.Rcode = dns.RcodeNameError
				response.Answer = []dns.RR{&dns.MX{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeMX, Class: dns.ClassINET}, Mx: "mail.example.", Preference: 10}}
			case "unrelated.example.":
				response.Answer = []dns.RR{&dns.MX{Hdr: dns.RR_Header{Name: "other.example.", Rrtype: dns.TypeMX, Class: dns.ClassINET}, Mx: "mail.example.", Preference: 10}}
			case "wrong-class.example.":
				response.Answer = []dns.RR{&dns.MX{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeMX, Class: dns.ClassCHAOS}, Mx: "mail.example.", Preference: 10}}
			case "dname-only.example.":
				response.Answer = []dns.RR{&dns.DNAME{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeDNAME, Class: dns.ClassINET}, Target: "other.example."}}
			case "address-missing.example.":
				response.Rcode = dns.RcodeNameError
				response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.ParseIP("192.0.2.1").To4()}}
			case "referral.example.":
				response.Ns = []dns.RR{&dns.NS{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeNS, Class: dns.ClassINET}, Ns: "ns.example."}}
			case "negative-soa.example.":
				response.Ns = []dns.RR{
					&dns.NS{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeNS, Class: dns.ClassINET}, Ns: "ns.example."},
					&dns.SOA{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeSOA, Class: dns.ClassINET}, Ns: "ns.example.", Mbox: "hostmaster.example."},
				}
			case "authoritative-empty.example.":
				response.RecursionAvailable = false
				response.Authoritative = true
			case "fallback-server.example.":
				if fallbackQueries.Add(1) == 1 {
					response.Ns = []dns.RR{&dns.NS{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeNS, Class: dns.ClassINET}, Ns: "ns.example."}}
				} else {
					response.Answer = []dns.RR{&dns.MX{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeMX, Class: dns.ClassINET}, Mx: "mail.example.", Preference: 10}}
				}
			case "failure.example.":
				response.Rcode = dns.RcodeServerFailure
			case "truncated.example.":
				response.Answer = []dns.RR{&dns.MX{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeMX, Class: dns.ClassINET}, Mx: "mail.example.", Preference: 10}}
			}
		}
		_ = w.WriteMsg(response)
	})
	udpServer := &dns.Server{PacketConn: udp, Handler: handler}
	tcpServer := &dns.Server{Listener: tcp, Handler: handler}
	go func() { _ = udpServer.ActivateAndServe() }()
	go func() { _ = tcpServer.ActivateAndServe() }()
	t.Cleanup(func() { _ = udpServer.Shutdown(); _ = tcpServer.Shutdown() })
	resolver := &SystemDNS{config: &dns.ClientConfig{Servers: []string{"127.0.0.1"}, Port: strconv.Itoa(port)}}
	for _, name := range []string{"null.example", "alias.example"} {
		mx, err := resolver.LookupMX(context.Background(), name)
		if err != nil || len(mx) != 1 || mx[0].Host != "." || mx[0].Pref != 0 {
			t.Fatalf("%s: null MX lost: %v %v", name, mx, err)
		}
	}
	if mx, err := resolver.LookupMX(context.Background(), "empty.example"); err != nil || len(mx) != 0 {
		t.Fatal("NODATA is not an empty MX answer", mx, err)
	}
	if _, err := resolver.LookupMX(context.Background(), "missing.example"); !errors.Is(err, delivery.ErrDomainNotFound) {
		t.Fatal("NXDOMAIN was not distinguished", err)
	}
	if _, err := resolver.LookupMX(context.Background(), "alias-missing.example"); !errors.Is(err, delivery.ErrDomainNotFound) {
		t.Fatal("negative answer with a CNAME was not classified as NXDOMAIN", err)
	}
	for _, name := range []string{"failure.example", "loop.example", "incomplete.example", "incomplete-missing.example", "contradictory-missing.example", "address-missing.example", "unrelated.example", "wrong-class.example", "dname-only.example", "referral.example", "referral-missing.example"} {
		if _, err := resolver.LookupMX(context.Background(), name); err == nil || errors.Is(err, delivery.ErrDomainNotFound) {
			t.Fatal("temporary or malformed answer was treated as permanent", name, err)
		}
	}
	if mx, err := resolver.LookupMX(context.Background(), "truncated.example"); err != nil || len(mx) != 1 || mx[0].Host != "mail.example." {
		t.Fatal("truncated UDP answer did not fall back to TCP", mx, err)
	}
	for _, name := range []string{"negative-soa.example", "authoritative-empty.example"} {
		if mx, err := resolver.LookupMX(context.Background(), name); err != nil || len(mx) != 0 {
			t.Fatal("valid negative answer did not allow an implicit MX", name, mx, err)
		}
	}
	secondResolver := &SystemDNS{config: &dns.ClientConfig{Servers: []string{"127.0.0.1", "127.0.0.1"}, Port: strconv.Itoa(port)}}
	if mx, err := secondResolver.LookupMX(context.Background(), "fallback-server.example"); err != nil || len(mx) != 1 || mx[0].Host != "mail.example." || fallbackQueries.Load() != 2 {
		t.Fatal("resolver did not try another server after a referral", mx, err, fallbackQueries.Load())
	}
	wire := &wireTestDNS{SystemDNS: resolver}
	mailResolver := &Resolver{DNS: wire, Filter: filter{}}
	for _, tc := range []struct {
		recipient string
		want      error
	}{{"user@null.example", delivery.ErrNullMX}, {"user@missing.example", delivery.ErrDomainNotFound}} {
		if _, err := mailResolver.ForRecipient(context.Background(), tc.recipient, "delivery", 0); !errors.Is(err, tc.want) {
			t.Fatal("wire DNS conclusion did not reach mail routing", tc.recipient, err)
		}
	}
	if wire.addressLookups != 0 {
		t.Fatal("looked up an implicit MX for a permanent DNS result")
	}
	for _, name := range []string{"referral.example", "referral-missing.example", "incomplete.example", "incomplete-missing.example", "contradictory-missing.example", "address-missing.example", "unrelated.example", "wrong-class.example", "dname-only.example"} {
		if _, err := mailResolver.ForRecipient(context.Background(), "user@"+name, "delivery", 0); err == nil || errors.Is(err, delivery.ErrDomainNotFound) {
			t.Fatal("unusable DNS response became a route or permanent failure", name, err)
		}
	}
	if wire.addressLookups != 0 {
		t.Fatal("unusable DNS response caused an implicit MX lookup")
	}
	if route, err := mailResolver.ForRecipient(context.Background(), "user@empty.example", "delivery", 0); err != nil || route.MXName != "empty.example" || wire.addressLookups != 1 {
		t.Fatal("NODATA did not use the implicit MX", route, err)
	}
}
