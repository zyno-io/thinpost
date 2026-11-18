package webhook

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

const (
	internalURL  = "http://hooks.mail.svc.cluster.local:3000/mail/events"
	serviceCIDRs = "10.96.0.0/12"
)

// dialLog records the vetted addresses the sender asked to connect to.
type dialLog struct {
	mu    sync.Mutex
	addrs []string
}

func (d *dialLog) get() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// internalSender returns a sender whose vetted connections are redirected to a
// plain HTTP test server, so the address policy runs unmodified.
func internalSender(t *testing.T, destinations string, answers []netip.Addr, handler http.Handler) (*Sender, *dialLog, *fixedResolver) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	policy, err := admin.ParseWebhookDestinations(destinations, serviceCIDRs)
	if err != nil {
		t.Fatal(err)
	}
	log := &dialLog{}
	resolver := &fixedResolver{addresses: answers}
	return &Sender{Destinations: policy, Resolver: resolver, connect: func(ctx context.Context, network, address string) (net.Conn, error) {
		log.mu.Lock()
		log.addrs = append(log.addrs, address)
		log.mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}}, log, resolver
}

func TestInternalDestinationDeliversSignedEvent(t *testing.T) {
	now := time.Now()
	var seen atomic.Bool
	sender, log, resolver := internalSender(t, internalURL, []netip.Addr{netip.MustParseAddr("10.96.12.34")}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		id, stamp, sig := r.Header.Get("X-Thinpost-Event-ID"), r.Header.Get("X-Thinpost-Timestamp"), r.Header.Get("X-Thinpost-Signature")
		if !format.VerifyThinpost(testSecret, id, stamp, body, sig, now, 5*time.Minute) {
			t.Error("signature does not verify")
		}
		if r.Host != "hooks.mail.svc.cluster.local:3000" || r.URL.Path != "/mail/events" || r.TLS != nil {
			t.Errorf("unexpected request %q %q", r.Host, r.URL.Path)
		}
		seen.Store(true)
		w.WriteHeader(204)
	}))
	result, err := sender.Send(context.Background(), internalURL, nativeRequest(t, "event-1", now), nil)
	if err != nil || !result.Success || result.StatusCode != 204 || !seen.Load() {
		t.Fatalf("result %+v %v", result, err)
	}
	if got := log.get(); len(got) != 1 || got[0] != "10.96.12.34:3000" {
		t.Fatalf("dialed %v", got)
	}
	if resolver.name != "hooks.mail.svc.cluster.local." {
		t.Fatalf("lookup was not rooted: %q", resolver.name)
	}
}

func TestInternalDestinationRequiresExactMatch(t *testing.T) {
	var calls atomic.Int32
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	sender, log, resolver := internalSender(t, internalURL, []netip.Addr{netip.MustParseAddr("10.96.12.34")}, handler)
	for _, endpoint := range []string{
		internalURL + "/x", internalURL + "?x=1", "http://hooks.mail.svc.cluster.local:3000/", "http://hooks.mail.svc.cluster.local:3000",
		"http://hooks.mail.svc.cluster.local/mail/events", "http://hooks.mail.svc.cluster.local:3001/mail/events",
		"http://other.mail.svc.cluster.local:3000/mail/events", "https://hooks.mail.svc.cluster.local:3000/mail/events",
	} {
		if _, err := sender.Send(context.Background(), endpoint, request(`{}`), nil); err == nil {
			t.Errorf("sent to unlisted %q", endpoint)
		}
	}
	if len(log.get()) != 0 || resolver.name != "" || calls.Load() != 0 {
		t.Fatal("an unlisted URL was resolved or dialed")
	}
}

