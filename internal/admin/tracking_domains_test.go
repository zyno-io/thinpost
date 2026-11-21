package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/customdomain"
)

type trackingFake struct {
	*fakeStore
	domains  map[string]TrackingDomain
	listing  []TrackingDomainListing
	setErr   error
	written  bool
	finished customdomain.TrackingOutcome
	began    int
	actors   []string
}

func (s *trackingFake) ListDomains(context.Context, string) ([]Domain, error) {
	return []Domain{{Name: "example.com", Active: true}, {Name: "pending.example", Active: false}}, nil
}
func (s *trackingFake) ListTrackingDomains(context.Context, string) (map[string]TrackingDomain, error) {
	return s.domains, nil
}
func (s *trackingFake) GetTrackingDomain(_ context.Context, _, sender string) (TrackingDomain, error) {
	d, ok := s.domains[sender]
	if !ok {
		return d, ErrNotFound
	}
	return d, nil
}
func (s *trackingFake) SetTrackingDomain(_ context.Context, actor, _, sender, name string) (TrackingDomain, error) {
	s.actors = append(s.actors, actor)
	if s.setErr != nil {
		return TrackingDomain{}, s.setErr
	}
	d := TrackingDomain{Name: name, State: TrackingDomainPending, CreatedAt: time.Unix(1, 0)}
	s.domains[sender] = d
	return d, nil
}
func (s *trackingFake) DeleteTrackingDomain(_ context.Context, _, _, sender string) error {
	if _, ok := s.domains[sender]; !ok {
		return ErrNotFound
	}
	delete(s.domains, sender)
	return nil
}
func (s *trackingFake) BeginTrackingCheck(_ context.Context, appID, sender string) (customdomain.Check, error) {
	d, ok := s.domains[sender]
	if !ok {
		return customdomain.Check{}, ErrNotFound
	}
	s.began++
	return customdomain.Check{ID: strings.Repeat("1", 32), AppID: appID, Domain: d.Name, Token: strings.Repeat("2", 32)}, nil
}
func (s *trackingFake) FinishTrackingCheck(_ context.Context, actor string, c customdomain.Check, o customdomain.TrackingOutcome) (TrackingDomain, bool, error) {
	s.finished = o
	s.actors = append(s.actors, actor)
	if !s.written {
		return TrackingDomain{}, false, nil
	}
	d := TrackingDomain{Name: c.Domain, State: TrackingDomainActive, CreatedAt: time.Unix(1, 0)}
	if o.HTTPS != customdomain.HTTPSOK {
		d.State = TrackingDomainFailing
	}
	https, dns := string(o.HTTPS), string(o.DNS)
	d.HTTPSResult, d.DNSResult = &https, &dns
	return d, true, nil
}
func (s *trackingFake) ListAllTrackingDomains(context.Context) ([]TrackingDomainListing, error) {
	return s.listing, nil
}

type fakeTrackingChecker struct {
	outcome customdomain.TrackingOutcome
	asked   string
}

func (c *fakeTrackingChecker) Check(_ context.Context, domain string) customdomain.TrackingOutcome {
	c.asked = domain
	return c.outcome
}

func trackingPath(app, sender, rest string) string {
	p := "/api/v1/apps/" + app + "/domains/" + sender + "/tracking-domain"
	if rest != "" {
		p += "/" + rest
	}
	return p
}

func newTrackingHandler(custom bool) (*Handler, *trackingFake, *fakeTrackingChecker) {
	store := &trackingFake{fakeStore: &fakeStore{}, domains: map[string]TrackingDomain{}}
	checker := &fakeTrackingChecker{outcome: customdomain.TrackingOutcome{DNS: customdomain.DNSWrongTarget, HTTPS: customdomain.HTTPSOK}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, TrackingChecker: checker, AdminHost: "console.example.net",
		Runtime: &RuntimeInfo{Tracking: &RuntimeTracking{Origin: "https://links.example.net", LinkEventLimit: 10, CustomDomains: custom, CNAMETarget: "links.example.net"}}}
	return h, store, checker
}

