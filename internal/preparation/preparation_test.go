package preparation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/tracking"
)

type fakeTrackingObjects struct {
	mappings [][]tracking.Mapping
	err      error
}

func (o *fakeTrackingObjects) Put(_ context.Context, _ string, mappings []tracking.Mapping) ([]tracking.Reference, error) {
	if o.err != nil {
		return nil, o.err
	}
	o.mappings = append(o.mappings, append([]tracking.Mapping(nil), mappings...))
	refs := make([]tracking.Reference, 0, len(mappings))
	for _, mapping := range mappings {
		hash, err := tracking.HashToken(mapping.Token)
		if err != nil {
			return nil, err
		}
		refs = append(refs, tracking.Reference{Hash: hash, Kind: mapping.Kind})
	}
	return refs, nil
}

func trackedSource(t *testing.T, objects *fakeObjects) {
	t.Helper()
	message := submission.Message{From: "a@example.com", To: []string{"b@example.net", "c@example.net"}, Subject: "hello", HTML: `<a href="https://example.net/private">link</a>`, EnhancedProtection: true, Tracking: &submission.TrackingOptions{Opens: true, Clicks: true}}
	data, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{1, message})
	if err != nil {
		t.Fatal(err)
	}
	objects.source = data
}

func TestTrackedPreparationRequiresConfigurationAndIsolatesRecipients(t *testing.T) {
	service, repo, objects, _ := setup(t)
	trackedSource(t, objects)
	if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 || len(objects.final) != 0 {
		t.Fatal("tracking without a configured store was queued")
	}
	mappings := &fakeTrackingObjects{}
	service.Tracking = &TrackingConfig{Origin: "https://tracking.example.com", Links: preparationLinkKeys(t), EventLimit: 10, Objects: mappings}
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.commits != 1 || len(repo.deliveries) != 2 || len(mappings.mappings) != 2 {
		t.Fatal("tracking was not committed per recipient")
	}
	for i, delivery := range repo.deliveries {
		if delivery.Tracking == nil || !delivery.Tracking.Indefinite || !delivery.Tracking.ExpiresAt.IsZero() || delivery.Tracking.PostSendValiditySeconds != 0 || len(delivery.Tracking.Links) != 2 || delivery.Tracking.EventLimit != 10 || len(mappings.mappings[i]) != 2 {
			t.Fatal("tracking references were not included in the delivery")
		}
		if !bytes.HasPrefix(objects.final[delivery.ObjectKey], []byte("DKIM-Signature:")) {
			t.Fatal("tracked MIME was not signed")
		}
	}
	if mappings.mappings[0][0].Token == mappings.mappings[1][0].Token || repo.deliveries[0].Tracking.ObjectKey == repo.deliveries[1].Tracking.ObjectKey {
		t.Fatal("recipients shared tracking capabilities or mapping objects")
	}
}

func TestTrackedPreparationDoesNotPublishWhenMappingWriteFails(t *testing.T) {
	service, repo, objects, _ := setup(t)
	trackedSource(t, objects)
	service.Tracking = &TrackingConfig{Origin: "https://tracking.example.com", Links: preparationLinkKeys(t), EventLimit: 10, Objects: &fakeTrackingObjects{err: errors.New("offline")}}
	if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 || len(objects.final) != 0 || len(repo.failures) != 1 || repo.failures[0] != "object_write" {
		t.Fatal("published after a failed tracking object write")
	}
}

func preparationLinkKeys(t *testing.T) *tracking.LinkKeys {
	t.Helper()
	keys, err := tracking.NewLinkKeys([]byte("0123456789abcdef0123456789abcdef"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestTrackedPreparationRequiresSigningKeys(t *testing.T) {
	service, repo, objects, _ := setup(t)
	trackedSource(t, objects)
	service.Tracking = &TrackingConfig{Origin: "https://tracking.example.com", EventLimit: 10, Objects: &fakeTrackingObjects{}}
	if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 {
		t.Fatal("tracking without signing keys was accepted")
	}
}

func TestClickTrackingWithoutEligibleLinksPreservesHTML(t *testing.T) {
	service, repo, objects, _ := setup(t)
	const original = `<div><a href="/relative">relative</a></div>`
	message := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "hello", HTML: original, EnhancedProtection: true, Tracking: &submission.TrackingOptions{Clicks: true}}
	data, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{1, message})
	if err != nil {
		t.Fatal(err)
	}
	objects.source = data
	mappings := &fakeTrackingObjects{}
	service.Tracking = &TrackingConfig{Origin: "https://tracking.example.com", Links: preparationLinkKeys(t), EventLimit: 10, Objects: mappings}
	if err = service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.deliveries) != 1 || repo.deliveries[0].Tracking != nil || len(mappings.mappings) != 0 {
		t.Fatal("created a mapping for an untrackable link")
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(objects.final[repo.deliveries[0].ObjectKey]))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(parsed.Body))
	if err != nil || strings.TrimSuffix(string(body), "\r\n") != original {
		t.Fatal("HTML without trackable links changed", err)
	}
}

