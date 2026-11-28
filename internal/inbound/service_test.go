package inbound

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/oplog"
)

const (
	appA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	appB     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	msgShare = "11111111111111111111111111111111"
	msgLegac = "22222222222222222222222222222222"
	del1     = "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1"
	del2     = "d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2"
	del3     = "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"
	msgOther = "33333333333333333333333333333333"
	delOther = "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4"
)

// fakeRepo is an in-memory Repository that applies the same rules the SQL does:
// candidates are per app, the shared form matches a message ID and the legacy
// form a delivery ID.
type fakeRepo struct {
	mu         sync.Mutex
	resolution Resolution
	resolveErr error
	check      Check
	checkErr   error
	commit     CommitStatus
	commitErr  error
	candidates []struct {
		app string
		c   Candidate
	}
	candErr  error
	closed   bool
	checks   []bool
	counts   []string
	records  []Record
	lookups  [][]string
	resolved [][]string
}

func (f *fakeRepo) ResolveInbound(_ context.Context, domains []string) (Resolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, domains)
	return f.resolution, f.resolveErr
}
func (f *fakeRepo) CheckInbound(_ context.Context, _ string, _ [][32]byte, correlated bool) (Check, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, correlated)
	return f.check, f.checkErr
}
func (f *fakeRepo) CountInbound(_ context.Context, app, result string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts = append(f.counts, app+":"+result)
	return nil
}
func (f *fakeRepo) ReplyCandidates(_ context.Context, app string, ids []string) ([]Candidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups = append(f.lookups, ids)
	if f.candErr != nil {
		return nil, f.candErr
	}
	var out []Candidate
	for _, id := range ids {
		for _, x := range f.candidates {
			if x.app == app && x.c.Key == id {
				out = append(out, x.c)
			}
		}
	}
	return out, nil
}
func (f *fakeRepo) CommitInbound(_ context.Context, r Record) (CommitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, r)
	return f.commit, f.commitErr
}
func (f *fakeRepo) InboundRoutes(context.Context) (RouteSet, error) {
	return RouteSet{Open: !f.closed, Routes: []Route{{Domain: "a.test", AppID: appA, Accept: true}}}, nil
}

func (f *fakeRepo) add(app string, c Candidate) {
	f.candidates = append(f.candidates, struct {
		app string
		c   Candidate
	}{app, c})
}

type fakeObjects struct {
	mu   sync.Mutex
	puts map[string][]byte
	err  error
	gate chan struct{}
}

func (o *fakeObjects) PutMIME(_ context.Context, key string, data []byte) error {
	if o.gate != nil {
		<-o.gate
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return o.err
	}
	if o.puts == nil {
		o.puts = map[string][]byte{}
	}
	o.puts[key] = append([]byte(nil), data...)
	return nil
}

func newService(repo *fakeRepo, objects *fakeObjects) *Service {
	return &Service{Repository: repo, Objects: objects, Keys: [][]byte{[]byte("0123456789abcdef0123456789abcdef")}, Resolver: &fakeDNS{}, AuthBudget: time.Second}
}

func testMessage(body string) Message {
	return Message{ID: strings.Repeat("c", 32), MailFrom: "alice@example.org", Rcpts: []string{"reply@a.test"}, ClientIP: "192.0.2.1", Helo: "mx.example.org", Receiver: "smtp.mx.test", Body: []byte(body)}
}

func reply(headers string) string {
	return "From: Alice <alice@example.org>\r\nTo: reply@a.test\r\nSubject: Re: hi\r\n" + headers + "\r\n\r\nthanks\r\n"
}

func idHeader(id, domain string) string { return "<" + id + "@" + domain + ">" }