func TestTrackingDomainRoutes(t *testing.T) {
	app := strings.Repeat("a", 32)
	h, store, checker := newTrackingHandler(true)

	t.Run("operators only", func(t *testing.T) {
		h.Authenticator = fakeAuth{admin: true}
		if w := send(h, "GET", trackingPath(app, "example.com", ""), "", ""); w.Code != 401 {
			t.Fatal("unauthenticated", w.Code)
		}
		h.Authenticator = fakeAuth{admin: false}
		for _, p := range [][2]string{{"GET", trackingPath(app, "example.com", "")}, {"PUT", trackingPath(app, "example.com", "")}, {"DELETE", trackingPath(app, "example.com", "")}, {"POST", trackingPath(app, "example.com", "check")}, {"GET", "/api/v1/tracking-domains"}} {
			if w := send(h, p[0], p[1], `{"name":"l.example.com"}`, "valid"); w.Code != 403 {
				t.Fatal("non-admin", p, w.Code)
			}
		}
		h.Authenticator = fakeAuth{admin: true}
	})

	t.Run("none yet", func(t *testing.T) {
		if w := send(h, "GET", trackingPath(app, "example.com", ""), "", "valid"); w.Code != 404 {
			t.Fatal(w.Code, w.Body.String())
		}
	})

	t.Run("PUT validates the name before the store sees it", func(t *testing.T) {
		for name, body := range map[string]string{
			"not JSON":                 `x`,
			"missing":                  `{}`,
			"not a domain":             `{"name":"not a domain"}`,
			"an address":               `{"name":"10.0.0.1"}`,
			"the sender domain itself": `{"name":"example.com"}`,
			"another domain":           `{"name":"links.example.org"}`,
			"a lookalike suffix":       `{"name":"links.not` + `example.com"}`,
			"too long":                 `{"name":"` + strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 60) + `.example.com"}`,
			"the tracking host":        `{"name":"links.example.net"}`,
			"the console host":         `{"name":"console.example.net"}`,
		} {
			sender := "example.com"
			if strings.Contains(name, "host") {
				sender = "example.net"
			}
			if w := send(h, "PUT", trackingPath(app, sender, ""), body, "valid"); w.Code != 400 {
				t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
			}
		}
		if len(store.actors) != 0 {
			t.Fatal("an invalid name reached the store")
		}
		w := send(h, "PUT", trackingPath(app, "example.com", ""), `{"name":"example.com"}`, "valid")
		if !strings.Contains(w.Body.String(), "subdomain of example.com") {
			t.Error("the message names the rule:", w.Body.String())
		}
	})

	t.Run("PUT creates", func(t *testing.T) {
		w := send(h, "PUT", trackingPath(app, "example.com", ""), `{"name":"Links.Example.com"}`, "valid")
		for _, want := range []string{`"name":"links.example.com"`, `"state":"pending"`, `"checking":false`} {
			if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
				t.Fatal("create", w.Code, want, w.Body.String())
			}
		}
		if store.actors[0] != "operator-1" {
			t.Fatal("actor", store.actors)
		}
		if w = send(h, "GET", trackingPath(app, "example.com", ""), "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"links.example.com"`) {
			t.Fatal("get", w.Code, w.Body.String())
		}
	})

	t.Run("PUT is a conflict when custom domains are unavailable", func(t *testing.T) {
		for name, runtime := range map[string]*RuntimeInfo{
			"custom domains off": {Tracking: &RuntimeTracking{CustomDomains: false}},
			"tracking off":       {},
			"no runtime":         nil,
		} {
			unavailable := *h
			unavailable.Runtime = runtime
			before := len(store.actors)
			w := send(&unavailable, "PUT", trackingPath(app, "example.com", ""), `{"name":"links2.example.com"}`, "valid")
			if w.Code != 409 || len(store.actors) != before {
				t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
			}
		}
		// Reading and removing still work, so a customer can clean up.
		unavailable := *h
		unavailable.Runtime = nil
		if w := send(&unavailable, "GET", trackingPath(app, "example.com", ""), "", "valid"); w.Code != 200 {
			t.Fatal("get", w.Code)
		}
	})

	t.Run("store errors become friendly statuses", func(t *testing.T) {
		for name, tc := range map[string]struct {
			err  error
			code int
			text string
		}{
			"paused app":     {ErrAppPaused, 409, "paused"},
			"unverified":     {ErrSenderDomainUnverified, 400, "Verify the sending domain"},
			"unknown sender": {ErrNotFound, 404, ""},
			"name in use":    {ErrConflict, 409, ""},
			"outage":         {errors.New("db"), 503, ""},
		} {
			store.setErr = tc.err
			w := send(h, "PUT", trackingPath(app, "example.com", ""), `{"name":"b2.example.com"}`, "valid")
			if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.text) {
				t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
			}
		}
		store.setErr = nil
	})

	t.Run("check", func(t *testing.T) {
		store.written = true
		w := send(h, "POST", trackingPath(app, "example.com", "check"), "", "valid")
		if w.Code != 200 || checker.asked != "links.example.com" || !strings.Contains(w.Body.String(), `"state":"active"`) || !strings.Contains(w.Body.String(), `"https_result":"ok"`) || !strings.Contains(w.Body.String(), `"dns_result":"wrong_target"`) {
			t.Fatal(w.Code, w.Body.String(), checker.asked)
		}
		if store.finished.DNS != customdomain.DNSWrongTarget {
			t.Fatal("outcome", store.finished)
		}
		checker.outcome.HTTPS = customdomain.HTTPSCertificate
		w = send(h, "POST", trackingPath(app, "example.com", "check"), "", "valid")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"failing"`) || store.finished.HTTPS != customdomain.HTTPSCertificate {
			t.Fatal(w.Code, w.Body.String())
		}
		store.written = false
		if w = send(h, "POST", trackingPath(app, "example.com", "check"), "", "valid"); w.Code != 409 {
			t.Fatal("a dropped result", w.Code)
		}
		if w = send(h, "POST", trackingPath(app, "nobody.example", "check"), "", "valid"); w.Code != 404 {
			t.Fatal("no tracking domain", w.Code)
		}
		h.TrackingChecker = nil
		if w = send(h, "POST", trackingPath(app, "example.com", "check"), "", "valid"); w.Code != 503 {
			t.Fatal("no checker on this replica", w.Code)
		}
		h.TrackingChecker = checker
	})

	t.Run("method and shape", func(t *testing.T) {
		for _, c := range [][3]string{
			{"POST", trackingPath(app, "example.com", ""), ""},
			{"PATCH", trackingPath(app, "example.com", ""), ""},
			{"GET", trackingPath(app, "example.com", "check"), ""},
			{"DELETE", trackingPath(app, "example.com", "check"), ""},
		} {
			if w := send(h, c[0], c[1], c[2], "valid"); w.Code != 405 {
				t.Errorf("%s %s: %d", c[0], c[1], w.Code)
			}
		}
		for _, p := range []string{trackingPath(app, "example.com", "") + "?x=1", trackingPath(app, "Not_A_Domain", ""), trackingPath(app, "example.com", "other")} {
			if w := send(h, "GET", p, "", "valid"); w.Code == 200 {
				t.Errorf("%s: accepted", p)
			}
		}
		if w := send(h, "DELETE", trackingPath(app, "example.com", ""), `{"x":1}`, "valid"); w.Code != 400 {
			t.Error("delete with a body", w.Code)
		}
		if w := send(h, "POST", trackingPath(app, "example.com", "check"), `{"x":1}`, "valid"); w.Code != 400 {
			t.Error("check with a body", w.Code)
		}
	})

	t.Run("DELETE", func(t *testing.T) {
		if w := send(h, "DELETE", trackingPath(app, "example.com", ""), "", "valid"); w.Code != 204 {
			t.Fatal(w.Code, w.Body.String())
		}
		if w := send(h, "DELETE", trackingPath(app, "example.com", ""), "", "valid"); w.Code != 404 {
			t.Fatal("deleting twice", w.Code)
		}
	})
}

func TestTrackingDomainsInDomainList(t *testing.T) {
	app := strings.Repeat("a", 32)
	h, store, _ := newTrackingHandler(true)
	store.domains["example.com"] = TrackingDomain{Name: "links.example.com", State: TrackingDomainActive}
	w := send(h, "GET", "/api/v1/apps/"+app+"/domains", "", "valid")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"tracking_domain":{"name":"links.example.com","state":"active"`) || strings.Count(w.Body.String(), "tracking_domain") != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	plain := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}}
	if w = send(plain, "GET", "/api/v1/apps/"+app+"/domains", "", "valid"); w.Code != 200 || strings.Contains(w.Body.String(), "tracking_domain") {
		t.Fatal("plain store", w.Code, w.Body.String())
	}
	if w = send(plain, "GET", trackingPath(app, "example.com", ""), "", "valid"); w.Code != 503 {
		t.Fatal("a store without tracking domains", w.Code)
	}
}