type fakeRepository struct {
	mu         sync.Mutex
	events     []string
	renewals   int
	renewFn    func(call int) error
	until      time.Time // lease deadline; zero disables expiry
	claim      Claim
	deliveries []Delivery
	commitErr  error
	failureErr error
	terminal   bool
	commits    int
	failures   []string
}

func (r *fakeRepository) expiredLocked() bool { return !r.until.IsZero() && time.Now().After(r.until) }

func (r *fakeRepository) RenewPreparation(_ context.Context, _ Claim, ttl time.Duration) error {
	r.mu.Lock()
	if r.expiredLocked() {
		r.mu.Unlock()
		return ErrLostLease
	}
	r.until = time.Now().Add(ttl)
	r.renewals++
	call := r.renewals
	r.events = append(r.events, "renew")
	fn := r.renewFn
	r.mu.Unlock()
	if fn != nil {
		return fn(call)
	}
	return nil
}

func (r *fakeRepository) FailPreparation(_ context.Context, _ Claim, reason string) (bool, error) {
	r.mu.Lock()
	r.events = append(r.events, "fail")
	expired := r.expiredLocked()
	r.mu.Unlock()
	if expired {
		return false, ErrLostLease
	}
	r.failures = append(r.failures, reason)
	return r.terminal, r.failureErr
}

func (r *fakeRepository) ClaimPreparation(_ context.Context, token string, ttl time.Duration) (Claim, error) {
	r.mu.Lock()
	r.until = time.Now().Add(ttl)
	r.mu.Unlock()
	r.claim.Token = token
	return r.claim, nil
}
func (r *fakeRepository) CommitPreparation(_ context.Context, _ Claim, deliveries []Delivery) error {
	r.mu.Lock()
	r.events = append(r.events, "commit")
	expired := r.expiredLocked()
	r.mu.Unlock()
	if expired {
		return ErrLostLease
	}
	r.commits++
	if r.commitErr != nil {
		return r.commitErr
	}
	r.deliveries = deliveries
	return nil
}

type fakeObjects struct {
	source []byte
	final  map[string][]byte
	putErr error
}

func (o *fakeObjects) Get(_ context.Context, _ string, _ int64) ([]byte, error) { return o.source, nil }
func (o *fakeObjects) PutMIME(_ context.Context, key string, data []byte) error {
	if o.putErr != nil {
		return o.putErr
	}
	if o.final == nil {
		o.final = map[string][]byte{}
	}
	o.final[key] = append([]byte(nil), data...)
	return nil
}

type fakeKeys struct {
	key SigningKey
	app string
}