func TestInternalDestinationRejectsRebindingOutsideServiceCIDR(t *testing.T) {
	var calls atomic.Int32
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	for name, answers := range map[string][]string{
		"public":          {"8.8.8.8"},
		"public v6":       {"2606:4700::1111"},
		"loopback":        {"127.0.0.1"},
		"metadata":        {"169.254.169.254"},
		"other private":   {"10.0.0.5"},
		"just past range": {"10.112.0.1"},
		"mapped public":   {"::ffff:8.8.8.8"},
		"none":            {},
	} {
		var addrs []netip.Addr
		for _, a := range answers {
			addrs = append(addrs, netip.MustParseAddr(a))
		}
		sender, log, _ := internalSender(t, internalURL, addrs, handler)
		result, err := sender.Send(context.Background(), internalURL, request(`{}`), nil)
		if err != nil || result.Success || len(log.get()) != 0 || calls.Load() != 0 {
			t.Fatalf("%s: %+v %v %v", name, result, err, log.get())
		}
		if result.Failure != FailureBlocked {
			t.Fatalf("%s: failure %q", name, result.Failure)
		}
	}
	// A mixed answer set connects only to the address inside the Service CIDR.
	sender, log, _ := internalSender(t, internalURL, []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.96.0.7")}, handler)
	if result, err := sender.Send(context.Background(), internalURL, request(`{}`), nil); err != nil || !result.Success {
		t.Fatalf("mixed answers: %+v %v", result, err)
	}
	if got := log.get(); len(got) != 1 || got[0] != "10.96.0.7:3000" {
		t.Fatalf("dialed %v", got)
	}
}

func TestInternalDestinationDoesNotFollowRedirects(t *testing.T) {
	var other atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { other.Add(1) }))
	defer target.Close()
	for _, location := range []string{target.URL + "/steal", "http://hooks.mail.svc.cluster.local:3000/other", "https://example.com/x"} {
		sender, log, _ := internalSender(t, internalURL, []netip.Addr{netip.MustParseAddr("10.96.12.34")}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, location, http.StatusTemporaryRedirect)
		}))
		result, err := sender.Send(context.Background(), internalURL, request(`{}`), nil)
		if err != nil || result.Success || result.StatusCode != 307 || len(log.get()) != 1 || other.Load() != 0 {
			t.Fatalf("redirect to %q: %+v %v %v", location, result, err, log.get())
		}
	}
}

func TestListedInternalURLDoesNotWidenOtherDestinations(t *testing.T) {
	// With an exception configured, a public endpoint still needs a public
	// address, and a Service CIDR address does not make it deliverable.
	sender, log, _ := internalSender(t, internalURL, []netip.Addr{netip.MustParseAddr("10.96.12.34")}, http.NotFoundHandler())
	result, err := sender.Send(context.Background(), "https://hooks.example.com/in", request(`{}`), nil)
	if err != nil || result.Failure != FailureBlocked || len(log.get()) != 0 {
		t.Fatalf("public endpoint to a Service address: %+v %v %v", result, err, log.get())
	}
	// Port 443 stays the only port for a public endpoint.
	if _, err := sender.Send(context.Background(), "https://hooks.example.com:8443/in", request(`{}`), nil); err == nil {
		t.Fatal("accepted a public endpoint on another port")
	}
}

func TestEmptyDestinationsLeavePublicBehaviorUnchanged(t *testing.T) {
	resolver := &fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("10.96.12.34")}}
	nilPolicy := &Sender{Resolver: resolver}
	if _, err := nilPolicy.Send(context.Background(), internalURL, request(`{}`), nil); err == nil || resolver.name != "" {
		t.Fatal("internal URL was accepted without a policy")
	}
	// Public path: a private answer is blocked, exactly as before.
	if result, err := nilPolicy.Send(context.Background(), "https://hooks.example.com/in", request(`{}`), nil); err != nil || result.Failure != FailureBlocked {
		t.Fatalf("%+v %v", result, err)
	}
	// CIDRs without destinations are not an exception either.
	policy, err := admin.ParseWebhookDestinations("", serviceCIDRs)
	if err != nil {
		t.Fatal(err)
	}
	sender := &Sender{Destinations: policy, Resolver: &fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("10.96.12.34")}}}
	if _, err := sender.Send(context.Background(), internalURL, request(`{}`), nil); err == nil {
		t.Fatal("CIDRs alone enabled an internal destination")
	}
	if result, err := sender.Send(context.Background(), "https://hooks.example.com/in", request(`{}`), nil); err != nil || result.Failure != FailureBlocked {
		t.Fatalf("public endpoint to a Service address: %+v %v", result, err)
	}
}

// internalFake is a Deliverer that classifies one endpoint as internal.
type internalFake struct {
	fakeDelivery
	internal string
	sends    int
}

func (f *internalFake) IsInternal(endpoint string) bool { return endpoint == f.internal }
func (f *internalFake) Send(ctx context.Context, endpoint string, r format.Request, auth *admin.WebhookAuth) (Result, error) {
	f.sends++
	return f.fakeDelivery.Send(ctx, endpoint, r, auth)
}

func sealedFor(name string) []byte {
	if format.Unsigned(name) {
		return nil
	}
	return []byte("sealed")
}

