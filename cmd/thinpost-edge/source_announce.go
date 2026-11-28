package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Source announcement. The public networks are ipvlan l2: every address on a
// node shares that node's MAC, and the router learns address -> MAC by ARP.
// After the pod moves to another node the router's entry is stale until it
// re-resolves, so the worker sends one datagram from each source to that
// source's gateway. The datagram needs no privilege (an ordinary UDP socket
// bound to the source); the kernel ARPs for the gateway to send it, and the
// request carries the source address and the node's MAC, which refreshes the
// router's neighbor entry. The gateway's complete entry in this pod's
// /proc/net/arp proves the exchange worked on the interface holding the source.
const (
	announcePort          = "9"
	announcePollInterval  = 500 * time.Millisecond
	announceBudget        = 10 * time.Second
	announceBackoff       = 5 * time.Second
	announceRefresh       = 30 * time.Second
	arpFlagComplete       = 0x2
	announceProcARPPath   = "/proc/net/arp"
	announceReadyLogLabel = "source neighbors ready n=%d"
)

// arpEntry is one /proc/net/arp row.
type arpEntry struct {
	Address netip.Addr
	Flags   uint64
	Device  string
}

type sourceAnnouncer struct {
	sources, gateways []netip.Addr
	// dial sends one byte from source to gateway:9; arp lists the neighbor
	// table; ifaceOf names the interface holding a local address. All are
	// injectable so tests need no network.
	dial    func(source, gateway netip.Addr) error
	arp     func() ([]arpEntry, error)
	ifaceOf func(netip.Addr) (string, error)
	logf    func(string, ...any)
	// Timing; zero values use the production constants.
	poll, budget, backoff, refresh time.Duration

	ok      atomic.Bool
	expired atomic.Bool
}

func newSourceAnnouncer(sources, gateways []netip.Addr, logf func(string, ...any)) (*sourceAnnouncer, error) {
	if len(sources) == 0 || len(sources) != len(gateways) {
		return nil, errors.New("source gateway count must match source count")
	}
	return &sourceAnnouncer{sources: sources, gateways: gateways, dial: dialAnnounce, arp: readProcARP, ifaceOf: interfaceHolding, logf: logf}, nil
}

func (a *sourceAnnouncer) ready() bool { return a != nil && a.ok.Load() }

func orDefault(v, d time.Duration) time.Duration {
	if v <= 0 {
		return d
	}
	return v
}

// complete reports whether every gateway has a complete ARP entry on the
// interface holding its source.
func (a *sourceAnnouncer) complete(pending []bool) bool {
	entries, err := a.arp()
	if err != nil {
		return false
	}
	all := true
	for i := range a.sources {
		if !pending[i] {
			continue
		}
		dev, derr := a.ifaceOf(a.sources[i])
		found := false
		if derr == nil {
			for _, e := range entries {
				if e.Address == a.gateways[i] && e.Device == dev && e.Flags&arpFlagComplete != 0 {
					found = true
					break
				}
			}
		}
		if found {
			pending[i] = false
		} else {
			all = false
		}
	}
	return all
}

func (a *sourceAnnouncer) sendPending(pending []bool) {
	for i := range a.sources {
		if pending[i] {
			// A failed send is just a pending neighbor; the next round retries.
			_ = a.dial(a.sources[i], a.gateways[i])
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// run announces until ctx ends. It never exits on failure: until every gateway
// is resolved the worker is simply not ready, and after the budget it retries
// at a slower pace. Once ready it re-announces periodically so a router that
// ages the entry out learns it again.
func (a *sourceAnnouncer) run(ctx context.Context) {
	poll := orDefault(a.poll, announcePollInterval)
	budget := orDefault(a.budget, announceBudget)
	backoff := orDefault(a.backoff, announceBackoff)
	refresh := orDefault(a.refresh, announceRefresh)
	pending := make([]bool, len(a.sources))
	for i := range pending {
		pending[i] = true
	}
	all := make([]bool, len(a.sources))
	for i := range all {
		all[i] = true
	}
	start := time.Now()
	for ctx.Err() == nil {
		a.sendPending(pending)
		if !sleepCtx(ctx, poll) {
			return
		}
		if a.complete(pending) {
			break
		}
		if time.Since(start) >= budget {
			if !a.expired.Swap(true) && a.logf != nil {
				a.logf(announcePendingLogLabel, countTrue(pending), len(a.sources))
			}
			if !sleepCtx(ctx, backoff-poll) {
				return
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	a.ok.Store(true)
	if a.logf != nil {
		a.logf(announceReadyLogLabel, len(a.sources))
	}
	for sleepCtx(ctx, refresh) {
		a.sendPending(all)
	}
}

func dialAnnounce(source, gateway netip.Addr) error {
	d := net.Dialer{LocalAddr: &net.UDPAddr{IP: source.AsSlice()}, Timeout: time.Second}
	conn, err := d.Dial("udp4", net.JoinHostPort(gateway.String(), announcePort))
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, err = conn.Write([]byte{0})
	return err
}

func readProcARP() ([]arpEntry, error) {
	f, err := os.Open(announceProcARPPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseARP(f)
}

// parseARP reads the /proc/net/arp layout: IP, HW type, Flags, HW address,
// Mask, Device. The header row and malformed rows are skipped.
func parseARP(r io.Reader) ([]arpEntry, error) {
	var out []arpEntry
	s := bufio.NewScanner(r)
	for first := true; s.Scan(); {
		if first {
			first = false
			continue
		}
		f := strings.Fields(s.Text())
		if len(f) < 6 {
			continue
		}
		addr, err := netip.ParseAddr(f[0])
		if err != nil {
			continue
		}
		flags, ok := parseHex(f[2])
		if !ok {
			continue
		}
		out = append(out, arpEntry{Address: addr, Flags: flags, Device: f[5]})
	}
	return out, s.Err()
}

func parseHex(s string) (uint64, bool) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return 0, false
	}
	var v uint64
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint64(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint64(c-'a'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

func interfaceHolding(address netip.Addr) (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		addrs, aerr := iface.Addrs()
		if aerr != nil {
			continue
		}
		for _, item := range addrs {
			if n, ok := item.(*net.IPNet); ok {
				if a, ok := netip.AddrFromSlice(n.IP); ok && a.Unmap() == address {
					return iface.Name, nil
				}
			}
		}
	}
	return "", errors.New("source interface not found")
}

// allReady combines readiness checks; a nil check is ignored.
func allReady(checks ...func() bool) func() bool {
	return func() bool {
		for _, c := range checks {
			if c != nil && !c() {
				return false
			}
		}
		return true
	}
}

// announcePendingLogLabel is logged once when the budget expires with sources
// still unresolved; counts only, never addresses.
const announcePendingLogLabel = "source neighbors pending n=%d of %d"

func countTrue(v []bool) int {
	n := 0
	for _, b := range v {
		if b {
			n++
		}
	}
	return n
}