func (k *fakeKeys) ForSender(_ context.Context, app, _ string) (SigningKey, error) {
	k.app = app
	return k.key, nil
}
func setup(t *testing.T) (*Service, *fakeRepository, *fakeObjects, *fakeKeys) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	message := submission.Message{From: "a@example.com", To: []string{"b@example.net", "c@example.net"}, ReplyTo: []string{"help@example.com"}, Subject: "hello", Text: "secret body", EnhancedProtection: true}
	source, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{1, message})
	if err != nil {
		t.Fatal(err)
	}
	repo := &fakeRepository{claim: Claim{MessageID: "0123456789abcdef0123456789abcdef", SenderDomain: "example.com", AppID: "app", SourceKey: "source", AcceptedAt: time.Now(), EnhancedProtection: true}}
	objects := &fakeObjects{source: source}
	keys := &fakeKeys{key: SigningKey{Domain: "example.com", Selector: "mail", Signer: key}}
	return &Service{Repository: repo, Objects: objects, Keys: keys}, repo, objects, keys
}
func TestPrepareStoresSignedPerRecipientObjectsBeforeQueueCommit(t *testing.T) {
	service, repo, objects, keys := setup(t)
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.commits != 1 || len(repo.deliveries) != 2 || len(objects.final) != 2 || keys.app != "app" {
		t.Fatalf("incorrect preparation: %+v", repo)
	}
	for i, delivery := range repo.deliveries {
		if delivery.RecipientIndex != i {
			t.Fatal("recipient index lost")
		}
		data := objects.final[delivery.ObjectKey]
		if !bytes.HasPrefix(data, []byte("DKIM-Signature:")) {
			t.Fatal("unsigned object")
		}
		parsed, err := mail.ReadMessage(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		expected := []string{"b@example.net", "c@example.net"}[i]
		if !strings.Contains(parsed.Header.Get("To"), expected) {
			t.Fatal("wrong recipient")
		}
		replies, err := parsed.Header.AddressList("Reply-To")
		if err != nil || len(replies) != 1 || replies[0].Address != "help@example.com" {
			t.Fatal("reply address lost during preparation", err)
		}
	}
}

// Every recipient's copy carries the one message-scoped Message-ID, on every
// run; the per-recipient delivery IDs stay distinct and appear in no header.
func TestEveryRecipientCopyCarriesTheSharedMessageID(t *testing.T) {
	service, repo, objects, _ := setup(t)
	want := "<" + repo.claim.MessageID + "@example.com>"
	for run := 0; run < 2; run++ {
		repo.deliveries = nil
		if err := service.PrepareNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(repo.deliveries) != 2 || repo.deliveries[0].ID == repo.deliveries[1].ID {
			t.Fatal("delivery IDs must be distinct")
		}
		for _, delivery := range repo.deliveries {
			data := objects.final[delivery.ObjectKey]
			parsed, err := mail.ReadMessage(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if got := parsed.Header.Get("Message-ID"); got != want {
				t.Fatalf("run %d: Message-ID %q, want %q", run, got, want)
			}
			if bytes.Contains(data, []byte(delivery.ID+"@")) {
				t.Fatal("delivery ID leaked into a header")
			}
		}
	}
}

func TestPreparationRequiresTheRecordedSenderDomain(t *testing.T) {
	for name, domain := range map[string]string{"empty": "", "different": "other.example"} {
		t.Run(name, func(t *testing.T) {
			service, repo, objects, _ := setup(t)
			repo.claim.SenderDomain = domain
			if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 || len(objects.final) != 0 || len(repo.failures) != 1 || repo.failures[0] != "signing_key" {
				t.Fatal("prepared without a matching recorded sender domain", err, repo.failures)
			}
		})
	}
}

func TestPreparationFailureNeverQueuesDeliveries(t *testing.T) {
	t.Run("oversized signed MIME", func(t *testing.T) {
		service, repo, objects, _ := setup(t)
		payload := struct {
			Version int                `json:"version"`
			Message submission.Message `json:"message"`
		}{1, submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "large", Text: strings.Repeat("é", 1500000), EnhancedProtection: true}}
		var err error
		objects.source, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err = service.PrepareNext(context.Background()); err == nil || repo.commits != 0 || len(objects.final) != 0 || len(repo.failures) != 1 || repo.failures[0] != "mime_build" {
			t.Fatal("oversized signed MIME was stored or queued", err)
		}
	})
	t.Run("TLS policy mismatch", func(t *testing.T) {
		service, repo, objects, _ := setup(t)
		repo.claim.EnhancedProtection = false
		if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 || len(objects.final) != 0 || len(repo.failures) != 1 || repo.failures[0] != "source_invalid" {
			t.Fatal("accepted corrupted policy")
		}
	})
	t.Run("object store failure", func(t *testing.T) {
		service, repo, objects, _ := setup(t)
		objects.putErr = errors.New("offline")
		if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 || len(repo.failures) != 1 || repo.failures[0] != "object_write" {
			t.Fatal("queued without durable artifacts")
		}
	})
	t.Run("lost lease", func(t *testing.T) {
		service, repo, objects, _ := setup(t)
		repo.commitErr = ErrLostLease
		if err := service.PrepareNext(context.Background()); !errors.Is(err, ErrLostLease) || len(objects.final) != 2 || len(repo.deliveries) != 0 || len(repo.failures) != 0 {
			t.Fatal("stale claim committed")
		}
	})
}

func TestPreparationLogsOnlyIDsAndStatuses(t *testing.T) {
	var lines []string
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }))
	service, repo, _, _ := setup(t)
	repo.claim.AppID, repo.claim.MessageID = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "message app=0123456789abcdef0123456789abcdef message=fedcba9876543210fedcba9876543210 status=prepared"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("unexpected success log %q", lines)
	}
	lines = nil
	service, repo, _, _ = setup(t)
	repo.claim.AppID, repo.claim.MessageID = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	repo.claim.EnhancedProtection = false
	repo.terminal = true
	if err := service.PrepareNext(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	want = "message app=0123456789abcdef0123456789abcdef message=fedcba9876543210fedcba9876543210 status=preparation_failed reason=source_invalid"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("unexpected failure log %q", lines)
	}
	for _, line := range lines {
		for _, forbidden := range []string{"example", "hello", "secret", "@"} {
			if strings.Contains(line, forbidden) {
				t.Fatalf("log contains %q: %s", forbidden, line)
			}
		}
	}
}

