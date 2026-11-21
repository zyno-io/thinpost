package admin

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type sendingIPTestStore struct {
	*fakeStore
	list        SendingIPList
	assigned    []*string
	createdApps []*string
	assignErr   error
	row         SendingIP
}

func (s *sendingIPTestStore) ListSendingIPs(context.Context) (SendingIPList, error) {
	return s.list, nil
}
func (s *sendingIPTestStore) GetSendingIP(_ context.Context, id string) (SendingIP, error) {
	if id != s.row.ID {
		return SendingIP{}, ErrNotFound
	}
	return s.row, nil
}
func (s *sendingIPTestStore) GetAppSendingIP(context.Context, string) (AppSendingIP, error) {
	return AppSendingIP{}, nil
}
func (s *sendingIPTestStore) SetAppSendingIP(_ context.Context, actor, _ string, id *string) (AppSendingIP, error) {
	s.actor = actor
	if s.assignErr != nil {
		return AppSendingIP{}, s.assignErr
	}
	s.assigned = append(s.assigned, id)
	return AppSendingIP{SendingIPID: id}, nil
}
func (s *sendingIPTestStore) CreateAppWithSendingIP(_ context.Context, actor, name string, id *string) (App, error) {
	s.actor = actor
	s.createdApps = append(s.createdApps, id)
	return App{ID: strings.Repeat("a", 32), Name: name, Active: true, CreatedAt: time.Now(), SendingIPID: id}, nil
}

