package mailroute

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/egress"
)

type fakeDNS struct {
	mx        []*net.MX
	mxErr     error
	addresses map[string][]netip.Addr
}

func TestInfrastructureExceptionIsAppliedToResolvedRecipientAndMX(t *testing.T) {
	address := netip.MustParseAddr("45.0.1.21")
	policy, err := egress.NewPolicy(
		[]netip.Prefix{netip.MustParsePrefix("45.0.1.0/24")},
		egress.Exception{Recipient: "test@mailhost.example", MX: "mail.mailhost.example", Address: address},
	)
	if err != nil {
		t.Fatal(err)
	}
	dns := fakeDNS{mx: []*net.MX{{Host: "mail.mailhost.example."}}, addresses: map[string][]netip.Addr{"mail.mailhost.example": {address}}}
	resolver := &Resolver{DNS: dns, Filter: policy}
	if got, err := resolver.ForRecipient(context.Background(), "test@mailhost.example", "delivery", 0); err != nil || got.Address != address || got.MXName != "mail.mailhost.example" {
		t.Fatalf("approved route unavailable: %+v, %v", got, err)
	}
	if _, err := resolver.ForRecipient(context.Background(), "other@mailhost.example", "delivery", 0); err == nil {
		t.Fatal("allowed exception for a different mailbox")
	}
	resolver.DNS = fakeDNS{mx: []*net.MX{{Host: "other.example."}}, addresses: map[string][]netip.Addr{"other.example": {address}}}
	if _, err := resolver.ForRecipient(context.Background(), "test@mailhost.example", "delivery", 0); err == nil {
		t.Fatal("allowed infrastructure address under a different MX")
	}
}

func (d fakeDNS) LookupMX(context.Context, string) ([]*net.MX, error) { return d.mx, d.mxErr }
func (d fakeDNS) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	v, ok := d.addresses[host]
	if !ok {
		return nil, errors.New("unavailable")
	}
	return v, nil
}

type filter struct{}

func (filter) AllowedFor(ip netip.Addr, _, _ string) bool {
	return ip.IsValid() && !ip.Unmap().IsPrivate() && !ip.Unmap().IsLoopback()
}
func TestMXPreferenceAndDestinationFiltering(t *testing.T) {
	r := &Resolver{DNS: fakeDNS{mx: []*net.MX{{Host: "backup.example.", Pref: 20}, {Host: "primary.example.", Pref: 10}}, addresses: map[string][]netip.Addr{"primary.example": {netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("8.8.8.8")}, "backup.example": {netip.MustParseAddr("1.1.1.1")}}}, Filter: filter{}}
	got, err := r.ForRecipient(context.Background(), "user@example.net", "delivery", 0)
	if err != nil || got.MXName != "primary.example" || got.Address.String() != "8.8.8.8" {
		t.Fatalf("route %+v %v", got, err)
	}
}
func TestImplicitAndNullMX(t *testing.T) {
	r := &Resolver{DNS: fakeDNS{addresses: map[string][]netip.Addr{"example.net": {netip.MustParseAddr("8.8.8.8")}}}, Filter: filter{}}
	if got, err := r.ForRecipient(context.Background(), "user@example.net", "delivery", 0); err != nil || got.MXName != "example.net" {
		t.Fatal("implicit MX failed")
	}
	r.DNS = fakeDNS{mx: []*net.MX{{Host: ".", Pref: 0}}}
	if _, err := r.ForRecipient(context.Background(), "user@example.net", "delivery", 0); !errors.Is(err, delivery.ErrNullMX) {
		t.Fatal("null MX accepted")
	}
	r.DNS = fakeDNS{mxErr: delivery.ErrDomainNotFound}
	if _, err := r.ForRecipient(context.Background(), "user@example.net", "delivery", 0); !errors.Is(err, delivery.ErrDomainNotFound) {
		t.Fatal("NXDOMAIN was not classified as permanent", err)
	}
	r.DNS = fakeDNS{mxErr: &net.DNSError{IsTemporary: true}}
	if _, err := r.ForRecipient(context.Background(), "user@example.net", "delivery", 0); err == nil {
		t.Fatal("temporary DNS failure treated as implicit MX")
	}
	r.DNS = fakeDNS{mx: []*net.MX{{Host: "."}, {Host: "mx.example."}}}
	if _, err := r.ForRecipient(context.Background(), "user@example.net", "delivery", 0); err == nil || errors.Is(err, delivery.ErrNullMX) {
		t.Fatal("malformed mixed null MX was treated as a permanent conclusion", err)
	}
}