func TestDispatcherFailsClosedForInternalEndpointsOutsideNativeNoAuth(t *testing.T) {
	for _, tc := range []struct {
		name, format string
		auth         []byte
		sends        bool
	}{
		{"native", format.Thinpost, nil, true},
		{"native with auth", format.Thinpost, []byte("sealed-auth"), false},
		{"resend", format.Resend, nil, false},
		{"postmark", format.Postmark, nil, false},
		{"mailgun", format.Mailgun, nil, false},
		{"inbound", format.ThinpostInbound, nil, false},
	} {
		repo := &memoryRepo{claim: Claim{AppID: "app", SubscriptionID: "hook", Endpoint: internalURL, Format: tc.format, SealedAuth: tc.auth, SealedSecret: sealedFor(tc.format), Event: Event{ID: "event-1", MessageID: "message-1", DeliveryID: "delivery-1", AttemptID: "attempt-1", Outcome: "accepted", SMTPCode: 250, OccurredAt: time.Now()}}}
		sender := &internalFake{fakeDelivery: fakeDelivery{status: 204}, internal: internalURL}
		if err := (&Dispatcher{Repository: repo, Sender: sender, Keys: &fakeKeys{}}).DispatchNext(context.Background()); err != nil {
			t.Fatal(tc.name, err)
		}
		if (sender.sends == 1) != tc.sends || repo.commits != 1 || tc.sends != (repo.status == 204) || !tc.sends && repo.status != 0 {
			t.Errorf("%s: sends=%d status=%d", tc.name, sender.sends, repo.status)
		}
	}
	// A public endpoint is unaffected by the rule.
	repo := &memoryRepo{claim: Claim{AppID: "app", SubscriptionID: "hook", Endpoint: "https://hooks.example.com/in", Format: format.Thinpost, SealedSecret: sealedFor(format.Thinpost), Event: Event{ID: "event-1", MessageID: "message-1", DeliveryID: "delivery-1", AttemptID: "attempt-1", Outcome: "accepted", SMTPCode: 250, OccurredAt: time.Now()}}}
	sender := &internalFake{fakeDelivery: fakeDelivery{status: 204}, internal: internalURL}
	if err := (&Dispatcher{Repository: repo, Sender: sender, Keys: &fakeKeys{}}).DispatchNext(context.Background()); err != nil || sender.sends != 1 {
		t.Fatal("public endpoint blocked", err)
	}
}

func TestTestSendFailsClosedForInternalEndpointsOutsideNativeNoAuth(t *testing.T) {
	for _, tc := range []struct {
		name, format string
		auth         []byte
		sends        bool
	}{
		{"native", format.Thinpost, nil, true},
		{"native with auth", format.Thinpost, []byte("sealed:x"), false},
		{"resend", format.Resend, nil, false},
		{"postmark", format.Postmark, nil, false},
		{"mailgun", format.Mailgun, nil, false},
		{"inbound", format.ThinpostInbound, nil, false},
	} {
		store := &svcStore{endpoint: internalURL, format: tc.format, targetAuth: tc.auth, target: []byte("sealed:tpwh_" + "0123456789abcdef0123456789abcdef")}
		sender := &internalFake{internal: internalURL}
		sender.status = 204
		svc := &Service{Store: store, Keys: &sealKeys{}, Sender: sender, Now: time.Now}
		got, err := svc.TestWebhook(context.Background(), "op", testApp, testHook)
		if tc.sends {
			if err != nil && got.Failure == FailureBlocked {
				t.Errorf("%s: blocked", tc.name)
			}
			continue
		}
		if sender.sends != 0 || got.Failure != FailureBlocked || got.Success {
			t.Errorf("%s: sends=%d result=%+v err=%v", tc.name, sender.sends, got, err)
		}
	}
}

func TestSenderRefusesCredentialsOnInternalDestination(t *testing.T) {
	sender, log, resolver := internalSender(t, internalURL, []netip.Addr{netip.MustParseAddr("10.96.12.34")}, http.NotFoundHandler())
	for _, auth := range []admin.WebhookAuth{
		admin.NewWebhookAuth("u", "p", nil),
		admin.NewWebhookAuth("", "", []admin.WebhookHeader{{Name: "X-Token", Value: "v"}}),
	} {
		if _, err := sender.Send(context.Background(), internalURL, request(`{}`), &auth); err == nil {
			t.Fatal("credentials sent to an internal destination")
		}
	}
	if len(log.get()) != 0 || resolver.name != "" {
		t.Fatal("credentialed internal send reached the network")
	}
	if !sender.IsInternal(internalURL) || sender.IsInternal(internalURL+"/x") || sender.IsInternal("https://hooks.example.com/in") {
		t.Fatal("IsInternal misclassified")
	}
}