func TestOperatorTrackingDomainList(t *testing.T) {
	h, store, _ := newTrackingHandler(true)
	https, dns := "ok", "wrong_target"
	expires := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	removed := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	store.listing = []TrackingDomainListing{
		{AppID: strings.Repeat("a", 32), SenderDomain: "example.com", TrackingDomain: TrackingDomain{Name: "links.example.com", State: "active", HTTPSResult: &https, DNSResult: &dns, CertificateExpiresAt: &expires}},
		{AppID: strings.Repeat("b", 32), SenderDomain: "example.org", TrackingDomain: TrackingDomain{Name: "old.example.org", State: "removed", RemovedAt: &removed}},
	}
	w := send(h, "GET", "/api/v1/tracking-domains", "", "valid")
	body := w.Body.String()
	for _, want := range []string{`"tracking_domains":[`, `"app_id":"` + strings.Repeat("a", 32) + `"`, `"sender_domain":"example.com"`, `"name":"links.example.com"`, `"https_result":"ok"`, `"dns_result":"wrong_target"`, `"certificate_expires_at":"2027-01-02T03:04:05Z"`, `"state":"removed"`, `"removed_at":"2026-11-01T00:00:00Z"`} {
		if w.Code != 200 || !strings.Contains(body, want) {
			t.Fatal(w.Code, want, body)
		}
	}
	store.listing = nil
	if w = send(h, "GET", "/api/v1/tracking-domains", "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"tracking_domains":[]`) {
		t.Fatal("empty list is an array", w.Body.String())
	}
	if w = send(h, "POST", "/api/v1/tracking-domains", "", "valid"); w.Code != 405 {
		t.Fatal("method", w.Code)
	}
	if w = send(h, "GET", "/api/v1/tracking-domains?x=1", "", "valid"); w.Code == 200 {
		t.Fatal("query accepted")
	}
	plain := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}}
	if w = send(plain, "GET", "/api/v1/tracking-domains", "", "valid"); w.Code != 503 {
		t.Fatal("a store without tracking domains", w.Code)
	}
}