func TestCorrelationBothFormsAndPickByFrom(t *testing.T) {
	repo := &fakeRepo{}
	// Shared form: one message, three recipients, bob is the replying one.
	for _, c := range []Candidate{
		{Key: msgShare, MessageID: msgShare, DeliveryID: del1, SenderDomain: "mail.example.com", Shared: true, Recipient: "alice@example.org", EnhancedProtection: true},
		{Key: msgShare, MessageID: msgShare, DeliveryID: del2, SenderDomain: "mail.example.com", Shared: true, Recipient: "bob@example.org"},
		{Key: msgShare, MessageID: msgShare, DeliveryID: del3, SenderDomain: "mail.example.com", Shared: true, Recipient: "carol@example.org"},
		// Legacy form: the ID is a delivery ID.
		{Key: del1[:0] + msgLegac, MessageID: "4444444444444444444444444444aaaa", DeliveryID: msgLegac, SenderDomain: "mail.example.com", Shared: false, Recipient: "alice@example.org"},
	} {
		repo.add(appA, c)
	}
	ctx := context.Background()

	got, err := Correlate(ctx, repo, appA, Parse([]byte(reply("In-Reply-To: "+idHeader(msgShare, "mail.example.com")+"\r\n"))))
	if err != nil || got == nil || got.MessageID != msgShare || got.DeliveryID != del1 || got.Header != "in_reply_to" || !got.FromMatches || !got.EnhancedProtection {
		t.Fatalf("shared form, From matches one recipient: %+v %v", got, err)
	}

	// The From address matches no recipient: message-level, no delivery.
	stranger := strings.Replace(reply("In-Reply-To: "+idHeader(msgShare, "mail.example.com")), "alice@example.org", "stranger@example.net", 1)
	got, _ = Correlate(ctx, repo, appA, Parse([]byte(stranger)))
	if got == nil || got.MessageID != msgShare || got.DeliveryID != "" || got.FromMatches {
		t.Fatalf("shared form, no From match: %+v", got)
	}

	// Two deliveries to the same address: not exactly one, so message-level.
	repo.add(appA, Candidate{Key: msgShare, MessageID: msgShare, DeliveryID: "d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5", SenderDomain: "mail.example.com", Shared: true, Recipient: "alice@example.org"})
	got, _ = Correlate(ctx, repo, appA, Parse([]byte(reply("In-Reply-To: "+idHeader(msgShare, "mail.example.com")+"\r\n"))))
	if got == nil || got.DeliveryID != "" || !got.FromMatches {
		t.Fatalf("shared form, ambiguous From: %+v", got)
	}

	// Legacy form identifies the delivery directly.
	got, _ = Correlate(ctx, repo, appA, Parse([]byte(reply("In-Reply-To: "+idHeader(msgLegac, "mail.example.com")+"\r\n"))))
	if got == nil || got.DeliveryID != msgLegac || got.MessageID != "4444444444444444444444444444aaaa" || !got.FromMatches {
		t.Fatalf("legacy form: %+v", got)
	}
}

