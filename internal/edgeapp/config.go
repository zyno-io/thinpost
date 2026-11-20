// Package edgeapp validates edge process configuration. It makes no network
// or deployment changes and does not establish that an IP is safe to own.
package edgeapp

import (
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/egress"
	"github.com/zyno-io/thinpost/internal/smtptransport"
)

type Config struct {
	WorkerID, Hostname, BounceDomain string
	ControlListen, SMTPListen        string
	Source                           netip.Addr
	// AdditionalSources are further addresses the Edge binds. Each accepts SMTP
	// and sends from its own address under its own name (AdditionalHostnames,
	// same order) and certificate (<AdditionalCertRoot>/<N>/tls.crt and
	// tls.key). Sources lists the primary first, then these. BounceDomains
	// lists the global bounce domain first, then every listener name, then the
	// explicit extras.
	AdditionalSources []netip.Addr
	// SourceGateways holds each source's gateway, in Sources() order. Empty
	// means no neighbour announcement (and no readiness gate on it).
	SourceGateways                         []netip.Addr
	AdditionalHostnames                    []string
	AdditionalCertRoot                     string
	BounceDomains                          []string
	ControllerIdentities                   []string
	ControlCA, ControlCert, ControlKey     string
	SMTPCert, SMTPKey, JournalPath         string
	JournalCapacity                        int
	JournalReserveBytes                    int64
	ClockMargin                            time.Duration
	SMTPGlobalInterval, SMTPSourceInterval time.Duration
	SMTPGlobalBurst, SMTPSourceBurst       int
	OutboundSMTPTimeouts                   smtptransport.Timeouts

	// Submission opens authenticated submission on every sending IP: 587
	// (STARTTLS) and 465 (implicit TLS), each presenting that IP's own listener
	// certificate. Port 25 is unchanged.
	Submission bool
	// SubmissionInternal is the optional in-cluster submission listener (STARTTLS
	// with a certificate from the cluster-local CA). It is not shown to clients.
	SubmissionInternal SubmissionInternal
}

// SubmissionInternal configures the optional internal submission listener.
type SubmissionInternal struct {
	// Listen is a literal private (or loopback) ip:port; empty disables it.
	Listen netip.AddrPort
	// Hostname is the name it greets with and its certificate is issued for.
	Hostname string
	// Cert and Key are the certificate files; CA is the cluster-local CA that
	// verifies the certificate.
	Cert, Key, CA string
}

// Enabled reports whether the internal listener is configured.
func (i SubmissionInternal) Enabled() bool { return i.Listen.IsValid() }

// SubmissionPublicPorts are the ports submission opens on every sending IP.
var SubmissionPublicPorts = []uint32{587, 465}

