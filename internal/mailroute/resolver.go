// Package mailroute resolves recipient MX routes in trusted. Edge independently
// checks the resulting numeric destination before opening an SMTP connection.
package mailroute

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/mail"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/dispatch"
	"golang.org/x/net/idna"
)

type DNS interface {
	LookupMX(context.Context, string) ([]*net.MX, error)
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type Filter interface {
	AllowedFor(netip.Addr, string, string) bool
}
type Resolver struct {
	DNS    DNS
	Filter Filter
	// Tests may shorten, never lengthen, the per-host budget.
	addressTimeout time.Duration
}

const (
	maxMXRecords         = 32
	maxMXAddresses       = 64
	maxAddressLookups    = 8
	addressLookupTimeout = 5 * time.Second
)

// ForRecipient resolves a route under one total timeout. It accepts implicit MX
// fallback and rejects null MX. Each authorized rejected attempt advances a
// cursor through the current permitted routes. Equal-preference hosts have a
// stable per-delivery order. DNSSEC and MTA-STS remain separate work: a
// certificate check alone does not authenticate an MX name from insecure DNS.
func (r *Resolver) ForRecipient(parent context.Context, recipient, deliveryID string, routeAttempt int) (dispatch.Route, error) {
	if r.DNS == nil || r.Filter == nil || deliveryID == "" || routeAttempt < 0 {
		return dispatch.Route{}, errors.New("mail resolver is not configured")
	}
	address, err := mail.ParseAddress(recipient)
	if err != nil || address.Address != recipient {
		return dispatch.Route{}, errors.New("invalid recipient")
	}
	at := strings.LastIndexByte(address.Address, '@')
	if at < 1 || at == len(address.Address)-1 {
		return dispatch.Route{}, errors.New("invalid recipient domain")
	}
	domain, err := idna.Lookup.ToASCII(address.Address[at+1:])
	if err != nil {
		return dispatch.Route{}, errors.New("invalid recipient domain")
	}
	domain, ok := admin.NormalizeDomain(domain)
	if !ok {
		return dispatch.Route{}, errors.New("invalid recipient domain")
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	mx, err := r.DNS.LookupMX(ctx, domain)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return dispatch.Route{}, ctxErr
	}
	if err != nil {
		if errors.Is(err, delivery.ErrDomainNotFound) {
			return dispatch.Route{}, delivery.ErrDomainNotFound
		}
		return dispatch.Route{}, errors.New("MX lookup failed")
	}
	if len(mx) == 0 {
		mx = []*net.MX{{Host: domain, Pref: 0}}
	}
	if len(mx) > maxMXRecords {
		return dispatch.Route{}, errors.New("too many MX records")
	}
	for _, record := range mx {
		if record == nil {
			return dispatch.Route{}, errors.New("invalid MX record")
		}
		if record.Host == "." {
			if len(mx) != 1 || record.Pref != 0 {
				return dispatch.Route{}, errors.New("invalid null MX configuration")
			}
			return dispatch.Route{}, delivery.ErrNullMX
		}
	}
	// Do not mutate the resolver's slice: implementations may share results.
	mx = append([]*net.MX(nil), mx...)
	sort.SliceStable(mx, func(i, j int) bool {
		if mx[i].Pref != mx[j].Pref {
			return mx[i].Pref < mx[j].Pref
		}
		left := sha256.Sum256([]byte(deliveryID + "\x00" + strings.ToLower(mx[i].Host)))
		right := sha256.Sum256([]byte(deliveryID + "\x00" + strings.ToLower(mx[j].Host)))
		if order := bytes.Compare(left[:], right[:]); order != 0 {
			return order < 0
		}
		return strings.ToLower(mx[i].Host) < strings.ToLower(mx[j].Host)
	})
	// Normalize and deduplicate before issuing DNS work. The first occurrence
	// has the best preference, because the MX records are sorted above.
	var hosts []string
	hostSeen := make(map[string]struct{})
	for _, record := range mx {
		host, ok := admin.NormalizeDomain(strings.TrimSuffix(record.Host, "."))
		if !ok {
			continue
		}
		if _, duplicate := hostSeen[host]; duplicate {
			continue
		}
		hostSeen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	lookups := r.lookupAddresses(ctx, hosts)
	if err = ctx.Err(); err != nil {
		return dispatch.Route{}, err
	}
	var candidates []dispatch.Route
	seen := make(map[dispatch.Route]struct{})
	for index, host := range hosts {
		addresses := lookups[index]
		sort.Slice(addresses, func(i, j int) bool { return addresses[i].Unmap().Compare(addresses[j].Unmap()) < 0 })
		for _, address := range addresses {
			if r.Filter.AllowedFor(address, recipient, host) {
				candidate := dispatch.Route{MXName: host, Address: address.Unmap()}
				if _, ok := seen[candidate]; !ok {
					seen[candidate] = struct{}{}
					candidates = append(candidates, candidate)
				}
			}
		}
	}
	if len(candidates) > 0 {
		return candidates[routeAttempt%len(candidates)], nil
	}
	return dispatch.Route{}, errors.New("no permitted mail route")
}

// lookupAddresses uses a fixed worker limit and a deadline per host within the
// overall query deadline. Results retain host order regardless of completion
// order; no DNS or filter data is retained after this resolution.
func (r *Resolver) lookupAddresses(ctx context.Context, hosts []string) [][]netip.Addr {
	results := make([][]netip.Addr, len(hosts))
	jobs := make(chan int)
	var workers sync.WaitGroup
	count := min(maxAddressLookups, len(hosts))
	if count == 0 {
		return results
	}
	timeout := addressLookupTimeout
	if r.addressTimeout > 0 && r.addressTimeout < timeout {
		timeout = r.addressTimeout
	}
	// Leave enough of the overall budget for all bounded waves of hosts.
	if deadline, ok := ctx.Deadline(); ok {
		waves := (len(hosts) + count - 1) / count
		if perWave := time.Until(deadline) / time.Duration(waves); perWave < timeout {
			timeout = perWave
		}
	}
	for range count {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				lookupCtx, cancel := context.WithTimeout(ctx, timeout)
				addresses, err := r.DNS.LookupNetIP(lookupCtx, "ip", hosts[index])
				withinDeadline := lookupCtx.Err() == nil
				cancel()
				if err == nil && withinDeadline && len(addresses) <= maxMXAddresses {
					results[index] = append([]netip.Addr(nil), addresses...)
				}
			}
		}()
	}
	for i := range hosts {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return results
		}
	}
	close(jobs)
	workers.Wait()
	return results
}
