package admin

import (
	"net/netip"
	"strings"
	"testing"
)

const internalHook = "http://hooks.mail.svc.cluster.local/events"

func TestParseWebhookDestinations(t *testing.T) {
	if p, err := ParseWebhookDestinations("", ""); p != nil || err != nil {
		t.Fatal("empty settings must mean no exceptions", p, err)
	}
	if p, err := ParseWebhookDestinations(" , ", " "); p != nil || err != nil {
		t.Fatal("blank list entries must be ignored", p, err)
	}
	// CIDRs alone configure no exception.
	if p, err := ParseWebhookDestinations("", "10.96.0.0/12"); err != nil || p == nil || len(p.exact) != 0 {
		t.Fatal("CIDRs alone", p, err)
	}
	if _, err := ParseWebhookDestinations(internalHook, ""); err == nil {
		t.Fatal("destinations without Service CIDRs must fail closed")
	}
	for _, cidr := range []string{
		"0.0.0.0/0", "::/0", "8.8.8.0/24", "10.0.0.0/7", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "2001:db8::/32", "2000::/3",
		"10.96.0.1/12", "10.96.0.0", "not-a-cidr", "::ffff:10.0.0.0/104", "172.0.0.0/8", "192.0.2.0/24", "100.0.0.0/8",
		"10.0.0.0/8", "10.96.0.0/11", "192.168.0.0/16,10.0.0.0/8", "fd00::/107", "fd00::/64", "fd00:ec2::/108", "100.64.0.0/10", "100.100.100.0/24", "100.100.100.200/32",
	} {
		if _, err := ParseWebhookDestinations(internalHook, cidr); err == nil {
			t.Errorf("accepted Service CIDR %q", cidr)
		}
	}
	for _, cidr := range []string{"10.96.0.0/12", "172.20.0.0/16", "192.168.0.0/20", "100.64.0.0/16", "100.101.0.0/16", "fd00:10:96::/112", "fd00::/108", "10.96.0.0/12,fd00::/108"} {
		if _, err := ParseWebhookDestinations(internalHook, cidr); err != nil {
			t.Errorf("rejected Service CIDR %q: %v", cidr, err)
		}
	}
	for _, bad := range []string{
		"https://hooks.example.com/x?a=1,b",             // would split on the comma
		"http://hooks.mail.svc.cluster.local:80/events", // non-canonical (default port)
		"HTTP://hooks.mail.svc.cluster.local/events",    // non-canonical
		"http://Hooks.mail.svc.cluster.local/events",    // non-canonical host case
		"http://user:pw@hooks.mail.svc/events",
		"http://hooks.mail.svc/events#f",
		"http://10.96.0.5/events", // IP literal
		"http://[fd00::5]/events",
		"http://hooks/events",                                                                     // single label
		"http://hooks.mail.svc/events", "http://hooks.mail.svc.cluster.local.evil.example/events", // not the exact cluster suffix
		"http://svc.cluster.local/events", "http://mail.svc.cluster.local/events", // no <service>.<namespace>
		"http://hooks.example.com/events", "https://hooks.mail.cluster.local/events",
		"ftp://hooks.mail.svc/events",
		"http://hooks.mail.svc:0/events",
		"http://hooks.mail.svc:99999/events",
		"http://hooks.mail.svc:080/events",
		"hooks.mail.svc/events",
		"http://hooks.mail.svc/ev ents",
	} {
		if _, err := ParseWebhookDestinations(bad, "10.96.0.0/12"); err == nil {
			t.Errorf("accepted destination %q", bad)
		}
	}
	if _, err := ParseWebhookDestinations(internalHook+","+internalHook+"?x=1,https://hooks.mail.svc.cluster.local:8443/e", "10.96.0.0/12"); err != nil {
		t.Fatal("rejected a multi-entry list", err)
	}
}