// Load requires literal private control and public SMTP bind addresses. The
// public source address is explicit and is also used for outbound SMTP.
func Load(getenv func(string) string) (Config, error) {
	var c Config
	c.WorkerID = getenv("THINPOST_EDGE_WORKER_ID")
	c.Hostname = getenv("THINPOST_EDGE_SMTP_HOSTNAME")
	c.BounceDomain = getenv("THINPOST_EDGE_BOUNCE_DOMAIN")
	c.ControlListen = getenv("THINPOST_EDGE_CONTROL_LISTEN")
	c.SMTPListen = getenv("THINPOST_EDGE_SMTP_LISTEN")
	c.ControlCA = getenv("THINPOST_EDGE_CONTROL_CA")
	c.ControlCert = getenv("THINPOST_EDGE_CONTROL_CERT")
	c.ControlKey = getenv("THINPOST_EDGE_CONTROL_KEY")
	c.SMTPCert = getenv("THINPOST_EDGE_SMTP_CERT")
	c.SMTPKey = getenv("THINPOST_EDGE_SMTP_KEY")
	c.JournalPath = getenv("THINPOST_EDGE_JOURNAL_PATH")
	if !validID(c.WorkerID) || c.ControlCA == "" || c.ControlCert == "" || c.ControlKey == "" || c.SMTPCert == "" || c.SMTPKey == "" || c.JournalPath == "" {
		return Config{}, errors.New("missing edge configuration")
	}
	for _, value := range []string{c.Hostname, c.BounceDomain} {
		normalized, ok := admin.NormalizeDomain(value)
		if !ok || normalized != value {
			return Config{}, errors.New("invalid edge DNS name")
		}
	}
	if len(c.Hostname) > 219 {
		return Config{}, errors.New("SMTP hostname is too long")
	}
	if len(c.BounceDomain) > 219 {
		return Config{}, errors.New("bounce domain is too long")
	}
	controlHost, controlPort, err := net.SplitHostPort(c.ControlListen)
	if err != nil {
		return Config{}, errors.New("invalid control listener")
	}
	controlAddr, err := netip.ParseAddr(controlHost)
	if err != nil || controlAddr.Zone() != "" || !(controlAddr.IsPrivate() || controlAddr.IsLoopback()) || !validPort(controlPort) {
		return Config{}, errors.New("control listener must use a literal private address")
	}
	publicHost, publicPort, err := net.SplitHostPort(c.SMTPListen)
	if err != nil || publicPort != "25" {
		return Config{}, errors.New("SMTP listener must use port 25")
	}
	c.Source, err = netip.ParseAddr(publicHost)
	if err != nil || c.Source.Zone() != "" || c.Source.Is4In6() {
		return Config{}, errors.New("SMTP listener must use a literal source IP")
	}
	// The primary is reported to control in Ready, which accepts IPv4 sources only.
	if !c.Source.Is4() {
		return Config{}, errors.New("SMTP listener must use an IPv4 address")
	}
	// The Edge applies no destination policy: control is the sole authority for
	// the egress deny ranges and route exceptions. THINPOST_EDGE_INFRASTRUCTURE_CIDRS
	// and THINPOST_EDGE_ROUTE_EXCEPTIONS_FILE from an earlier release are
	// therefore never read, so an Edge that still has them set starts normally.
	if c.AdditionalSources, err = parseAdditionalSources(getenv("THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES")); err != nil {
		return Config{}, err
	}
	if c.AdditionalHostnames, err = parseAdditionalHostnames(getenv("THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES"), c.Hostname, len(c.AdditionalSources)); err != nil {
		return Config{}, err
	}
	if len(c.AdditionalSources) > 0 {
		c.AdditionalCertRoot = getenv("THINPOST_EDGE_SMTP_ADDITIONAL_CERT_ROOT")
		if !filepath.IsAbs(c.AdditionalCertRoot) || filepath.Clean(c.AdditionalCertRoot) != c.AdditionalCertRoot {
			return Config{}, errors.New("additional SMTP certificate root must be a clean absolute path")
		}
	}
	if _, err = egress.NewSourceDialer(c.Sources()); err != nil {
		return Config{}, errors.New("invalid SMTP source address")
	}
	if c.SourceGateways, err = parseSourceGateways(getenv("THINPOST_EDGE_SOURCE_GATEWAYS"), len(c.Sources())); err != nil {
		return Config{}, err
	}
	if c.BounceDomains, err = parseBounceDomains(c.BounceDomain, append([]string{c.Hostname}, c.AdditionalHostnames...), getenv("THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS")); err != nil {
		return Config{}, err
	}
	for _, value := range strings.Split(getenv("THINPOST_EDGE_CONTROLLER_IDENTITIES"), ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			return Config{}, errors.New("controller identities required")
		}
		c.ControllerIdentities = append(c.ControllerIdentities, value)
	}
	c.JournalCapacity, err = strconv.Atoi(getenv("THINPOST_EDGE_JOURNAL_CAPACITY"))
	if err != nil || c.JournalCapacity <= 0 || c.JournalCapacity > 100000 {
		return Config{}, errors.New("invalid journal capacity")
	}
	c.JournalReserveBytes, err = strconv.ParseInt(getenv("THINPOST_EDGE_JOURNAL_RESERVE_BYTES"), 10, 64)
	if err != nil || c.JournalReserveBytes < 1<<20 || c.JournalReserveBytes > 1<<30 {
		return Config{}, errors.New("invalid journal reserve size")
	}
	c.ClockMargin, err = time.ParseDuration(getenv("THINPOST_EDGE_CLOCK_MARGIN"))
	if err != nil || c.ClockMargin <= 0 || c.ClockMargin > 30*time.Second {
		return Config{}, errors.New("invalid clock margin")
	}
	c.OutboundSMTPTimeouts = smtptransport.DefaultTimeouts()
	for _, item := range []struct {
		key   string
		value *time.Duration
	}{
		{"THINPOST_EDGE_SMTP_CONNECT_TIMEOUT", &c.OutboundSMTPTimeouts.Connect},
		{"THINPOST_EDGE_SMTP_GREETING_TIMEOUT", &c.OutboundSMTPTimeouts.Greeting},
		{"THINPOST_EDGE_SMTP_COMMAND_TIMEOUT", &c.OutboundSMTPTimeouts.Command},
		{"THINPOST_EDGE_SMTP_DATA_INIT_TIMEOUT", &c.OutboundSMTPTimeouts.DataInit},
		{"THINPOST_EDGE_SMTP_DATA_BLOCK_TIMEOUT", &c.OutboundSMTPTimeouts.DataBlock},
		{"THINPOST_EDGE_SMTP_DATA_FINAL_TIMEOUT", &c.OutboundSMTPTimeouts.DataFinal},
	} {
		if value := getenv(item.key); value != "" {
			*item.value, err = time.ParseDuration(value)
			if err != nil {
				return Config{}, errors.New("invalid SMTP stage timeout")
			}
		}
	}
	if err = c.OutboundSMTPTimeouts.Validate(); err != nil {
		return Config{}, err
	}
	// Optional bounded defaults protect the listener before SMTP processing.
	// Operators must tune these for actual sender traffic and monitor drops.
	for _, item := range []struct {
		key      string
		value    *time.Duration
		fallback time.Duration
	}{
		{"THINPOST_EDGE_SMTP_GLOBAL_INTERVAL", &c.SMTPGlobalInterval, 100 * time.Millisecond},
		{"THINPOST_EDGE_SMTP_SOURCE_INTERVAL", &c.SMTPSourceInterval, time.Second},
	} {
		*item.value = item.fallback
		if value := getenv(item.key); value != "" {
			*item.value, err = time.ParseDuration(value)
			if err != nil || *item.value < 10*time.Millisecond || *item.value > time.Hour {
				return Config{}, errors.New("invalid SMTP connection rate interval")
			}
		}
	}
	for _, item := range []struct {
		key      string
		value    *int
		fallback int
	}{
		{"THINPOST_EDGE_SMTP_GLOBAL_BURST", &c.SMTPGlobalBurst, 100},
		{"THINPOST_EDGE_SMTP_SOURCE_BURST", &c.SMTPSourceBurst, 12},
	} {
		*item.value = item.fallback
		if value := getenv(item.key); value != "" {
			*item.value, err = strconv.Atoi(value)
			if err != nil || *item.value < 1 || *item.value > 1000 {
				return Config{}, errors.New("invalid SMTP connection rate burst")
			}
		}
	}
	if err = c.loadSubmission(getenv, controlAddr, controlPort); err != nil {
		return Config{}, err
	}
	return c, nil
}