// The pool is read-only: only listing and the DNS check exist, and every
// other method on it is refused before it reaches the store.
func TestSendingIPPoolRoutes(t *testing.T) {
	store := &sendingIPTestStore{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: false}, Store: store}
	if w := send(h, "GET", "/api/v1/sending-ips", "", "valid"); w.Code != 403 {
		t.Fatal("non-admin listed", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	w := send(h, "GET", "/api/v1/sending-ips", "", "valid")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ips":[]`) || !strings.Contains(w.Body.String(), `"edge_reports_identity":false`) ||
		strings.Contains(w.Body.String(), `unnamed_edge_addresses`) || strings.Contains(w.Body.String(), `global_bounce_domain`) {
		t.Fatal("empty list", w.Code, w.Body.String())
	}
	store.list.IPs = []SendingIP{{ID: strings.Repeat("c", 32), Address: "45.0.0.10", HeloName: "smtp.example.org"}}
	w = send(h, "GET", "/api/v1/sending-ips", "", "valid")
	for _, field := range []string{`"helo_name":"smtp.example.org"`, `"address":"45.0.0.10"`, `"is_primary":false`, `"on_edge":null`} {
		if !strings.Contains(w.Body.String(), field) {
			t.Fatal("missing", field, w.Body.String())
		}
	}
	for _, removed := range []string{`"name"`, `bounce_domain`, `bounce_on_edge`} {
		if strings.Contains(w.Body.String(), removed) {
			t.Fatal("removed field returned", removed)
		}
	}
	if w := send(h, "GET", "/api/v1/sending-ips?x=1", "", "valid"); w.Code != 503 {
		t.Fatal("query accepted", w.Code)
	}
	id := strings.Repeat("c", 32)
	for _, c := range [][3]string{
		{"POST", "/api/v1/sending-ips", `{"address":"45.0.0.10","helo_name":"smtp.example.org"}`},
		{"PUT", "/api/v1/sending-ips", ``},
		{"PATCH", "/api/v1/sending-ips/" + id, `{"helo_name":"smtp.example.org"}`},
		{"DELETE", "/api/v1/sending-ips/" + id, ``},
		{"GET", "/api/v1/sending-ips/" + id, ``},
	} {
		if w := send(h, c[0], c[1], c[2], "valid"); w.Code != 405 {
			t.Fatal("pool write not refused", c[0], c[1], w.Code)
		}
	}
	if w := send(h, "PATCH", "/api/v1/sending-ips/not-an-id", `{}`, "valid"); w.Code != 404 {
		t.Fatal("bad id", w.Code)
	}
	if w := send(h, "GET", "/api/v1/sending-ipsx", "", "valid"); w.Code == 200 {
		t.Fatal("prefix match leaked into another route")
	}
}

func TestAppSendingIPRoutes(t *testing.T) {
	store := &sendingIPTestStore{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store}
	app := strings.Repeat("a", 32)
	ip := strings.Repeat("c", 32)
	if w := send(h, "GET", "/api/v1/apps/"+app+"/sending-ip", "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"sending_ip_id":null`) {
		t.Fatal("get", w.Code, w.Body.String())
	}
	for _, body := range []string{``, `{}`, `{"sending_ip_id":5}`, `{"sending_ip_id":"nope"}`, `{"sending_ip_id":"` + ip + `","x":1}`, `{"id":"` + ip + `"}`} {
		if w := send(h, "PUT", "/api/v1/apps/"+app+"/sending-ip", body, "valid"); w.Code != 400 || len(store.assigned) != 0 {
			t.Fatal("invalid assignment accepted", body, w.Code)
		}
	}
	if w := send(h, "PUT", "/api/v1/apps/"+app+"/sending-ip", `{"sending_ip_id":"`+ip+`"}`, "valid"); w.Code != 200 || len(store.assigned) != 1 || *store.assigned[0] != ip {
		t.Fatal("assign", w.Code, w.Body.String())
	}
	if w := send(h, "PUT", "/api/v1/apps/"+app+"/sending-ip", `{"sending_ip_id":null}`, "valid"); w.Code != 200 || len(store.assigned) != 2 || store.assigned[1] != nil {
		t.Fatal("assign to the primary", w.Code)
	}
	// An IP the Edge is not reporting cannot be chosen, for an existing app or a new one.
	store.assignErr = ErrConflict
	if w := send(h, "PUT", "/api/v1/apps/"+app+"/sending-ip", `{"sending_ip_id":"`+ip+`"}`, "valid"); w.Code != 409 {
		t.Fatal("unavailable IP accepted", w.Code)
	}
	store.assignErr = nil
	if w := send(h, "DELETE", "/api/v1/apps/"+app+"/sending-ip", "", "valid"); w.Code != 405 {
		t.Fatal("wrong method", w.Code)
	}
	// Creating an app: with an IP it uses the optional store method, without
	// one it uses the plain create.
	if w := send(h, "POST", "/api/v1/apps", `{"name":"Hosted","sending_ip_id":"`+ip+`"}`, "valid"); w.Code != 201 || len(store.createdApps) != 1 || *store.createdApps[0] != ip || !strings.Contains(w.Body.String(), `"sending_ip_id":"`+ip+`"`) {
		t.Fatal("create with IP", w.Code, w.Body.String())
	}
	if w := send(h, "POST", "/api/v1/apps", `{"name":"Plain"}`, "valid"); w.Code != 201 || len(store.createdApps) != 1 || !strings.Contains(w.Body.String(), `"sending_ip_id":null`) {
		t.Fatal("create without IP", w.Code, w.Body.String())
	}
	if w := send(h, "POST", "/api/v1/apps", `{"name":"Bad","sending_ip_id":"x"}`, "valid"); w.Code != 400 {
		t.Fatal("create with a malformed IP id", w.Code)
	}
	// A store without the feature cannot honor an IP choice.
	plain := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}}
	if w := send(plain, "POST", "/api/v1/apps", `{"name":"Hosted","sending_ip_id":"`+ip+`"}`, "valid"); w.Code != 503 {
		t.Fatal("create with IP on a store without the feature", w.Code)
	}
	if w := send(plain, "GET", "/api/v1/sending-ips", "", "valid"); w.Code != 503 {
		t.Fatal("list on a store without the feature", w.Code)
	}
}

