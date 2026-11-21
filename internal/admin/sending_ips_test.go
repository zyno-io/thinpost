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
	created     []SendingIPInput
	patched     []SendingIPPatch
	deleted     []string
	assigned    []*string
	createdApps []*string
	deleteErr   error
	createErr   error
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
func (s *sendingIPTestStore) CreateSendingIP(_ context.Context, actor string, in SendingIPInput) (SendingIP, error) {
	s.actor = actor
	if s.createErr != nil {
		return SendingIP{}, s.createErr
	}
	s.created = append(s.created, in)
	return SendingIP{ID: strings.Repeat("c", 32), Name: in.Name, Address: in.Address, HeloName: in.HeloName, BounceDomain: in.BounceDomain}, nil
}
func (s *sendingIPTestStore) UpdateSendingIP(_ context.Context, actor, id string, patch SendingIPPatch) (SendingIP, error) {
	s.actor = actor
	s.patched = append(s.patched, patch)
	return SendingIP{ID: id, Name: "x"}, nil
}
func (s *sendingIPTestStore) DeleteSendingIP(_ context.Context, actor, id string) error {
	s.actor = actor
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, id)
	return nil
}
func (s *sendingIPTestStore) GetAppSendingIP(context.Context, string) (AppSendingIP, error) {
	return AppSendingIP{}, nil
}
func (s *sendingIPTestStore) SetAppSendingIP(_ context.Context, actor, _ string, id *string) (AppSendingIP, error) {
	s.actor = actor
	s.assigned = append(s.assigned, id)
	return AppSendingIP{SendingIPID: id}, nil
}
func (s *sendingIPTestStore) CreateAppWithSendingIP(_ context.Context, actor, name string, id *string) (App, error) {
	s.actor = actor
	s.createdApps = append(s.createdApps, id)
	return App{ID: strings.Repeat("a", 32), Name: name, Active: true, CreatedAt: time.Now(), SendingIPID: id}, nil
}

const validCreate = `{"name":"hosting","address":"45.0.0.10","helo_name":"smtp.hosting.example.org","bounce_domain":"hosting.example.org"}`

