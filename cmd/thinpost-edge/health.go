package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const healthAddress = "127.0.0.1:8081"

// journalHealth samples the journal independently of probes. A stale result
// expires even if a filesystem operation blocks. This is not a free-space,
// fencing, public SMTP, or controller-session check.
type journalHealth struct {
	ctx     context.Context
	validTo atomic.Pointer[time.Time]
}

func startJournalHealth(ctx context.Context, check func() error, interval, validity time.Duration) *journalHealth {
	h := &journalHealth{ctx: ctx}
	go func() {
		defer h.validTo.Store(nil)
		for ctx.Err() == nil {
			err := check()
			if err != nil || ctx.Err() != nil {
				h.validTo.Store(nil)
			} else {
				expires := time.Now().Add(validity)
				h.validTo.Store(&expires)
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
			}
		}
	}()
	return h
}

func (h *journalHealth) ready() bool {
	if h == nil || h.ctx.Err() != nil {
		return false
	}
	expires := h.validTo.Load()
	return expires != nil && time.Now().Before(*expires)
}

func healthHandler(ready func() bool) http.Handler {
	return pathHealthHandler("/readyz", ready)
}

// edgeHealthHandler serves /readyz from the full readiness (journal and source
// neighbours) and /livez from the journal alone, so a neighbour that has not
// resolved delays readiness but never restarts the process.
func edgeHealthHandler(ready, live func() bool) http.Handler {
	r, l := pathHealthHandler("/readyz", ready), pathHealthHandler("/livez", live)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/livez" {
			l.ServeHTTP(w, req)
			return
		}
		r.ServeHTTP(w, req)
	})
}

func pathHealthHandler(path string, ready func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if ready == nil || !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// probeHealth is used by a local exec probe; it never consults proxy settings.
func probeHealth(address string) error { return probeHealthPath(address, "/readyz") }

func probeHealthPath(address, path string) error {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	response, err := client.Get("http://" + address + path)
	if err != nil {
		return errors.New("edge health check failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errors.New("edge health check failed")
	}
	return nil
}