func TestNormalizeEndpointIsExactMatchOnly(t *testing.T) {
	p, err := ParseWebhookDestinations(internalHook+",http://hooks.mail.svc.cluster.local:3000/q?a=1", "10.96.0.0/12")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{internalHook, "HTTP://HOOKS.mail.svc.cluster.local:80/events", "http://hooks.mail.svc.cluster.local:3000/q?a=1"} {
		got, internal, ok := p.NormalizeEndpoint(raw)
		if !ok || !internal || strings.ToLower(got) != got && !strings.Contains(got, "/q?a=1") {
			t.Errorf("%q: %q %v %v", raw, got, internal, ok)
		}
	}
	if got, _, _ := p.NormalizeEndpoint("HTTP://HOOKS.mail.svc.cluster.local:80/events"); got != internalHook {
		t.Errorf("not canonical: %q", got)
	}
	for _, raw := range []string{
		"http://hooks.mail.svc.cluster.local/events/", // path prefix of an entry plus more
		"http://hooks.mail.svc.cluster.local/events/x",
		"http://hooks.mail.svc.cluster.local/events?x=1", // added query
		"http://hooks.mail.svc.cluster.local/",           // host only
		"http://hooks.mail.svc.cluster.local",
		"http://hooks.mail.svc.cluster.local:8080/events", // other port
		"http://hooks.mail.svc.cluster.local:3000/q",      // missing query
		"http://hooks.mail.svc.cluster.local:3000/q?a=2",
		"https://hooks.mail.svc.cluster.local/events", // other scheme
		"http://other.mail.svc.cluster.local/events",  // sibling host
		"http://hooks.mail.svc.cluster.local.evil.example/events",
		"http://hooks.mail.svc.cluster.local@evil.example/events",
		"http://evil.example/events",
		"http://localhost/events",
		"http://10.96.0.5/events",
		"http://hooks.mail.svc.cluster.local/events#frag",
		"http://hooks.mail.svc.cluster.local/%65vents", // an equivalent spelling is not an exact match
	} {
		if got, internal, ok := p.NormalizeEndpoint(raw); ok || internal || got != "" {
			t.Errorf("accepted non-matching %q as %q", raw, got)
		}
	}
	// Public endpoints keep working and are never marked internal.
	if got, internal, ok := p.NormalizeEndpoint("https://Hooks.Example.com:443/in"); !ok || internal || got != "https://hooks.example.com/in" {
		t.Errorf("public endpoint: %q %v %v", got, internal, ok)
	}
}

func TestNilAndEmptyDestinationsKeepPublicRules(t *testing.T) {
	empty, _ := ParseWebhookDestinations("", "10.96.0.0/12")
	for name, p := range map[string]*WebhookDestinations{"nil": nil, "cidrs only": empty} {
		if _, _, ok := p.NormalizeEndpoint(internalHook); ok {
			t.Errorf("%s: accepted an internal URL", name)
		}
		if got, internal, ok := p.NormalizeEndpoint("https://hooks.example.com/in"); !ok || internal || got != "https://hooks.example.com/in" {
			t.Errorf("%s: public endpoint changed", name)
		}
		for _, raw := range []string{"http://hooks.example.com/in", "https://hooks.example.com:8443/in", "https://x.svc/in", "https://10.0.0.1/in"} {
			if _, _, ok := p.NormalizeEndpoint(raw); ok {
				t.Errorf("%s: accepted %q", name, raw)
			}
		}
	}
	if (*WebhookDestinations)(nil).AllowAddress(netip.MustParseAddr("10.96.0.1")) {
		t.Error("nil policy allowed an address")
	}
}

func TestAllowAddressRequiresServiceCIDR(t *testing.T) {
	p, err := ParseWebhookDestinations(internalHook, "10.96.0.0/12,fd00:10:96::/112")
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]bool{
		"10.96.0.10": true, "10.111.255.255": true, "::ffff:10.96.0.10": true, "fd00:10:96::5": true,
		"10.112.0.0": false, "10.0.0.1": false, "127.0.0.1": false, "8.8.8.8": false, "169.254.169.254": false,
		"2606:4700::1111": false, "fd00:10:97::5": false, "::1": false, "fd00:10:96::5%eth0": false,
	} {
		a, err := netip.ParseAddr(addr)
		if err != nil {
			t.Fatal(addr, err)
		}
		if p.AllowAddress(a) != want {
			t.Errorf("AllowAddress(%s) = %v", addr, !want)
		}
	}
	if p.AllowAddress(netip.Addr{}) {
		t.Error("allowed the zero address")
	}
}