func TestRetriesRotateThroughDeterministicRoutes(t *testing.T) {
	r := &Resolver{DNS: fakeDNS{
		mx: []*net.MX{{Host: "backup.example.", Pref: 20}, {Host: "primary.example.", Pref: 10}, {Host: "PRIMARY.example.", Pref: 10}},
		addresses: map[string][]netip.Addr{
			"primary.example": {netip.MustParseAddr("8.8.4.4"), netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("8.8.4.4")},
			"backup.example":  {netip.MustParseAddr("1.1.1.1")},
		},
	}, Filter: filter{}}
	for i, want := range []string{"primary.example/8.8.4.4", "primary.example/8.8.8.8", "backup.example/1.1.1.1", "primary.example/8.8.4.4"} {
		route, err := r.ForRecipient(context.Background(), "user@example.net", "delivery", i)
		if err != nil || route.MXName+"/"+route.Address.String() != want {
			t.Fatalf("attempt %d: %+v %v", i+1, route, err)
		}
	}
	if _, err := r.ForRecipient(context.Background(), "user@example.net", "delivery", -1); err == nil {
		t.Fatal("accepted invalid attempt number")
	}
}

func TestEqualPreferenceIsStablePerDeliveryAndDistributed(t *testing.T) {
	r := &Resolver{DNS: fakeDNS{
		mx: []*net.MX{{Host: "one.example.", Pref: 10}, {Host: "two.example.", Pref: 10}},
		addresses: map[string][]netip.Addr{
			"one.example": {netip.MustParseAddr("8.8.8.8")},
			"two.example": {netip.MustParseAddr("1.1.1.1")},
		},
	}, Filter: filter{}}
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("%032x", i)
		first, err := r.ForRecipient(context.Background(), "user@example.net", id, 0)
		if err != nil {
			t.Fatal(err)
		}
		repeat, err := r.ForRecipient(context.Background(), "user@example.net", id, 0)
		if err != nil || repeat != first {
			t.Fatal("route order changed for the same delivery", err)
		}
		seen[first.MXName] = true
	}
	if len(seen) != 2 {
		t.Fatal("equal-preference mail did not distribute across hosts")
	}
}

type boundedDNS struct {
	mx           []*net.MX
	blocked      string
	blockedHosts map[string]bool
	delay        time.Duration
	mu           sync.Mutex
	active, peak int
	calls        map[string]int
	started      chan struct{}
	once         sync.Once
}