func TestRetriedPreparationFailureIsNotLoggedAsFailed(t *testing.T) {
	var lines []string
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }))
	service, repo, _, _ := setup(t)
	repo.claim.AppID, repo.claim.MessageID = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	repo.claim.EnhancedProtection = false
	if err := service.PrepareNext(context.Background()); err == nil || len(repo.failures) != 1 {
		t.Fatal("expected recorded failure", err)
	}
	if len(lines) != 0 {
		t.Fatalf("requeued failure was logged: %q", lines)
	}
}

func oneClickSource(t *testing.T, objects *fakeObjects, edit func(*submission.Message)) {
	t.Helper()
	message := submission.Message{From: "a@example.com", To: []string{"b@example.net", "c@example.net"}, Subject: "hello", Text: "body"}
	if edit != nil {
		edit(&message)
	}
	data, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{1, message})
	if err != nil {
		t.Fatal(err)
	}
	objects.source = data
}

func TestOneClickUnsubscribeIsPreparedPerDelivery(t *testing.T) {
	service, repo, objects, _ := setup(t)
	oneClickSource(t, objects, nil)
	repo.claim.EnhancedProtection = false
	repo.claim.ListUnsubscribe = true
	keys := preparationLinkKeys(t)
	service.Tracking = &TrackingConfig{Origin: "https://tracking.example.com", Links: keys, EventLimit: 10, Objects: &fakeTrackingObjects{}}
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.commits != 1 || len(repo.deliveries) != 2 {
		t.Fatal("not committed")
	}
	seen := map[string]bool{}
	handler := &tracking.Handler{Origin: "https://tracking.example.com", Links: keys}
	for _, delivery := range repo.deliveries {
		data := objects.final[delivery.ObjectKey]
		parsed, err := mail.ReadMessage(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		list := parsed.Header.Get("List-Unsubscribe")
		if !strings.HasPrefix(list, "<https://tracking.example.com/t/u/v2/") || !strings.HasSuffix(list, ">") || strings.Contains(list, delivery.ID) || seen[list] {
			t.Fatal("unexpected or shared unsubscribe URL")
		}
		seen[list] = true
		if parsed.Header.Get("List-Unsubscribe-Post") != "List-Unsubscribe=One-Click" {
			t.Fatal("missing one-click post header")
		}
		// The signed link verifies (a GET shows the confirmation page).
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, strings.Trim(list, "<>"), nil))
		if w.Code != 200 {
			t.Fatal("prepared link does not verify", w.Code)
		}
		// DKIM covers both headers.
		signature := strings.ToLower(parsed.Header.Get("DKIM-Signature"))
		if !strings.Contains(signature, "list-unsubscribe") || !strings.Contains(signature, "list-unsubscribe-post") {
			t.Fatal("DKIM does not cover the unsubscribe headers")
		}
	}
	// The stored source is untouched: the headers belong to the delivery only.
	if strings.Contains(string(objects.source), "ListUnsubscribe") {
		t.Fatal("source carries the managed header")
	}
}