func TestCorrelationOrderAndGuards(t *testing.T) {
	repo := &fakeRepo{}
	repo.add(appA, Candidate{Key: msgShare, MessageID: msgShare, DeliveryID: del1, SenderDomain: "mail.example.com", Shared: true, Recipient: "alice@example.org"})
	repo.add(appA, Candidate{Key: msgLegac, MessageID: "4444444444444444444444444444aaaa", DeliveryID: msgLegac, SenderDomain: "mail.example.com", Recipient: "alice@example.org"})
	repo.add(appB, Candidate{Key: msgOther, MessageID: msgOther, DeliveryID: delOther, SenderDomain: "mail.example.com", Shared: true, Recipient: "alice@example.org"})
	ctx := context.Background()
	do := func(headers string) *Correlation {
		t.Helper()
		got, err := Correlate(ctx, repo, appA, Parse([]byte(reply(headers))))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	a, b, c := idHeader(msgShare, "mail.example.com"), idHeader(msgLegac, "mail.example.com"), idHeader(msgOther, "mail.example.com")

	// In-Reply-To wins over References.
	if got := do("In-Reply-To: " + b + "\r\nReferences: " + a + "\r\n"); got.MessageID == msgShare || got.Header != "in_reply_to" {
		t.Fatalf("In-Reply-To should win: %+v", got)
	}
	// References are scanned last to first.
	if got := do("References: " + a + " " + b + "\r\n"); got.DeliveryID != msgLegac || got.Header != "references" {
		t.Fatalf("References order: %+v", got)
	}
	// An ID of another app is skipped silently, and scanning continues.
	if got := do("In-Reply-To: " + c + "\r\nReferences: " + a + "\r\n"); got == nil || got.MessageID != msgShare || got.Header != "references" {
		t.Fatalf("cross-app skip: %+v", got)
	}
	if got := do("In-Reply-To: " + c + "\r\n"); got != nil {
		t.Fatalf("an ID owned by another app correlated: %+v", got)
	}
	// The sender domain must match the ID's right-hand side.
	if got := do("In-Reply-To: " + idHeader(msgShare, "evil.example") + "\r\n"); got != nil {
		t.Fatalf("domain mismatch correlated: %+v", got)
	}
	if got := do("In-Reply-To: " + idHeader(msgShare, "MAIL.Example.COM") + "\r\n"); got == nil {
		t.Fatal("the domain compare must be case-insensitive")
	}
	// Malformed IDs are ignored.
	for _, bad := range []string{"<notahexid@mail.example.com>", "<" + msgShare + ">", msgShare + "@mail.example.com", "<" + strings.ToUpper(del1) + "@mail.example.com>", "garbage"} {
		if got := do("In-Reply-To: " + bad + "\r\n"); got != nil {
			t.Fatalf("%q correlated", bad)
		}
	}
	// A lookup failure is an error, never a silent miss.
	repo.candErr = errors.New("database unavailable")
	if _, err := Correlate(ctx, repo, appA, Parse([]byte(reply("In-Reply-To: "+a+"\r\n")))); err == nil {
		t.Fatal("repository error swallowed")
	}
}

func TestCorrelationScansAtMostTwentyIdentifiers(t *testing.T) {
	repo := &fakeRepo{}
	var refs []string
	for i := 0; i < 30; i++ {
		refs = append(refs, idHeader(strings.Repeat(string(rune('a'+i%6)), 31)+string(rune('0'+i%10)), "mail.example.com"))
	}
	// The earliest reference is the one that exists, but it is the 30th scanned.
	repo.add(appA, Candidate{Key: strings.Repeat("a", 31) + "0", MessageID: msgShare, DeliveryID: del1, SenderDomain: "mail.example.com", Recipient: "alice@example.org"})
	got, _ := Correlate(context.Background(), repo, appA, Parse([]byte(reply("References: "+strings.Join(refs, " ")+"\r\n"))))
	if got != nil {
		t.Fatal("more than twenty identifiers were scanned")
	}
	if len(repo.lookups) != 1 || len(repo.lookups[0]) > 20 {
		t.Fatalf("lookups: %v", repo.lookups)
	}
}

func TestReceiveRejectsAndTempfailsBeforeStoring(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  func(*fakeRepo)
		mutate func(*Message)
		want   Verdict
	}{
		"empty body":    {func(*fakeRepo) {}, func(m *Message) { m.Body = nil }, Rejected},
		"too large":     {func(*fakeRepo) {}, func(m *Message) { m.Body = make([]byte, MaxBytes+1) }, Rejected},
		"bad envelope":  {func(*fakeRepo) {}, func(m *Message) { m.Rcpts = []string{"nope"} }, Rejected},
		"unroutable":    {func(r *fakeRepo) { r.resolution = Resolution{AppID: appA, Status: Unroutable} }, nil, Tempfail},
		"backlog":       {func(r *fakeRepo) { r.resolution = Resolution{AppID: appA, Status: Backlog} }, nil, Tempfail},
		"database down": {func(r *fakeRepo) { r.resolveErr = errors.New("gate closed") }, nil, Tempfail},
		"check fails":   {func(r *fakeRepo) { r.checkErr = errors.New("db") }, nil, Tempfail},
		"full":          {func(r *fakeRepo) { r.check = CheckFull }, nil, Tempfail},
		"duplicate":     {func(r *fakeRepo) { r.check = CheckDuplicate }, nil, Accepted},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &fakeRepo{resolution: Resolution{AppID: appA, Status: Routed}, commit: CommitAccepted}
			tc.setup(repo)
			objects := &fakeObjects{}
			m := testMessage(reply(""))
			if tc.mutate != nil {
				tc.mutate(&m)
			}
			if got := newService(repo, objects).Receive(context.Background(), m); got != tc.want {
				t.Fatalf("verdict %v want %v", got, tc.want)
			}
			if len(objects.puts) != 0 || len(repo.records) != 0 {
				t.Fatal("something was stored for a refused message")
			}
		})
	}
}