func (d *boundedDNS) LookupMX(context.Context, string) ([]*net.MX, error) { return d.mx, nil }
func (d *boundedDNS) LookupNetIP(ctx context.Context, _ string, host string) ([]netip.Addr, error) {
	d.mu.Lock()
	d.active++
	if d.active > d.peak {
		d.peak = d.active
	}
	d.calls[host]++
	d.mu.Unlock()
	defer func() { d.mu.Lock(); d.active--; d.mu.Unlock() }()
	if d.started != nil {
		d.once.Do(func() { close(d.started) })
	}
	if host == d.blocked || d.blockedHosts[host] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if d.delay > 0 {
		timer := time.NewTimer(d.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
}

func TestSlowPrimaryAddressLookupDoesNotHideBackup(t *testing.T) {
	dns := &boundedDNS{mx: []*net.MX{{Host: "primary.example.", Pref: 10}, {Host: "backup.example.", Pref: 20}}, blocked: "primary.example", calls: map[string]int{}}
	r := &Resolver{DNS: dns, Filter: filter{}, addressTimeout: 50 * time.Millisecond}
	start := time.Now()
	route, err := r.ForRecipient(context.Background(), "user@example.net", "delivery", 0)
	if err != nil || route.MXName != "backup.example" || time.Since(start) > time.Second {
		t.Fatal("backup hidden by slow primary", route, err)
	}
	dns.mu.Lock()
	defer dns.mu.Unlock()
	if dns.calls["primary.example"] != 1 || dns.calls["backup.example"] != 1 {
		t.Fatal("unexpected lookup count", dns.calls)
	}
}

func TestStalledLookupWaveDoesNotStarveLaterMX(t *testing.T) {
	dns := &boundedDNS{calls: map[string]int{}, blockedHosts: map[string]bool{}}
	for i := 0; i < maxAddressLookups; i++ {
		host := fmt.Sprintf("slow-%d.example", i)
		dns.blockedHosts[host] = true
		dns.mx = append(dns.mx, &net.MX{Host: host + ".", Pref: uint16(i + 1)})
	}
	dns.mx = append(dns.mx, &net.MX{Host: "backup.example.", Pref: 100})
	r := &Resolver{DNS: dns, Filter: filter{}, addressTimeout: 50 * time.Millisecond}
	route, err := r.ForRecipient(context.Background(), "user@example.net", "delivery", 0)
	if err != nil || route.MXName != "backup.example" {
		t.Fatal("stalled lookup wave hid backup", route, err)
	}
	dns.mu.Lock()
	defer dns.mu.Unlock()
	if dns.calls["backup.example"] != 1 || dns.peak > maxAddressLookups {
		t.Fatal("backup not queried within bounded concurrency", dns.calls, dns.peak)
	}
}

func TestAddressLookupsAreBoundedAndDeduplicated(t *testing.T) {
	dns := &boundedDNS{calls: map[string]int{}, delay: 30 * time.Millisecond}
	for i := 0; i < 16; i++ {
		host := fmt.Sprintf("mx%02d.example.", i)
		dns.mx = append(dns.mx, &net.MX{Host: host, Pref: uint16(16 - i)}, &net.MX{Host: strings.ToUpper(host), Pref: uint16(16 - i)})
	}
	firstHost := dns.mx[0].Host
	r := &Resolver{DNS: dns, Filter: filter{}}
	if _, err := r.ForRecipient(context.Background(), "user@example.net", "delivery", 0); err != nil {
		t.Fatal(err)
	}
	if dns.mx[0].Host != firstHost {
		t.Fatal("mutated shared DNS result")
	}
	dns.mu.Lock()
	defer dns.mu.Unlock()
	if dns.peak < 2 || dns.peak > maxAddressLookups || len(dns.calls) != 16 {
		t.Fatal("lookup concurrency or deduplication incorrect", dns.peak, len(dns.calls))
	}
	for host, count := range dns.calls {
		if count != 1 {
			t.Fatal("duplicate lookup", host, count)
		}
	}
}

func TestCancelledResolutionNeverReturnsAResult(t *testing.T) {
	dns := &boundedDNS{mx: []*net.MX{{Host: "primary.example.", Pref: 10}, {Host: "backup.example.", Pref: 20}}, blocked: "primary.example", calls: map[string]int{}, started: make(chan struct{})}
	r := &Resolver{DNS: dns, Filter: filter{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := r.ForRecipient(ctx, "user@example.net", "delivery", 0); done <- err }()
	<-dns.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled DNS resolution returned a route", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	r.DNS = fakeDNS{mxErr: delivery.ErrDomainNotFound}
	if _, err := r.ForRecipient(ctx, "user@example.net", "delivery", 0); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled query yielded permanent failure", err)
	}
}

func TestConcurrentResolutionsPreserveSharedDNSResults(t *testing.T) {
	dns := fakeDNS{
		mx: []*net.MX{{Host: "backup.example.", Pref: 20}, {Host: "primary.example.", Pref: 10}},
		addresses: map[string][]netip.Addr{
			"primary.example": {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("8.8.4.4")},
			"backup.example":  {netip.MustParseAddr("1.1.1.1")},
		},
	}
	r := &Resolver{DNS: dns, Filter: filter{}}
	const clients = 24
	errs := make(chan error, clients)
	for range clients {
		go func() {
			route, err := r.ForRecipient(context.Background(), "user@example.net", "delivery", 0)
			if err == nil && (route.MXName != "primary.example" || route.Address.String() != "8.8.4.4") {
				err = errors.New("unexpected route")
			}
			errs <- err
		}()
	}
	for range clients {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if dns.mx[0].Host != "backup.example." || dns.addresses["primary.example"][0].String() != "8.8.8.8" {
		t.Fatal("resolver mutated shared DNS records")
	}
}