// loadSubmission reads the submission settings. Submission is off unless
// THINPOST_EDGE_SUBMISSION is exactly "true".
func (c *Config) loadSubmission(getenv func(string) string, controlAddr netip.Addr, controlPort string) error {
	switch getenv("THINPOST_EDGE_SUBMISSION") {
	case "", "false":
	case "true":
		c.Submission = true
	default:
		return errors.New("THINPOST_EDGE_SUBMISSION must be true or false")
	}
	listen := getenv("THINPOST_EDGE_SUBMISSION_INTERNAL_LISTEN")
	hostname := getenv("THINPOST_EDGE_SUBMISSION_INTERNAL_HOSTNAME")
	cert, key, ca := getenv("THINPOST_EDGE_SUBMISSION_INTERNAL_CERT"), getenv("THINPOST_EDGE_SUBMISSION_INTERNAL_KEY"), getenv("THINPOST_EDGE_SUBMISSION_INTERNAL_CA")
	if listen == "" {
		if hostname != "" || cert != "" || key != "" || ca != "" {
			return errors.New("the internal submission settings need THINPOST_EDGE_SUBMISSION_INTERNAL_LISTEN")
		}
		return nil
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return errors.New("invalid internal submission listener")
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.Zone() != "" || address.IsUnspecified() || !(address.IsPrivate() || address.IsLoopback()) || !validPort(port) {
		return errors.New("internal submission listener must use a literal private address")
	}
	if port == "25" || port == controlPort && address == controlAddr {
		return errors.New("internal submission listener port conflicts with another listener")
	}
	normalized, ok := admin.NormalizeDomain(hostname)
	if !ok || normalized != hostname || len(hostname) > 219 || cert == "" || key == "" || ca == "" {
		return errors.New("the internal submission listener needs a hostname, certificate, key and CA")
	}
	c.SubmissionInternal = SubmissionInternal{Listen: netip.AddrPortFrom(address, mustPort(port)), Hostname: hostname, Cert: cert, Key: key, CA: ca}
	return nil
}

func mustPort(value string) uint16 {
	port, _ := strconv.Atoi(value)
	return uint16(port)
}

// MaxAdditionalSources and MaxBounceDomains bound the optional lists. The
// primary plus these stay within the 16 addresses a worker may advertise.
const (
	MaxAdditionalSources = 15
	MaxBounceDomains     = 24
)

// Listener is one SMTP listener: the address the Edge binds on port 25, the
// name it greets with, and the certificate files for that name.
type Listener struct {
	Address             netip.Addr
	Hostname, Cert, Key string
}

// Listeners returns every SMTP listener, the primary first. Each additional
// source has its own certificate under AdditionalCertRoot, numbered from 0.
func (c Config) Listeners() []Listener {
	out := []Listener{{Address: c.Source, Hostname: c.Hostname, Cert: c.SMTPCert, Key: c.SMTPKey}}
	for i, address := range c.AdditionalSources {
		dir := filepath.Join(c.AdditionalCertRoot, strconv.Itoa(i))
		out = append(out, Listener{Address: address, Hostname: c.AdditionalHostnames[i], Cert: filepath.Join(dir, "tls.crt"), Key: filepath.Join(dir, "tls.key")})
	}
	return out
}

// Sources returns every outbound source, the primary (the listener) first.
func (c Config) Sources() []netip.Addr {
	return append([]netip.Addr{c.Source}, c.AdditionalSources...)
}

// parseAdditionalSources reads a comma-separated list of literal public IPv4
// addresses. Uniqueness, including the primary, is checked by
// egress.NewSourceDialer.
func parseAdditionalSources(value string) ([]netip.Addr, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var out []netip.Addr
	for _, item := range strings.Split(value, ",") {
		address, err := netip.ParseAddr(strings.TrimSpace(item))
		if err != nil || !egress.PublicSource(address) {
			return nil, errors.New("additional SMTP sources must be literal public IPv4 addresses")
		}
		out = append(out, address)
	}
	if len(out) > MaxAdditionalSources {
		return nil, errors.New("too many additional SMTP sources")
	}
	return out, nil
}

// parseAdditionalHostnames reads one SMTP name per additional source, in the
// same order. Each is normalized, unique, and different from the primary name.
func parseAdditionalHostnames(value, primary string, sources int) ([]string, error) {
	if sources == 0 {
		if strings.TrimSpace(value) != "" {
			return nil, errors.New("additional SMTP hostnames need additional SMTP sources")
		}
		return nil, nil
	}
	items := strings.Split(value, ",")
	if strings.TrimSpace(value) == "" || len(items) != sources {
		return nil, errors.New("each additional SMTP source needs exactly one hostname")
	}
	seen := map[string]struct{}{primary: {}}
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		normalized, ok := admin.NormalizeDomain(item)
		if !ok || normalized != item || len(item) > 219 {
			return nil, errors.New("invalid additional SMTP hostname")
		}
		if _, duplicate := seen[item]; duplicate {
			return nil, errors.New("duplicate SMTP hostname")
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out, nil
}

// parseBounceDomains returns the global bounce domain, then every listener
// name not already present (each listener accepts its own name as a bounce
// domain), then the optional comma-separated extras. Extras are for retired
// domains and must not repeat anything already listed.
func parseBounceDomains(global string, listenerNames []string, value string) ([]string, error) {
	domains := []string{global}
	seen := map[string]struct{}{global: {}}
	for _, name := range listenerNames {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			domains = append(domains, name)
		}
	}
	if strings.TrimSpace(value) != "" {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			normalized, ok := admin.NormalizeDomain(item)
			if !ok || normalized != item || len(item) > 219 {
				return nil, errors.New("invalid additional bounce domain")
			}
			if _, duplicate := seen[item]; duplicate {
				return nil, errors.New("duplicate bounce domain")
			}
			seen[item] = struct{}{}
			domains = append(domains, item)
		}
	}
	if len(domains) > MaxBounceDomains {
		return nil, errors.New("too many bounce domains")
	}
	return domains, nil
}
func validPort(value string) bool {
	port, err := strconv.Atoi(value)
	return err == nil && port > 0 && port <= 65535
}
func validID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// parseSourceGateways reads one literal IPv4 gateway per source, primary first.
// An empty value disables announcement; otherwise the count must match.
func parseSourceGateways(value string, sources int) ([]netip.Addr, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var out []netip.Addr
	for _, item := range strings.Split(value, ",") {
		a, err := netip.ParseAddr(strings.TrimSpace(item))
		if err != nil || !a.Is4() {
			return nil, errors.New("invalid source gateway")
		}
		out = append(out, a)
	}
	if len(out) != sources {
		return nil, errors.New("source gateway count must match source count")
	}
	return out, nil
}