func TestOneClickWithoutTrackingRuntimeFailsVisibly(t *testing.T) {
	service, repo, objects, _ := setup(t)
	oneClickSource(t, objects, nil)
	repo.claim.EnhancedProtection = false
	repo.claim.ListUnsubscribe = true
	for name, config := range map[string]*TrackingConfig{
		"no tracking": nil,
		"no keys":     {Origin: "https://tracking.example.com", EventLimit: 10, Objects: &fakeTrackingObjects{}},
		"no origin":   {Links: preparationLinkKeys(t), EventLimit: 10, Objects: &fakeTrackingObjects{}},
	} {
		repo.commits, repo.failures = 0, nil
		service.Tracking = config
		if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 || len(objects.final) != 0 || len(repo.failures) != 1 || repo.failures[0] != "internal" {
			t.Fatal(name, "was queued or not failed with reason internal", repo.failures)
		}
	}
}

func TestOneClickWithANonASCIIOriginFailsWithReasonInternal(t *testing.T) {
	service, repo, objects, _ := setup(t)
	oneClickSource(t, objects, nil)
	repo.claim.EnhancedProtection = false
	repo.claim.ListUnsubscribe = true
	service.Tracking = &TrackingConfig{Origin: "https://träck.example.com", Links: preparationLinkKeys(t), EventLimit: 10, Objects: &fakeTrackingObjects{}}
	if err := service.PrepareNext(context.Background()); err == nil || repo.commits != 0 || len(repo.failures) != 1 || repo.failures[0] != "internal" {
		t.Fatal("non-ASCII origin was prepared or not failed with reason internal", repo.failures)
	}
}

func TestOneClickIsOffUnlessTheClaimSaysSo(t *testing.T) {
	service, repo, objects, _ := setup(t)
	oneClickSource(t, objects, nil)
	repo.claim.EnhancedProtection = false
	service.Tracking = &TrackingConfig{Origin: "https://tracking.example.com", Links: preparationLinkKeys(t), EventLimit: 10, Objects: &fakeTrackingObjects{}}
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, delivery := range repo.deliveries {
		if bytes.Contains(objects.final[delivery.ObjectKey], []byte("List-Unsubscribe")) {
			t.Fatal("header added for an app that is not opted in")
		}
	}
}

func TestOneClickKeepsAppSuppliedHeadersFromASourceAcceptedBeforeTheToggle(t *testing.T) {
	service, repo, objects, _ := setup(t)
	oneClickSource(t, objects, func(m *submission.Message) {
		m.ListUnsubscribe, m.ListUnsubscribePost = "<https://app.example.com/u>", true
	})
	repo.claim.EnhancedProtection = false
	repo.claim.ListUnsubscribe = true
	// Even without a tracking runtime: the app's own header is passed through.
	if err := service.PrepareNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, delivery := range repo.deliveries {
		parsed, err := mail.ReadMessage(bytes.NewReader(objects.final[delivery.ObjectKey]))
		if err != nil || parsed.Header.Get("List-Unsubscribe") != "<https://app.example.com/u>" {
			t.Fatal("app header replaced", err)
		}
	}
}

// walkParts visits every leaf part of a MIME entity with its decoded body.
func walkParts(t *testing.T, header textproto.MIMEHeader, body io.Reader, visit func(textproto.MIMEHeader, []byte)) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		t.Fatal("bad content type", err)
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		reader := multipart.NewReader(body, params["boundary"])
		for {
			part, err := reader.NextRawPart()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			walkParts(t, part.Header, part, visit)
		}
	}
	var decoded io.Reader = body
	switch strings.ToLower(header.Get("Content-Transfer-Encoding")) {
	case "base64":
		decoded = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		decoded = quotedprintable.NewReader(body)
	}
	data, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatal("cannot decode part", err)
	}
	visit(header, data)
}

