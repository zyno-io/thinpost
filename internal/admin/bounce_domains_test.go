package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/customdomain"
)

type bounceFake struct {
	*fakeStore
	domains   map[string]BounceDomain
	target    customdomain.Target
	setErr    error
	written   bool
	finished  customdomain.Result
	finishedO string
	began     int
	actors    []string
}

func (s *bounceFake) ListDomains(context.Context, string) ([]Domain, error) {
	return []Domain{{Name: "example.com", Active: true}, {Name: "pending.example", Active: false}}, nil
}
func (s *bounceFake) ListBounceDomains(context.Context, string) (map[string]BounceDomain, error) {
	return s.domains, nil
}
func (s *bounceFake) GetBounceDomain(_ context.Context, _, sender string) (BounceDomain, error) {
	d, ok := s.domains[sender]
	if !ok {
		return d, ErrNotFound
	}
	return d, nil
}
func (s *bounceFake) SetBounceDomain(_ context.Context, actor, _, sender, name string) (BounceDomain, error) {
	s.actors = append(s.actors, actor)
	if s.setErr != nil {
		return BounceDomain{}, s.setErr
	}
	d := BounceDomain{Name: name, State: BounceDomainPending, CreatedAt: time.Unix(1, 0)}
	s.domains[sender] = d
	return d, nil
}
func (s *bounceFake) DeleteBounceDomain(_ context.Context, _, _, sender string) error {
	if _, ok := s.domains[sender]; !ok {
		return ErrNotFound
	}
	delete(s.domains, sender)
	return nil
}
func (s *bounceFake) BounceTarget(context.Context, string) (customdomain.Target, error) {
	return s.target, nil
}
func (s *bounceFake) BeginBounceCheck(_ context.Context, appID, sender string) (customdomain.Check, error) {
	d, ok := s.domains[sender]
	if !ok {
		return customdomain.Check{}, ErrNotFound
	}
	s.began++
	return customdomain.Check{ID: strings.Repeat("1", 32), AppID: appID, Domain: d.Name, Token: strings.Repeat("2", 32), Target: s.target}, nil
}
func (s *bounceFake) FinishBounceCheck(_ context.Context, actor string, c customdomain.Check, result customdomain.Result, observed string) (BounceDomain, bool, error) {
	s.finished, s.finishedO = result, observed
	s.actors = append(s.actors, actor)
	if !s.written {
		return BounceDomain{}, false, nil
	}
	d := BounceDomain{Name: c.Domain, State: BounceDomainActive, CreatedAt: time.Unix(1, 0)}
	if result != customdomain.ResultOK {
		d.State = BounceDomainFailing
	}
	r := string(result)
	d.Result = &r
	return d, true, nil
}

type fakeCNAME struct {
	answer string
	err    error
	asked  string
}

func (c *fakeCNAME) LookupCNAME(_ context.Context, host string) (string, error) {
	c.asked = host
	return c.answer, c.err
}

func bouncePath(app, sender, rest string) string {
	p := "/api/v1/apps/" + app + "/domains/" + sender + "/bounce-domain"
	if rest != "" {
		p += "/" + rest
	}
	return p
}

func newBounceHandler() (*Handler, *bounceFake, *fakeCNAME) {
	store := &bounceFake{fakeStore: &fakeStore{identity: EdgeIdentity{MainHostname: "smtp.example.net", MainAddress: "45.0.0.10", Hostnames: []string{"smtp.example.net", "smtp2.example.net"}}},
		domains: map[string]BounceDomain{}, target: customdomain.Target{Name: "smtp.example.net", Address: "45.0.0.10", Reported: []string{"smtp.example.net"}}}
	cname := &fakeCNAME{answer: "smtp.example.net."}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, CNAME: cname,
		ReverseDNS: &fakeReverse{names: []string{"smtp.example.net."}, hosts: []string{"45.0.0.10"}}}
	return h, store, cname
}