func TestReceiveStoresAndCommits(t *testing.T) {
	repo := &fakeRepo{resolution: Resolution{AppID: appA, Status: Routed}, commit: CommitAccepted}
	repo.add(appA, Candidate{Key: msgShare, MessageID: msgShare, DeliveryID: del1, SenderDomain: "mail.example.com", Shared: true, Recipient: "alice@example.org"})
	objects := &fakeObjects{}
	var lines []string
	restore := oplog.SetOutput(func(format string, args ...any) { lines = append(lines, format) })
	defer restore()
	body := reply("In-Reply-To: " + idHeader(msgShare, "mail.example.com") + "\r\nMessage-ID: <m1@example.org>\r\n")
	m := testMessage(body)
	m.Rcpts = []string{"Reply+Ticket@a.test", "reply+ticket@A.test", "other@b.test"}
	m.Helo = "mx\x00\xff.example.org"
	if got := newService(repo, objects).Receive(context.Background(), m); got != Accepted {
		t.Fatalf("verdict %v", got)
	}
	if len(repo.records) != 1 || len(objects.puts) != 1 {
		t.Fatalf("records %d puts %d", len(repo.records), len(objects.puts))
	}
	r := repo.records[0]
	if string(objects.puts[r.ObjectKey]) != body || !strings.HasPrefix(r.ObjectKey, "inbound/v1/") || !strings.HasSuffix(r.ObjectKey, "/"+r.ID) {
		t.Fatal("stored object does not match the record")
	}
	if r.AppID != appA || r.Domain != "a.test" || strings.Join(r.Domains, ",") != "a.test,b.test" || len(r.Recipients) != 2 || r.Recipients[0] != "Reply+Ticket@a.test" {
		t.Fatalf("recipients %+v", r)
	}
	if r.MailFrom != "alice@example.org" || r.HeaderFrom != "alice@example.org" || r.Subject != "Re: hi" || r.MessageID != "<m1@example.org>" || r.Size != len(body) || r.Receiver != "smtp.mx.test" || r.ClientIP.String() != "192.0.2.1" {
		t.Fatalf("fields %+v", r)
	}
	if r.Helo != "mx.example.org" {
		t.Fatalf("HELO was not sanitized: %q", r.Helo)
	}
	if r.Correlation == nil || r.Correlation.DeliveryID != del1 || r.Auth.SPF.Result != "none" || len(r.Fingerprints) != 1 {
		t.Fatalf("correlation/auth %+v", r)
	}
	if len(repo.resolved) < 1 || strings.Join(repo.resolved[0], ",") != "a.test,b.test" {
		t.Fatalf("routing was not resolved from the recipients' domains: %v", repo.resolved)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "inbound result=") {
			t.Fatalf("unexpected log line %q", l)
		}
	}
}

func TestReceiveCommitOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		status CommitStatus
		err    error
		want   Verdict
		result string
	}{
		"accepted":  {CommitAccepted, nil, Accepted, "accepted"},
		"duplicate": {CommitDuplicate, nil, Accepted, "duplicate"},
		"limit":     {CommitTempfailLimit, nil, Tempfail, "tempfail_limit"},
		"routing":   {CommitTempfailRouting, nil, Tempfail, "tempfail_routing"},
		"error":     {CommitAccepted, errors.New("commit failed"), Tempfail, "tempfail_internal"},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &fakeRepo{resolution: Resolution{AppID: appA, Status: Routed}, commit: tc.status, commitErr: tc.err}
			var lines []string
			restore := oplog.SetOutput(func(format string, args ...any) {
				if len(args) > 0 {
					lines = append(lines, args[0].(string))
				}
			})
			defer restore()
			if got := newService(repo, &fakeObjects{}).Receive(context.Background(), testMessage(reply(""))); got != tc.want {
				t.Fatalf("verdict %v want %v", got, tc.want)
			}
			if len(lines) != 1 || lines[0] != tc.result {
				t.Fatalf("logged %v want %s", lines, tc.result)
			}
		})
	}
}