func TestWebhookRoutesAcceptInternalURLOnlyWhenListed(t *testing.T) {
	app, id := strings.Repeat("a", 32), strings.Repeat("b", 32)
	f := &fakeWebhooks{hook: Webhook{ID: id, Endpoint: internalHook, Active: true}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, Webhooks: f}
	base := "/api/v1/apps/" + app + "/webhooks"
	create := `{"endpoint":"` + internalHook + `"}`
	if w := send(h, "POST", base, create, "valid"); w.Code != 400 || f.calls != 0 {
		t.Fatal("default configuration accepted an internal URL", w.Code)
	}
	if w := send(h, "PATCH", base+"/"+id, `{"endpoint":"`+internalHook+`"}`, "valid"); w.Code != 400 || f.calls != 0 {
		t.Fatal("default configuration accepted an internal URL on update", w.Code)
	}
	var err error
	if h.WebhookDestinations, err = ParseWebhookDestinations(internalHook, "10.96.0.0/12"); err != nil {
		t.Fatal(err)
	}
	if w := send(h, "POST", base, `{"endpoint":"HTTP://Hooks.mail.svc.cluster.local:80/events","events":["accepted"]}`, "valid"); w.Code != 201 || f.input.Endpoint != internalHook {
		t.Fatal("listed URL rejected or not canonical", w.Code, f.input.Endpoint)
	}
	if w := send(h, "PATCH", base+"/"+id, `{"endpoint":"`+internalHook+`"}`, "valid"); w.Code != 200 || f.patch.Endpoint == nil || *f.patch.Endpoint != internalHook {
		t.Fatal("listed URL rejected on update", w.Code)
	}
	f.calls = 0
	for _, raw := range []string{internalHook + "/x", internalHook + "?x=1", "http://hooks.mail.svc.cluster.local/", "http://other.mail.svc.cluster.local/events", "http://hooks.mail.svc.cluster.local:81/events"} {
		if w := send(h, "POST", base, `{"endpoint":"`+raw+`"}`, "valid"); w.Code != 400 {
			t.Fatal("unlisted URL accepted", raw, w.Code)
		}
		if w := send(h, "PATCH", base+"/"+id, `{"endpoint":"`+raw+`"}`, "valid"); w.Code != 400 {
			t.Fatal("unlisted URL accepted on update", raw, w.Code)
		}
	}
	if f.calls != 0 {
		t.Fatal("an unlisted URL reached the store")
	}
	if w := send(h, "POST", base, `{"endpoint":"https://hooks.example.com/in"}`, "valid"); w.Code != 201 {
		t.Fatal("public endpoint stopped working", w.Code)
	}
}

func TestInternalEndpointPermitted(t *testing.T) {
	for _, tc := range []struct {
		format string
		auth   bool
		want   bool
	}{
		{"thinpost", false, true}, {"thinpost", true, false}, {"resend", false, false}, {"postmark", false, false},
		{"mailgun", false, false}, {"thinpost_inbound", false, false}, {"", false, false},
	} {
		if InternalEndpointPermitted(tc.format, tc.auth) != tc.want {
			t.Errorf("%+v", tc)
		}
	}
}

func TestInternalURLRequiresNativeFormatAndNoAuth(t *testing.T) {
	app, id := strings.Repeat("a", 32), strings.Repeat("b", 32)
	f := &fakeWebhooks{hook: Webhook{ID: id, Endpoint: internalHook, Active: true}}
	policy, err := ParseWebhookDestinations(internalHook, "10.96.0.0/12")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, Webhooks: f, WebhookDestinations: policy}
	base := "/api/v1/apps/" + app + "/webhooks"
	ep := `"endpoint":"` + internalHook + `"`
	for name, body := range map[string]string{
		"resend":          `{` + ep + `,"format":"resend"}`,
		"postmark":        `{` + ep + `,"format":"postmark"}`,
		"mailgun":         `{` + ep + `,"format":"mailgun"}`,
		"inbound":         `{` + ep + `,"format":"thinpost_inbound"}`,
		"basic auth":      `{` + ep + `,"auth":{"username":"u","password":"p"}}`,
		"header auth":     `{` + ep + `,"auth":{"headers":[{"name":"X-Token","value":"v"}]}}`,
		"native + header": `{` + ep + `,"format":"thinpost","auth":{"headers":[{"name":"X-Token","value":"v"}]}}`,
	} {
		if w := send(h, "POST", base, body, "valid"); w.Code != 400 || f.calls != 0 {
			t.Errorf("%s: create accepted (%d)", name, w.Code)
		}
	}
	for name, body := range map[string]string{"native": `{` + ep + `}`, "explicit native": `{` + ep + `,"format":"thinpost"}`} {
		if w := send(h, "POST", base, body, "valid"); w.Code != 201 {
			t.Errorf("%s: rejected (%d)", name, w.Code)
		}
	}
	// A public endpoint keeps its formats and credentials.
	pub := `{"endpoint":"https://hooks.example.com/in","format":"resend","auth":{"username":"u","password":"p"}}`
	if w := send(h, "POST", base, pub, "valid"); w.Code != 201 {
		t.Errorf("public endpoint with auth rejected (%d)", w.Code)
	}
	f.calls = 0
	if w := send(h, "PATCH", base+"/"+id, `{`+ep+`,"auth":{"username":"u","password":"p"}}`, "valid"); w.Code != 400 || f.calls != 0 {
		t.Errorf("update to internal with credentials accepted (%d)", w.Code)
	}
	if w := send(h, "PATCH", base+"/"+id, `{`+ep+`}`, "valid"); w.Code != 200 {
		t.Errorf("update to internal rejected (%d)", w.Code)
	}
}