func TestTrackedPreparationKeepsAttachmentsAndHidesDestinations(t *testing.T) {
	service, repo, objects, _ := setup(t)
	const destination = "https://example.net/private/order-4417"
	logo := bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 0, 1, 2, 0xFF}, 40)
	report := []byte("%PDF-1.7\r\nreport body \x00\xff\r\n" + strings.Repeat("row,1,2\n", 500))
	message := submission.Message{
		From: "a@example.com", To: []string{"b@example.net"}, Subject: "hello",
		HTML:               `<p><a href="` + destination + `">order</a> <img src="cid:logo@example.com"></p>`,
		EnhancedProtection: true, Tracking: &submission.TrackingOptions{Opens: true, Clicks: true},
		Attachments: []submission.Attachment{
			{Filename: "logo.png", ContentType: "image/png", Inline: true, ContentID: "logo@example.com", Content: logo},
			{Filename: "report.pdf", ContentType: "application/pdf", Content: report},
		},
	}
	source, err := json.Marshal(struct {
		Version int                `json:"version"`
		Message submission.Message `json:"message"`
	}{1, message})
	if err != nil {
		t.Fatal(err)
	}
	objects.source = source
	service.Tracking = &TrackingConfig{Origin: "https://tracking.example.com", Links: preparationLinkKeys(t), EventLimit: 10, Objects: &fakeTrackingObjects{}}
	if err := service.PrepareNext(context.Background()); err != nil || len(repo.deliveries) != 1 {
		t.Fatal("preparation failed", err)
	}
	raw := objects.final[repo.deliveries[0].ObjectKey]
	if bytes.Contains(raw, []byte(destination)) || bytes.Contains(raw, []byte("order-4417")) {
		t.Fatal("original destination appears in the MIME")
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	type leaf struct {
		header textproto.MIMEHeader
		data   []byte
	}
	var leaves []leaf
	walkParts(t, textproto.MIMEHeader(parsed.Header), parsed.Body, func(h textproto.MIMEHeader, data []byte) {
		leaves = append(leaves, leaf{h, data})
	})
	var sawHTML bool
	attachments := map[string]leaf{}
	for _, l := range leaves {
		mediaType, params, _ := mime.ParseMediaType(l.header.Get("Content-Type"))
		_, dispParams, _ := mime.ParseMediaType(l.header.Get("Content-Disposition"))
		name := dispParams["filename"]
		if name == "" {
			name = params["name"]
		}
		if mediaType == "text/html" {
			sawHTML = true
			html := string(l.data)
			if !strings.Contains(html, "https://tracking.example.com/t/c/") || !strings.Contains(html, "https://tracking.example.com/t/o/") || strings.Contains(html, "order-4417") {
				t.Fatal("HTML lacks rewritten click links or the open pixel")
			}
			if !strings.Contains(html, "cid:logo@example.com") {
				t.Fatal("inline image reference was rewritten")
			}
			continue
		}
		if name != "" {
			attachments[name] = leaf{l.header, l.data}
			if got := l.header.Get("Content-Type"); !strings.HasPrefix(got, mediaType) || mediaType != map[string]string{"logo.png": "image/png", "report.pdf": "application/pdf"}[name] {
				t.Fatalf("%s content type %q", name, got)
			}
		}
	}
	if !sawHTML || len(attachments) != 2 {
		t.Fatalf("html=%v attachments=%d", sawHTML, len(attachments))
	}
	if !bytes.Equal(attachments["logo.png"].data, logo) || !bytes.Equal(attachments["report.pdf"].data, report) {
		t.Fatal("attachment content changed")
	}
	if got := attachments["logo.png"].header.Get("Content-ID"); got != "<logo@example.com>" {
		t.Fatalf("Content-ID %q", got)
	}
	if attachments["report.pdf"].header.Get("Content-ID") != "" {
		t.Fatal("regular attachment gained a Content-ID")
	}
}

// A stored source with a ceiling-size attachment prepares even if the
// configured admission limit was lowered afterwards: readers know only the ceiling.
func TestPreparationAcceptsCeilingSizeAttachmentSource(t *testing.T) {
	for _, tc := range []struct {
		size int
		ok   bool
	}{{submission.MaxAttachmentBytes, true}, {submission.MaxAttachmentBytes + 1, false}} {
		service, repo, objects, _ := setup(t)
		message := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "hello", Text: "body", HTML: "<p>body</p>", EnhancedProtection: true,
			Attachments: []submission.Attachment{{Filename: "big.bin", ContentType: "application/octet-stream", Content: bytes.Repeat([]byte{7, 8, 9}, tc.size/3+1)[:tc.size]}}}
		source, err := json.Marshal(struct {
			Version int                `json:"version"`
			Message submission.Message `json:"message"`
		}{1, message})
		if err != nil {
			t.Fatal(err)
		}
		objects.source = source
		err = service.PrepareNext(context.Background())
		if tc.ok && (err != nil || len(repo.deliveries) != 1) {
			t.Fatal("ceiling-size source not prepared", err)
		}
		if !tc.ok && len(repo.deliveries) != 0 {
			t.Fatal("source above the ceiling prepared")
		}
	}
}