func TestReceiveUploadFailureOrCorrelationErrorIsTempfail(t *testing.T) {
	repo := &fakeRepo{resolution: Resolution{AppID: appA, Status: Routed}, commit: CommitAccepted}
	if got := newService(repo, &fakeObjects{err: errors.New("s3 down")}).Receive(context.Background(), testMessage(reply(""))); got != Tempfail || len(repo.records) != 0 {
		t.Fatal("an upload failure was not a tempfail, or committed anyway")
	}
	repo = &fakeRepo{resolution: Resolution{AppID: appA, Status: Routed}, commit: CommitAccepted, candErr: errors.New("db")}
	body := reply("In-Reply-To: " + idHeader(msgShare, "mail.example.com") + "\r\n")
	if got := newService(repo, &fakeObjects{}).Receive(context.Background(), testMessage(body)); got != Tempfail || len(repo.records) != 0 {
		t.Fatal("a failed correlation lookup was ignored")
	}
}

// The commit never runs before the upload finished.
func TestReceiveCommitsOnlyAfterUpload(t *testing.T) {
	repo := &fakeRepo{resolution: Resolution{AppID: appA, Status: Routed}, commit: CommitAccepted}
	objects := &fakeObjects{gate: make(chan struct{})}
	done := make(chan Verdict, 1)
	go func() { done <- newService(repo, objects).Receive(context.Background(), testMessage(reply(""))) }()
	select {
	case <-done:
		t.Fatal("received before the object was stored")
	case <-time.After(100 * time.Millisecond):
	}
	repo.mu.Lock()
	committed := len(repo.records)
	repo.mu.Unlock()
	if committed != 0 {
		t.Fatal("committed before the upload finished")
	}
	close(objects.gate)
	if got := <-done; got != Accepted {
		t.Fatal(got)
	}
}

// Authentication never delays or fails a message beyond its budget.
func TestReceiveWithUnansweringDNSStillCommits(t *testing.T) {
	repo := &fakeRepo{resolution: Resolution{AppID: appA, Status: Routed}, commit: CommitAccepted}
	s := newService(repo, &fakeObjects{})
	s.Resolver = &fakeDNS{block: map[string]bool{"example.org": true, "_dmarc.example.org": true}}
	s.AuthBudget = 150 * time.Millisecond
	started := time.Now()
	if got := s.Receive(context.Background(), testMessage(reply(""))); got != Accepted {
		t.Fatal(got)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("DNS stalled the message")
	}
	r := repo.records[0]
	if r.Auth.SPF.Result != "temperror" || r.Auth.DMARC.Result != "temperror" {
		t.Fatalf("%+v", r.Auth)
	}
}

func TestRoutesTempfailAllWhileGateClosed(t *testing.T) {
	repo := &fakeRepo{}
	s := newService(repo, &fakeObjects{})
	routes, err := s.Routes(context.Background())
	if err != nil || len(routes) != 1 || !routes[0].Accept {
		t.Fatalf("%v %v", routes, err)
	}
	repo.closed = true
	routes, err = s.Routes(context.Background())
	if err != nil || len(routes) != 1 || routes[0].Accept {
		t.Fatalf("a closed gate still accepts: %v %v", routes, err)
	}
}