type fakeReverse struct {
	names     []string
	addrErr   error
	hosts     []string
	hostErr   error
	txt       map[string][]string
	txtErr    map[string]error
	mx        map[string][]*net.MX
	mxErr     error
	lookedUp  []string
	deadlined bool
}

func (f *fakeReverse) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	_, f.deadlined = ctx.Deadline()
	f.lookedUp = append(f.lookedUp, addr)
	return f.names, f.addrErr
}
func (f *fakeReverse) LookupHost(_ context.Context, host string) ([]string, error) {
	f.lookedUp = append(f.lookedUp, host)
	return f.hosts, f.hostErr
}
func (f *fakeReverse) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	f.lookedUp = append(f.lookedUp, "mx:"+name)
	return f.mx[name], f.mxErr
}
func (f *fakeReverse) LookupTXT(_ context.Context, name string) ([]string, error) {
	return f.txt[name], f.txtErr[name]
}

func TestSendingIPDNSCheck(t *testing.T) {
	ip := SendingIP{ID: strings.Repeat("c", 32), Address: "45.0.0.10", HeloName: "smtp.hosting.example.org"}
	notFound := &net.DNSError{IsNotFound: true}
	name := "smtp.hosting.example.org"
	mxSelf := map[string][]*net.MX{name: {{Host: name + ".", Pref: 10}}}
	for label, tc := range map[string]struct {
		dns                       *fakeReverse
		reverse, forward, mx, spf string
		reverseNames              int
	}{
		"all good": {&fakeReverse{names: []string{"SMTP.hosting.example.org."}, hosts: []string{"45.0.0.10"}, mx: mxSelf,
			txt: map[string][]string{name: {"v=spf1 ip4:45.0.0.0/24 -all"}}}, DNSOK, DNSOK, DNSOK, DNSOK, 1},
		"a mechanism": {&fakeReverse{names: []string{name + "."}, hosts: []string{"45.0.0.10"}, mx: mxSelf,
			txt: map[string][]string{name: {"v=spf1 a -all"}}}, DNSOK, DNSOK, DNSOK, DNSOK, 1},
		// No MX record: the name is its own exchanger, so its address decides.
		"implicit mx": {&fakeReverse{names: []string{name + "."}, hosts: []string{"45.0.0.10"}, mxErr: notFound,
			txt: map[string][]string{name: {"v=spf1 a -all"}}}, DNSOK, DNSOK, DNSOK, DNSOK, 1},
		"mismatch": {&fakeReverse{names: []string{"other.example.org."}, hosts: []string{"45.0.0.99"}, mx: mxSelf,
			txt: map[string][]string{name: {"v=spf1 ip4:45.0.0.99 -all"}}}, DNSMismatch, DNSMismatch, DNSMismatch, DNSMismatch, 1},
		"missing": {&fakeReverse{addrErr: notFound, hostErr: notFound, mxErr: notFound, txtErr: map[string]error{name: notFound}},
			DNSMissing, DNSMissing, DNSMissing, DNSMissing, 0},
		"unavailable": {&fakeReverse{addrErr: errors.New("timeout"), hostErr: errors.New("timeout"), mxErr: errors.New("timeout"), txtErr: map[string]error{name: errors.New("x")}},
			DNSUnavailable, DNSUnavailable, DNSUnavailable, DNSUnavailable, 0},
	} {
		t.Run(label, func(t *testing.T) {
			got := checkSendingIPDNS(context.Background(), tc.dns, tc.dns, ip)
			if got.Reverse != tc.reverse || got.Forward != tc.forward || got.MX != tc.mx || got.SPF != tc.spf || len(got.ReverseNames) != tc.reverseNames {
				t.Fatalf("%+v", got)
			}
			if !tc.dns.deadlined {
				t.Fatal("DNS lookups have no deadline")
			}
		})
	}
}

