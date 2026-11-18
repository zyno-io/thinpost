package mailroute

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/zyno-io/thinpost/internal/delivery"
)

// SystemDNS uses the pod's configured recursive resolvers for MX lookups.
// Go's net.Resolver filters the root-label exchanger used by null MX. Address
// lookups still use the standard resolver. DNS answers are not authenticated.
type SystemDNS struct {
	ConfigPath string
	IPResolver *net.Resolver
	config     *dns.ClientConfig // injected by wire-protocol tests
}

func (s *SystemDNS) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r := s.IPResolver
	if r == nil {
		r = net.DefaultResolver
	}
	return r.LookupNetIP(ctx, network, host)
}

func (s *SystemDNS) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	path := s.ConfigPath
	if path == "" {
		path = "/etc/resolv.conf"
	}
	config := s.config
	var err error
	if config == nil {
		config, err = dns.ClientConfigFromFile(path)
	}
	if err != nil || len(config.Servers) == 0 || len(config.Servers) > 8 {
		return nil, errors.New("DNS resolver configuration unavailable")
	}
	name = dns.Fqdn(name)
	seen := make(map[string]bool)
	for depth := 0; depth < 10; depth++ {
		key := strings.ToLower(name)
		if seen[key] {
			return nil, errors.New("DNS alias loop")
		}
		seen[key] = true
		response, err := exchangeMX(ctx, config, name)
		if err != nil {
			return nil, err
		}
		if response.Rcode == dns.RcodeNameError {
			return nil, delivery.ErrDomainNotFound
		}
		var records []*net.MX
		var alias string
		for _, answer := range response.Answer {
			if answer.Header().Class != dns.ClassINET || !strings.EqualFold(answer.Header().Name, name) {
				continue
			}
			switch record := answer.(type) {
			case *dns.MX:
				records = append(records, &net.MX{Host: record.Mx, Pref: record.Preference})
			case *dns.CNAME:
				if alias != "" && !strings.EqualFold(alias, record.Target) {
					return nil, errors.New("invalid DNS aliases")
				}
				alias = record.Target
			}
		}
		if len(records) > 0 && alias != "" {
			return nil, errors.New("invalid DNS answer")
		}
		if len(records) > 0 || alias == "" {
			return records, nil
		}
		name = alias
	}
	return nil, errors.New("too many DNS aliases")
}

func exchangeMX(ctx context.Context, config *dns.ClientConfig, name string) (*dns.Msg, error) {
	for _, server := range config.Servers {
		if _, err := netip.ParseAddr(server); err != nil {
			continue
		}
		address := net.JoinHostPort(server, config.Port)
		request := new(dns.Msg)
		request.SetQuestion(name, dns.TypeMX)
		request.RecursionDesired = true
		client := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
		response, _, err := client.ExchangeContext(ctx, request, address)
		if err == nil && response != nil && response.Truncated {
			client.Net = "tcp"
			response, _, err = client.ExchangeContext(ctx, request, address)
		}
		if err != nil || response == nil || !response.Response || response.Opcode != dns.OpcodeQuery || response.Truncated || len(response.Question) != 1 || !strings.EqualFold(response.Question[0].Name, name) || response.Question[0].Qtype != dns.TypeMX || response.Question[0].Qclass != dns.ClassINET {
			continue
		}
		if (response.Rcode == dns.RcodeSuccess || response.Rcode == dns.RcodeNameError) && usableMXAnswer(response, name) {
			return response, nil
		}
	}
	return nil, errors.New("MX lookup failed")
}

// Empty or negative replies must not turn a referral or an incomplete
// non-recursive response into an implicit MX or a permanent domain failure.
// This is structural validation only; it does not authenticate DNS data.
func usableMXAnswer(response *dns.Msg, name string) bool {
	var relevant, unexpected bool
	for _, answer := range response.Answer {
		if answer.Header().Class != dns.ClassINET {
			if response.Rcode == dns.RcodeNameError {
				return false
			}
			unexpected = true
			continue
		}
		switch answer.(type) {
		case *dns.MX:
			if response.Rcode == dns.RcodeNameError {
				return false
			}
			if strings.EqualFold(answer.Header().Name, name) {
				relevant = true
			} else {
				unexpected = true
			}
		case *dns.CNAME:
			if strings.EqualFold(answer.Header().Name, name) {
				relevant = true
			} else {
				unexpected = true
			}
		case *dns.RRSIG:
			// DNSSEC signatures may accompany a CNAME chain.
		case *dns.DNAME:
			// A DNAME response needs its synthesized CNAME; without it,
			// an empty MX result would be an unsafe interpretation.
			unexpected = true
		default:
			if response.Rcode == dns.RcodeNameError {
				return false
			}
			unexpected = true
		}
	}
	if response.Rcode == dns.RcodeSuccess && relevant {
		return true
	}
	if !relevant && unexpected {
		return false
	}
	if !response.Authoritative && !response.RecursionAvailable {
		return false
	}
	var hasSOA, hasNS bool
	for _, authority := range response.Ns {
		if authority.Header().Class != dns.ClassINET {
			continue
		}
		switch authority.(type) {
		case *dns.SOA:
			hasSOA = true
		case *dns.NS:
			hasNS = true
		}
	}
	return !hasNS || hasSOA
}