// A retry of an already committed message is answered 250 even when the app
// cannot receive right now.
func TestDuplicateIsAcceptedEvenWhenAppIsUnroutable(t *testing.T) {
	for _, status := range []RouteStatus{Unroutable, Backlog} {
		repo := &fakeRepo{resolution: Resolution{AppID: appA, Status: status}, check: CheckDuplicate}
		objects := &fakeObjects{}
		if got := newService(repo, objects).Receive(context.Background(), testMessage(reply(""))); got != Accepted {
			t.Fatalf("status %v: verdict %v", status, got)
		}
		if len(repo.counts) != 1 || repo.counts[0] != appA+":duplicate" || len(objects.puts) != 0 {
			t.Fatalf("%v %v", repo.counts, objects.puts)
		}
		// Not a duplicate: still a tempfail, counted.
		repo = &fakeRepo{resolution: Resolution{AppID: appA, Status: status}, check: CheckNew}
		if got := newService(repo, objects).Receive(context.Background(), testMessage(reply(""))); got != Tempfail {
			t.Fatal(got)
		}
		want := appA + ":tempfail_routing"
		if status == Backlog {
			want = appA + ":tempfail_limit"
		}
		if len(repo.counts) != 1 || repo.counts[0] != want {
			t.Fatalf("%v", repo.counts)
		}
	}
}

// Pre-upload refusals and duplicates are counted, the correlation is known
// before the pre-check (so the real cap applies) and the stored ID is the
// worker's transfer ID.
func TestPreUploadDecisionsAreCountedAndCorrelationPrecedesTheCheck(t *testing.T) {
	repo := &fakeRepo{resolution: Resolution{AppID: appA, Status: Routed}, check: CheckFull}
	repo.add(appA, Candidate{Key: msgShare, MessageID: msgShare, DeliveryID: del1, SenderDomain: "mail.example.com", Shared: true, Recipient: "alice@example.org"})
	objects := &fakeObjects{}
	s := newService(repo, objects)
	if got := s.Receive(context.Background(), testMessage(reply(""))); got != Tempfail {
		t.Fatal(got)
	}
	if got := s.Receive(context.Background(), testMessage(reply("In-Reply-To: "+idHeader(msgShare, "mail.example.com")+"\r\n"))); got != Tempfail {
		t.Fatal(got)
	}
	if len(repo.checks) != 2 || repo.checks[0] || !repo.checks[1] {
		t.Fatalf("the check must know whether the message correlated: %v", repo.checks)
	}
	if len(objects.puts) != 0 || len(repo.counts) != 2 || repo.counts[0] != appA+":tempfail_limit" {
		t.Fatalf("an over-cap message was uploaded or not counted: %v %v", objects.puts, repo.counts)
	}
	repo.check = CheckNew
	repo.commit = CommitAccepted
	m := testMessage(reply(""))
	if got := s.Receive(context.Background(), m); got != Accepted {
		t.Fatal(got)
	}
	if r := repo.records[0]; r.ID != m.ID || !strings.HasSuffix(r.ObjectKey, "/"+m.ID) {
		t.Fatalf("the stored ID is not the transfer ID: %s", r.ID)
	}
}

func TestDeterministicCommitRefusalIsRejected(t *testing.T) {
	repo := &fakeRepo{resolution: Resolution{AppID: appA, Status: Routed}, commit: CommitRejected}
	if got := newService(repo, &fakeObjects{}).Receive(context.Background(), testMessage(reply(""))); got != Rejected {
		t.Fatalf("a record the database can never store must not loop as 451: %v", got)
	}
}

type hangingRepo struct{ *fakeRepo }

func (hangingRepo) CommitInbound(ctx context.Context, _ Record) (CommitStatus, error) {
	<-ctx.Done()
	return CommitTempfailRouting, ctx.Err()
}

func TestReceiveHasADeadline(t *testing.T) {
	repo := hangingRepo{&fakeRepo{resolution: Resolution{AppID: appA, Status: Routed}}}
	s := newService(repo.fakeRepo, &fakeObjects{})
	s.Repository = repo
	s.ReceiveTimeout = 200 * time.Millisecond
	started := time.Now()
	if got := s.Receive(context.Background(), testMessage(reply(""))); got != Tempfail {
		t.Fatal(got)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("no deadline")
	}
	if ReceiveTimeout >= 60*time.Second {
		t.Fatal("control must give up before the worker's 60 second wait")
	}
}