func TestSendingIPDNSCheckRoute(t *testing.T) {
	row := SendingIP{ID: strings.Repeat("c", 32), Address: "45.0.0.10", HeloName: "smtp.hosting.example.org"}
	store := &sendingIPTestStore{fakeStore: &fakeStore{}, row: row}
	dns := &fakeReverse{names: []string{"smtp.hosting.example.org."}, hosts: []string{"45.0.0.10"}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, DNS: dns, ReverseDNS: dns}
	path := "/api/v1/sending-ips/" + row.ID + "/check-dns"
	if w := send(h, "POST", path, "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"reverse":"ok"`) || !strings.Contains(w.Body.String(), `"forward":"ok"`) {
		t.Fatal("check", w.Code, w.Body.String())
	}
	if w := send(h, "GET", path, "", "valid"); w.Code != 405 {
		t.Fatal("wrong method", w.Code)
	}
	if w := send(h, "POST", path, `{"x":1}`, "valid"); w.Code != 400 {
		t.Fatal("body accepted", w.Code)
	}
	if w := send(h, "POST", "/api/v1/sending-ips/"+strings.Repeat("d", 32)+"/check-dns", "", "valid"); w.Code != 404 {
		t.Fatal("unknown row", w.Code)
	}
	h.ReverseDNS = nil
	if w := send(h, "POST", path, "", "valid"); w.Code != 503 {
		t.Fatal("no resolver", w.Code)
	}
}

func TestSPFListsAddress(t *testing.T) {
	address := netip.MustParseAddr("45.0.0.10")
	for record, want := range map[string]bool{
		"v=spf1 ip4:45.0.0.10 -all":                true,
		"v=spf1 +ip4:45.0.0.10 ~all":               true,
		"V=SPF1 IP4:45.0.0.10 -all":                true,
		"v=spf1 ip4:45.0.0.0/24 -all":              true,
		"v=spf1 ip4:45.0.0.11 ip4:10.0.0.0/8 -all": false,
		"v=spf1 include:_spf.example.net -all":     false,
		"v=spf1 ip4:45.0.0.10/33 -all":             false,
		"ip4:45.0.0.10":                            false,
		"v=spf1 ip6:2607:f8b0::/32 -all":           false,
		"":                                         false,
	} {
		if got := spfListsAddress(record, address); got != want {
			t.Errorf("%q: got %v", record, got)
		}
	}
}

type hostTable map[string][]string

func (h hostTable) LookupAddr(context.Context, string) ([]string, error) { return nil, nil }
func (h hostTable) LookupMX(context.Context, string) ([]*net.MX, error)  { return nil, nil }
func (h hostTable) LookupHost(_ context.Context, host string) ([]string, error) {
	if v, ok := h[host]; ok {
		return v, nil
	}
	return nil, &net.DNSError{IsNotFound: true}
}

func TestSPFAMatches(t *testing.T) {
	address := netip.MustParseAddr("45.0.0.10")
	hosts := hostTable{"bounce.example.org": {"45.0.0.10"}, "other.example.org": {"45.0.0.99"}, "mx.example.org": {"45.0.0.1"}}
	for record, want := range map[string]bool{
		"v=spf1 a -all":                                      true,
		"V=SPF1 +A ~all":                                     true,
		"v=spf1 a:other.example.org -all":                    false,
		"v=spf1 a:other.example.org/24 -all":                 true,
		"v=spf1 a/24 -all":                                   true,
		"v=spf1 a:mx.example.org a:other.example.org a -all": false, // only two lookups are made
		"v=spf1 a:missing.example.org a -all":                true,
		"v=spf1 a/33 -all":                                   false,
		"v=spf1 a:%{d} -all":                                 false,
		"v=spf1 mx -all":                                     false,
		"a":                                                  false,
	} {
		if got := spfAMatches(context.Background(), hosts, record, "bounce.example.org", address); got != want {
			t.Errorf("%q: got %v", record, got)
		}
	}
}