func TestBounceDomainRoutes(t *testing.T) {
	app := strings.Repeat("a", 32)
	h, store, cname := newBounceHandler()

	t.Run("operators only", func(t *testing.T) {
		h.Authenticator = fakeAuth{admin: true}
		if w := send(h, "GET", bouncePath(app, "example.com", ""), "", ""); w.Code != 401 {
			t.Fatal("unauthenticated", w.Code)
		}
		h.Authenticator = fakeAuth{admin: false}
		for _, m := range []string{"GET", "PUT", "DELETE"} {
			if w := send(h, m, bouncePath(app, "example.com", ""), `{"name":"b.example.com"}`, "valid"); w.Code != 403 {
				t.Fatal("non-admin", m, w.Code)
			}
		}
		h.Authenticator = fakeAuth{admin: true}
	})

	t.Run("none yet", func(t *testing.T) {
		if w := send(h, "GET", bouncePath(app, "example.com", ""), "", "valid"); w.Code != 404 {
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
			"another domain":           `{"name":"bounces.example.org"}`,
			"a lookalike suffix":       `{"name":"bounces.not` + `example.com"}`,
			"too long":                 `{"name":"` + strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 40) + `.example.com"}`,
			"a HELO name of ours":      `{"name":"smtp2.example.net"}`,
		} {
			if w := send(h, "PUT", bouncePath(app, "example.com", ""), body, "valid"); w.Code != 400 {
				t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
			}
		}
		if len(store.actors) != 0 {
			t.Fatal("an invalid name reached the store")
		}
		w := send(h, "PUT", bouncePath(app, "example.com", ""), `{"name":"example.com"}`, "valid")
		if !strings.Contains(w.Body.String(), "subdomain of example.com") {
			t.Error("the message names the rule:", w.Body.String())
		}
	})

	t.Run("PUT creates, with the name the CNAME must use", func(t *testing.T) {
		w := send(h, "PUT", bouncePath(app, "example.com", ""), `{"name":"Bounces.Example.com"}`, "valid")
		for _, want := range []string{`"name":"bounces.example.com"`, `"state":"pending"`, `"checking":false`, `"cname_target":"smtp.example.net"`, `"cname_target_status":"ok"`} {
			if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
				t.Fatal("create", w.Code, want, w.Body.String())
			}
		}
		if store.actors[0] != "operator-1" {
			t.Fatal("actor", store.actors)
		}
		if w = send(h, "GET", bouncePath(app, "example.com", ""), "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"bounces.example.com"`) {
			t.Fatal("get", w.Code, w.Body.String())
		}
	})

	t.Run("store errors become friendly statuses", func(t *testing.T) {
		for name, tc := range map[string]struct {
			err  error
			code int
			text string
		}{
			"receiving domain": {ErrInboundDomainClash, 409, "already receives mail"},
			"paused app":       {ErrAppPaused, 409, "paused"},
			"unverified":       {ErrSenderDomainUnverified, 400, "Verify the sending domain"},
			"unknown sender":   {ErrNotFound, 404, ""},
			"name in use":      {ErrConflict, 409, ""},
			"outage":           {errors.New("db"), 503, ""},
		} {
			store.setErr = tc.err
			w := send(h, "PUT", bouncePath(app, "example.com", ""), `{"name":"b2.example.com"}`, "valid")
			if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.text) {
				t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
			}
		}
		store.setErr = nil
	})

	t.Run("the CNAME target is withheld until DNS confirms the name", func(t *testing.T) {
		h.ReverseDNS = &fakeReverse{names: []string{"other.example."}, hosts: []string{"45.0.0.10"}}
		w := send(h, "GET", bouncePath(app, "example.com", ""), "", "valid")
		if strings.Contains(w.Body.String(), `"cname_target":`) || !strings.Contains(w.Body.String(), `"cname_target_status":"dns_unconfirmed"`) {
			t.Fatal(w.Body.String())
		}
		store.target = customdomain.Target{}
		w = send(h, "GET", bouncePath(app, "example.com", ""), "", "valid")
		if !strings.Contains(w.Body.String(), `"cname_target_status":"not_reported"`) {
			t.Fatal(w.Body.String())
		}
		store.target = customdomain.Target{Name: "smtp.example.net", Address: "45.0.0.10"}
		h.ReverseDNS = &fakeReverse{names: []string{"smtp.example.net."}, hosts: []string{"45.0.0.10"}}
	})

	t.Run("check", func(t *testing.T) {
		store.written = true
		w := send(h, "POST", bouncePath(app, "example.com", "check"), "", "valid")
		if w.Code != 200 || store.finished != customdomain.ResultOK || cname.asked != "bounces.example.com." || !strings.Contains(w.Body.String(), `"state":"active"`) || !strings.Contains(w.Body.String(), `"result":"ok"`) {
			t.Fatal(w.Code, w.Body.String(), store.finished, cname.asked)
		}
		cname.answer = "cdn.example.org."
		w = send(h, "POST", bouncePath(app, "example.com", "check"), "", "valid")
		if w.Code != 200 || store.finished != customdomain.ResultWrongTarget || store.finishedO != "cdn.example.org" || !strings.Contains(w.Body.String(), `"state":"failing"`) {
			t.Fatal(w.Code, w.Body.String(), store.finished)
		}
		// A newer check (or an IP change) took the row while this one ran.
		store.written = false
		if w = send(h, "POST", bouncePath(app, "example.com", "check"), "", "valid"); w.Code != 409 {
			t.Fatal("a dropped result", w.Code)
		}
		if w = send(h, "POST", bouncePath(app, "nobody.example", "check"), "", "valid"); w.Code != 404 {
			t.Fatal("no bounce domain", w.Code)
		}
		h.CNAME = nil
		if w = send(h, "POST", bouncePath(app, "example.com", "check"), "", "valid"); w.Code != 503 {
			t.Fatal("no resolver", w.Code)
		}
		h.CNAME = cname
	})

	t.Run("method and shape", func(t *testing.T) {
		for _, c := range [][3]string{
			{"POST", bouncePath(app, "example.com", ""), ""},
			{"PATCH", bouncePath(app, "example.com", ""), ""},
			{"GET", bouncePath(app, "example.com", "check"), ""},
			{"DELETE", bouncePath(app, "example.com", "check"), ""},
		} {
			if w := send(h, c[0], c[1], c[2], "valid"); w.Code != 405 {
				t.Errorf("%s %s: %d", c[0], c[1], w.Code)
			}
		}
		for _, p := range []string{bouncePath(app, "example.com", "") + "?x=1", bouncePath(app, "Not_A_Domain", ""), bouncePath(app, "example.com", "other")} {
			if w := send(h, "GET", p, "", "valid"); w.Code == 200 {
				t.Errorf("%s: accepted", p)
			}
		}
		if w := send(h, "DELETE", bouncePath(app, "example.com", ""), `{"x":1}`, "valid"); w.Code != 400 {
			t.Error("delete with a body", w.Code)
		}
	})

	t.Run("DELETE", func(t *testing.T) {
		if w := send(h, "DELETE", bouncePath(app, "example.com", ""), "", "valid"); w.Code != 204 {
			t.Fatal(w.Code, w.Body.String())
		}
		if w := send(h, "DELETE", bouncePath(app, "example.com", ""), "", "valid"); w.Code != 404 {
			t.Fatal("deleting twice", w.Code)
		}
	})
}

