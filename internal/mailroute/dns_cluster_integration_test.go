package mailroute

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// These fixtures run only in an explicitly created, isolated staging lab.
// They do not query external names or send mail.
func TestClusterDNSFixtureServer(t *testing.T) {
	if os.Getenv("THINPOST_TEST_DNS_CLUSTER_SERVER") != "1" {
		t.Skip("isolated cluster DNS fixture not enabled")
	}
	tcp, err := net.Listen("tcp4", "0.0.0.0:1053")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp4", "0.0.0.0:1053")
	if err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(request)
		response.RecursionAvailable = true
		if len(request.Question) != 1 {
			response.Rcode = dns.RcodeServerFailure
		} else {
			question := request.Question[0]
			switch {
			case question.Name == "fallback.fixture.invalid." && question.Qtype == dns.TypeMX:
				if w.RemoteAddr().Network() == "udp" {
					response.Truncated = true
					fmt.Println("fixture UDP truncation")
				} else {
					response.Answer = []dns.RR{&dns.MX{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeMX, Class: dns.ClassINET}, Preference: 10, Mx: "mail.fixture.invalid."}}
					fmt.Println("fixture TCP answer")
				}
			case question.Name == "routing.fixture.invalid." && question.Qtype == dns.TypeMX:
				response.Answer = []dns.RR{
					&dns.MX{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeMX, Class: dns.ClassINET}, Preference: 10, Mx: "slow.fixture.invalid."},
					&dns.MX{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeMX, Class: dns.ClassINET}, Preference: 20, Mx: "backup.fixture.invalid."},
				}
			case question.Name == "slow.fixture.invalid." && (question.Qtype == dns.TypeA || question.Qtype == dns.TypeAAAA):
				time.Sleep(time.Second)
				if question.Qtype == dns.TypeA {
					response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(8, 8, 4, 4)}}
				}
			case question.Name == "backup.fixture.invalid." && (question.Qtype == dns.TypeA || question.Qtype == dns.TypeAAAA):
				if question.Qtype == dns.TypeA {
					response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(8, 8, 8, 8)}}
				}
			default:
				response.Rcode = dns.RcodeServerFailure
			}
		}
		_ = w.WriteMsg(response)
	})
	udpServer := &dns.Server{PacketConn: udp, Handler: handler}
	tcpServer := &dns.Server{Listener: tcp, Handler: handler}
	defer func() { _ = udpServer.Shutdown(); _ = tcpServer.Shutdown() }()
	errs := make(chan error, 2)
	go func() { errs <- udpServer.ActivateAndServe() }()
	go func() { errs <- tcpServer.ActivateAndServe() }()
	fmt.Println("fixture DNS ready")
	select {
	case err := <-errs:
		t.Fatal("fixture DNS server stopped", err)
	case <-time.After(3 * time.Minute):
		t.Fatal("fixture DNS server timed out")
	}
}

func TestClusterDNSBoundedAlternate(t *testing.T) {
	address := os.Getenv("THINPOST_TEST_DNS_CLUSTER_ADDRESS")
	if address == "" {
		t.Skip("isolated cluster DNS target not configured")
	}
	ip, err := netip.ParseAddr(address)
	if err != nil || !ip.Is4() || !ip.IsPrivate() || ip.IsLoopback() {
		t.Fatal("fixture target must be a private IPv4 address")
	}
	server := net.JoinHostPort(ip.String(), "1053")
	resolver := &SystemDNS{
		config: &dns.ClientConfig{Servers: []string{ip.String()}, Port: "1053"},
		IPResolver: &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, server)
		}},
	}
	routes := &Resolver{DNS: resolver, Filter: filter{}, addressTimeout: 400 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	route, err := routes.ForRecipient(ctx, "test@routing.fixture.invalid", "fixture-delivery", 0)
	if err != nil || route.MXName != "backup.fixture.invalid" || route.Address.String() != "8.8.8.8" {
		t.Fatal("slow MX did not fall back", route, err)
	}
}

func TestClusterDNSTruncatedUDPFallsBackToTCP(t *testing.T) {
	address := os.Getenv("THINPOST_TEST_DNS_CLUSTER_ADDRESS")
	if address == "" {
		t.Skip("isolated cluster DNS target not configured")
	}
	ip, err := netip.ParseAddr(address)
	if err != nil || !ip.Is4() || !ip.IsPrivate() || ip.IsLoopback() {
		t.Fatal("fixture target must be a private IPv4 address")
	}
	resolver := &SystemDNS{config: &dns.ClientConfig{Servers: []string{ip.String()}, Port: strconv.Itoa(1053)}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mx, err := resolver.LookupMX(ctx, "fallback.fixture.invalid")
	if err != nil || len(mx) != 1 || mx[0].Host != "mail.fixture.invalid." || mx[0].Pref != 10 {
		t.Fatal("UDP truncation did not yield the TCP MX answer", err)
	}
	if _, err = resolver.LookupMX(ctx, "other.fixture.invalid"); err == nil {
		t.Fatal("failed resolver answer was accepted")
	}
}

func TestClusterDNSDeniedByPolicy(t *testing.T) {
	address := os.Getenv("THINPOST_TEST_DNS_CLUSTER_ADDRESS")
	if address == "" {
		t.Skip("isolated cluster DNS target not configured")
	}
	ip, err := netip.ParseAddr(address)
	if err != nil || !ip.Is4() || !ip.IsPrivate() || ip.IsLoopback() {
		t.Fatal("fixture target must be a private IPv4 address")
	}
	resolver := &SystemDNS{config: &dns.ClientConfig{Servers: []string{ip.String()}, Port: "1053"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = resolver.LookupMX(ctx, "fallback.fixture.invalid"); err == nil {
		t.Fatal("DNS traffic bypassed the isolated lab policy")
	}
}