func TestSendingIPPoolRoutes(t *testing.T) {
	store := &sendingIPTestStore{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, BounceDomain: "bounce.example.net"}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "POST", "/api/v1/sending-ips", validCreate, "valid"); w.Code != 403 || len(store.created) != 0 {
		t.Fatal("non-admin created an IP", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}
	if w := send(h, "GET", "/api/v1/sending-ips", "", "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), `"ips":[]`) || !strings.Contains(w.Body.String(), `"unnamed_edge_addresses":[]`) || strings.Contains(w.Body.String(), `global_bounce_domain`) {
		t.Fatal("empty list", w.Code, w.Body.String())
	}
	if w := send(h, "GET", "/api/v1/sending-ips?x=1", "", "valid"); w.Code != 503 {
		t.Fatal("query accepted", w.Code)
	}
	for _, body := range []string{
		``, `{}`, `[]`,
		`{"name":"primary","address":"45.0.0.10","helo_name":"smtp.example.org","bounce_domain":"b.example.org"}`,
		`{"name":"other","address":"45.0.0.10","helo_name":"smtp.example.org","bounce_domain":"b.example.org"}`,
		`{"name":"removed","address":"45.0.0.10","helo_name":"smtp.example.org","bounce_domain":"b.example.org"}`,
		`{"name":"Hosting","address":"45.0.0.10","helo_name":"smtp.example.org","bounce_domain":"b.example.org"}`,
		`{"name":"-x","address":"45.0.0.10","helo_name":"smtp.example.org","bounce_domain":"b.example.org"}`,
		`{"name":"` + strings.Repeat("a", 33) + `","address":"45.0.0.10","helo_name":"smtp.example.org","bounce_domain":"b.example.org"}`,
		`{"name":"x","address":"10.0.0.1","helo_name":"smtp.example.org","bounce_domain":"b.example.org"}`,
		`{"name":"x","address":"127.0.0.1","helo_name":"smtp.example.org","bounce_domain":"b.example.org"}`,
		`{"name":"x","address":"2607:f8b0:4000::1","helo_name":"smtp.example.org","bounce_domain":"b.example.org"}`,
		`{"name":"x","address":"45.0.0.10/32","helo_name":"smtp.example.org","bounce_domain":"b.example.org"}`,
		`{"name":"x","address":"045.0.0.10","helo_name":"smtp.example.org","bounce_domain":"b.example.org"}`,
		`{"name":"x","address":"45.0.0.10","helo_name":"SMTP.example.org","bounce_domain":"b.example.org"}`,
		`{"name":"x","address":"45.0.0.10","helo_name":"localhost","bounce_domain":"b.example.org"}`,
		`{"name":"x","address":"45.0.0.10","helo_name":"smtp.example.org","bounce_domain":""}`,
		`{"name":"x","address":"45.0.0.10","helo_name":"smtp.example.org","bounce_domain":"45.0.0.10"}`,
		`{"name":"x","address":"45.0.0.10","helo_name":"smtp.example.org"}`,
		`{"name":"x","address":"45.0.0.10","helo_name":"smtp.example.org","bounce_domain":"b.example.org","is_default":true}`,
		// The HELO name must be the bounce domain or sit under it.
		`{"name":"x","address":"45.0.0.10","helo_name":"smtp.example.org","bounce_domain":"other.example.org"}`,
		`{"name":"x","address":"45.0.0.10","helo_name":"smtp.example.org","bounce_domain":"mtp.example.org"}`,
	} {
		if w := send(h, "POST", "/api/v1/sending-ips", body, "valid"); w.Code != 400 || len(store.created) != 0 {
			t.Fatal("invalid create accepted", body, w.Code)
		}
	}
	if w := send(h, "POST", "/api/v1/sending-ips", validCreate, "valid"); w.Code != 201 || len(store.created) != 1 || store.actor != "operator-1" {
		t.Fatal("create", w.Code, w.Body.String())
	}
	store.createErr = ErrConflict
	if w := send(h, "POST", "/api/v1/sending-ips", validCreate, "valid"); w.Code != 409 {
		t.Fatal("duplicate", w.Code)
	}
	if w := send(h, "PUT", "/api/v1/sending-ips", validCreate, "valid"); w.Code != 405 {
		t.Fatal("wrong method", w.Code)
	}
	id := strings.Repeat("c", 32)
	// The address cannot be edited; at least one editable field is required.
	for _, body := range []string{``, `{}`, `{"address":"45.0.0.11"}`, `{"name":"Bad"}`, `{"name":"primary"}`, `{"helo_name":"x"}`, `{"bounce_domain":"UPPER.example.org"}`, `{"name":"ok","extra":1}`} {
		if w := send(h, "PATCH", "/api/v1/sending-ips/"+id, body, "valid"); w.Code != 400 || len(store.patched) != 0 {
			t.Fatal("invalid patch accepted", body, w.Code)
		}
	}
	if w := send(h, "PATCH", "/api/v1/sending-ips/"+id, `{"name":"renamed","bounce_domain":"b2.example.org"}`, "valid"); w.Code != 200 || len(store.patched) != 1 || *store.patched[0].Name != "renamed" || store.patched[0].HeloName != nil {
		t.Fatal("patch", w.Code, w.Body.String())
	}
	if w := send(h, "PATCH", "/api/v1/sending-ips/not-an-id", `{"name":"x"}`, "valid"); w.Code != 404 {
		t.Fatal("bad id", w.Code)
	}
	store.deleteErr = ErrConflict
	if w := send(h, "DELETE", "/api/v1/sending-ips/"+id, "", "valid"); w.Code != 409 || len(store.deleted) != 0 {
		t.Fatal("delete of an IP in use", w.Code)
	}
	store.deleteErr = nil
	if w := send(h, "DELETE", "/api/v1/sending-ips/"+id, `{"x":1}`, "valid"); w.Code != 400 {
		t.Fatal("delete with a body", w.Code)
	}
	if w := send(h, "DELETE", "/api/v1/sending-ips/"+id, "", "valid"); w.Code != 204 || len(store.deleted) != 1 {
		t.Fatal("delete", w.Code)
	}
	if w := send(h, "GET", "/api/v1/sending-ips/"+id, "", "valid"); w.Code != 405 {
		t.Fatal("wrong method on a row", w.Code)
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
func (f *fakeReverse) LookupTXT(_ context.Context, name string) ([]string, error) {
	return f.txt[name], f.txtErr[name]
}

func TestSendingIPDNSCheck(t *testing.T) {
	ip := SendingIP{ID: strings.Repeat("c", 32), Name: "hosting", Address: "45.0.0.10", HeloName: "smtp.hosting.example.org", BounceDomain: "bounce.hosting.example.org"}
	notFound := &net.DNSError{IsNotFound: true}
	for name, tc := range map[string]struct {
		dns                                  *fakeReverse
		reverse, forward, heloSPF, bounceSPF string
		reverseNames                         int
	}{
		"all good":    {&fakeReverse{names: []string{"SMTP.hosting.example.org."}, hosts: []string{"45.0.0.10"}, txt: map[string][]string{"smtp.hosting.example.org": {"v=spf1 a -all"}, "bounce.hosting.example.org": {"v=spf1 ip4:45.0.0.0/24 -all"}}}, DNSOK, DNSOK, DNSOK, DNSOK, 1},
		"a mechanism": {&fakeReverse{names: []string{"smtp.hosting.example.org."}, hosts: []string{"45.0.0.10"}, txt: map[string][]string{"smtp.hosting.example.org": {"v=spf1 a -all"}, "bounce.hosting.example.org": {"v=spf1 a -all"}}}, DNSOK, DNSOK, DNSOK, DNSOK, 1},
		"mismatch":    {&fakeReverse{names: []string{"other.example.org."}, hosts: []string{"45.0.0.99"}, txt: map[string][]string{"smtp.hosting.example.org": {"v=spf1 a -all"}, "bounce.hosting.example.org": {"v=spf1 ip4:45.0.0.99 -all"}}}, DNSMismatch, DNSMismatch, DNSOK, DNSMismatch, 1},
		"missing":     {&fakeReverse{addrErr: notFound, hostErr: notFound, txtErr: map[string]error{"smtp.hosting.example.org": notFound, "bounce.hosting.example.org": notFound}}, DNSMissing, DNSMissing, DNSMissing, DNSMissing, 0},
		"unavailable": {&fakeReverse{addrErr: errors.New("timeout"), hostErr: errors.New("timeout"), txtErr: map[string]error{"smtp.hosting.example.org": errors.New("x"), "bounce.hosting.example.org": errors.New("x")}}, DNSUnavailable, DNSUnavailable, DNSUnavailable, DNSUnavailable, 0},
	} {
		t.Run(name, func(t *testing.T) {
			got := checkSendingIPDNS(context.Background(), tc.dns, tc.dns, ip)
			if got.Reverse != tc.reverse || got.Forward != tc.forward || got.HeloSPF != tc.heloSPF || got.BounceSPF != tc.bounceSPF || len(got.ReverseNames) != tc.reverseNames {
				t.Fatalf("%+v", got)
			}
			if !tc.dns.deadlined {
				t.Fatal("DNS lookups have no deadline")
			}
		})
	}
}

func TestSendingIPDNSCheckRoute(t *testing.T) {
	row := SendingIP{ID: strings.Repeat("c", 32), Name: "hosting", Address: "45.0.0.10", HeloName: "smtp.hosting.example.org", BounceDomain: "bounce.hosting.example.org"}
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