func TestBounceDomainsInDomainList(t *testing.T) {
	app := strings.Repeat("a", 32)
	h, store, _ := newBounceHandler()
	r := "bounces.example.com"
	store.domains["example.com"] = BounceDomain{Name: r, State: BounceDomainActive}
	w := send(h, "GET", "/api/v1/apps/"+app+"/domains", "", "valid")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"bounce_domain":{"name":"bounces.example.com","state":"active"`) {
		t.Fatal(w.Code, body)
	}
	// Only verified domains carry the CNAME target.
	if strings.Count(body, `"cname_target":"smtp.example.net"`) != 1 || strings.Count(body, `"cname_target_status":"ok"`) != 1 {
		t.Fatal("target on the wrong domains", body)
	}
	// A store without bounce domains lists as before.
	plain := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}}
	if w = send(plain, "GET", "/api/v1/apps/"+app+"/domains", "", "valid"); w.Code != 200 || strings.Contains(w.Body.String(), "bounce_domain") {
		t.Fatal("plain store", w.Code, w.Body.String())
	}
	if w = send(plain, "GET", bouncePath(app, "example.com", ""), "", "valid"); w.Code != 503 {
		t.Fatal("a store without bounce domains", w.Code)
	}
}

// A receiving domain cannot take a name an app uses for bounces.
func TestInboundRefusesBounceDomain(t *testing.T) {
	app := strings.Repeat("a", 32)
	store := &clashInbound{inboundFake: &inboundFake{fakeStore: &fakeStore{}, domains: map[string]InboundDomain{"x.example.com": {Name: "x.example.com", State: InboundDomainPending}}}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, DNS: fakeDNS{records: []string{"thinpost-inbound-verification=test"}}, MX: fakeMX{}}
	w := send(h, "POST", inboundPath(app, "inbound-domains"), `{"domain":"x.example.com"}`, "valid")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "bounce domain") {
		t.Fatal("request", w.Code, w.Body.String())
	}
	w = send(h, "POST", inboundPath(app, "inbound-domains/x.example.com/verify"), "", "valid")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "bounce domain") {
		t.Fatal("verify", w.Code, w.Body.String())
	}
}

type clashInbound struct{ *inboundFake }

func (s *clashInbound) RequestInboundDomain(context.Context, string, string, string) (InboundDomainChallenge, error) {
	return InboundDomainChallenge{}, ErrBounceDomainClash
}
func (s *clashInbound) VerifyInboundDomain(context.Context, string, string, string, [32]byte) (InboundDomain, error) {
	return InboundDomain{}, ErrBounceDomainClash
}
