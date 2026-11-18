package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/submission"
	"golang.org/x/sync/semaphore"
)

const testLimit = 1000

func encodedBytes(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }

func TestAdmissionEnforcesConfiguredAttachmentLimit(t *testing.T) {
	bodies := map[string]func(n int) (string, string, map[string]string, int){
		"postmark": func(n int) (string, string, map[string]string, int) {
			return "/email", fmt.Sprintf(`{"From":"a@example.com","To":"b@example.net","Subject":"x","TextBody":"t","Attachments":[{"Name":"a.bin","Content":%q,"ContentType":"application/octet-stream"}]}`, encodedBytes(n)), map[string]string{"X-Postmark-Server-Token": "valid-key"}, 200
		},
		"resend": func(n int) (string, string, map[string]string, int) {
			return "/emails", fmt.Sprintf(`{"from":"a@example.com","to":"b@example.net","subject":"x","text":"t","attachments":[{"filename":"a.bin","content":%q}]}`, encodedBytes(n)), map[string]string{"Authorization": "Bearer valid-key"}, 200
		},
		"sendgrid": func(n int) (string, string, map[string]string, int) {
			return "/v3/mail/send", fmt.Sprintf(`{"from":{"email":"a@example.com"},"personalizations":[{"to":[{"email":"b@example.net"}]}],"subject":"x","content":[{"type":"text/plain","value":"t"}],"attachments":[{"filename":"a.bin","content":%q,"type":"application/octet-stream"}]}`, encodedBytes(n)), map[string]string{"Authorization": "Bearer valid-key"}, 202
		},
	}
	for name, build := range bodies {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				size int
				ok   bool
			}{{testLimit, true}, {testLimit + 1, false}} {
				path, body, headers, status := build(tc.size)
				f := &fakeService{app: submission.App{ID: "app"}}
				w := request(t, NewWithOptions(f, Options{MaxAttachmentBytes: testLimit}), path, body, headers)
				if tc.ok && (w.Code != status || len(f.requests) != 1) || !tc.ok && (w.Code != 400 || len(f.requests) != 0) {
					t.Fatalf("size %d: status %d: %s", tc.size, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestAdmissionLimitIsTotalAcrossAttachments(t *testing.T) {
	body := fmt.Sprintf(`{"from":"a@example.com","to":"b@example.net","subject":"x","text":"t","attachments":[{"filename":"a","content":%q},{"filename":"b","content":%q}]}`, encodedBytes(600), encodedBytes(401))
	f := &fakeService{app: submission.App{ID: "app"}}
	if w := request(t, NewWithOptions(f, Options{MaxAttachmentBytes: testLimit}), "/emails", body, map[string]string{"Authorization": "Bearer valid-key"}); w.Code != 400 || len(f.requests) != 0 {
		t.Fatal("attachments summing above the limit accepted", w.Code)
	}
}

func TestAdmissionEnforcesConfiguredLimitInMailgunMultipart(t *testing.T) {
	for _, tc := range []struct {
		size int
		want int
	}{{testLimit, 200}, {testLimit + 1, 400}} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for k, v := range map[string]string{"from": "a@example.com", "to": "b@example.net", "subject": "x", "text": "t"} {
			_ = writer.WriteField(k, v)
		}
		part, _ := writer.CreateFormFile("attachment", "a.bin")
		_, _ = part.Write(make([]byte, tc.size))
		_ = writer.Close()
		f := &fakeService{app: submission.App{ID: "app"}}
		r := httptest.NewRequest(http.MethodPost, "/v3/example.com/messages", &body)
		r.SetBasicAuth("api", "valid-key")
		r.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		NewWithOptions(f, Options{MaxAttachmentBytes: testLimit}).ServeHTTP(w, r)
		if w.Code != tc.want || (tc.want == 200) != (len(f.requests) == 1) {
			t.Fatalf("size %d: status %d: %s", tc.size, w.Code, w.Body.String())
		}
	}
}

func TestAdmissionEnforcesConfiguredLimitInBatches(t *testing.T) {
	options := Options{MaxAttachmentBytes: testLimit}
	for _, tc := range []struct {
		size int
		ok   bool
	}{{testLimit, true}, {testLimit + 1, false}} {
		f := &fakeService{app: submission.App{ID: "app"}}
		postmark := fmt.Sprintf(`[{"From":"a@example.com","To":"b@example.net","Subject":"x","TextBody":"t","Attachments":[{"Name":"a","Content":%q}]}]`, encodedBytes(tc.size))
		w := request(t, NewWithOptions(f, options), "/email/batch", postmark, map[string]string{"X-Postmark-Server-Token": "valid-key"})
		if w.Code != 200 || tc.ok != (len(f.requests) == 1) || tc.ok != !strings.Contains(w.Body.String(), `"ErrorCode":300`) {
			t.Fatalf("postmark batch size %d: %d %s", tc.size, w.Code, w.Body.String())
		}
		b := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"id"}}}
		resend := fmt.Sprintf(`[{"from":"a@example.com","to":"b@example.net","subject":"x","text":"t","attachments":[{"filename":"a","content":%q}]}]`, encodedBytes(tc.size))
		w = request(t, NewWithOptions(b, options), "/emails/batch", resend, map[string]string{"Authorization": "Bearer valid-key"})
		if tc.ok != (b.batchCalls == 1) || tc.ok != (w.Code == 200) {
			t.Fatalf("resend batch size %d: %d %s", tc.size, w.Code, w.Body.String())
		}
		b = &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"a", "b"}}}
		sendgrid := fmt.Sprintf(`{"from":{"email":"a@example.com"},"personalizations":[{"to":[{"email":"b@example.net"}]},{"to":[{"email":"c@example.net"}]}],"subject":"x","content":[{"type":"text/plain","value":"t"}],"attachments":[{"filename":"a","content":%q}]}`, encodedBytes(tc.size))
		w = request(t, NewWithOptions(b, options), "/v3/mail/send", sendgrid, map[string]string{"Authorization": "Bearer valid-key"})
		if tc.ok != (b.batchCalls == 1) {
			t.Fatalf("sendgrid batch size %d: %d %s", tc.size, w.Code, w.Body.String())
		}
	}
}

func TestSingleRequestBodyLimitDerivesFromAttachmentLimit(t *testing.T) {
	if got := SingleBodyLimit(submission.DefaultMaxAttachmentBytes); got != 11184812 {
		t.Fatalf("default derived limit %d", got)
	}
	headers := map[string]string{"Authorization": "Bearer valid-key"}
	// A 3 MiB attachment makes a request above the 2 MiB batch limit.
	big := fmt.Sprintf(`{"from":"a@example.com","to":"b@example.net","subject":"x","text":"t","attachments":[{"filename":"a","content":%q}]}`, encodedBytes(3<<20))
	f := &fakeService{app: submission.App{ID: "app"}}
	if w := request(t, NewWithOptions(f, Options{}), "/emails", big, headers); w.Code != 200 || len(f.requests) != 1 {
		t.Fatal("single request above 2 MiB rejected", w.Code)
	}
	// Batch routes keep 2 MiB.
	b := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"id"}}}
	batch := fmt.Sprintf(`[{"from":"a@example.com","to":"b@example.net","subject":"x","text":"t","attachments":[{"filename":"a","content":%q}]}]`, encodedBytes(3<<20))
	if w := request(t, NewWithOptions(b, Options{}), "/emails/batch", batch, headers); w.Code != 400 || b.batchCalls != 0 {
		t.Fatal("batch body limit was widened", w.Code)
	}
	// A body above the derived limit is rejected before decoding.
	limit := SingleBodyLimit(testLimit)
	f = &fakeService{app: submission.App{ID: "app"}}
	small := `{"from":"a@example.com","to":"b@example.net","subject":"x","text":"t"}`
	if w := request(t, NewWithOptions(f, Options{MaxAttachmentBytes: testLimit}), "/emails", small+strings.Repeat(" ", limit-len(small)+1), headers); w.Code != 400 || len(f.requests) != 0 {
		t.Fatal("body above derived limit accepted", w.Code)
	}
	if w := request(t, NewWithOptions(f, Options{MaxAttachmentBytes: testLimit}), "/emails", small+strings.Repeat(" ", limit-len(small)), headers); w.Code != 200 {
		t.Fatal("body at derived limit rejected", w.Code)
	}
}

// gateService counts concurrent Accept calls and blocks them until released.
type gateService struct {
	fakeService
	mu          sync.Mutex
	inFlight    int
	peak        int
	started     chan struct{}
	release     chan struct{}
	acceptCalls int
}

func (g *gateService) Accept(ctx context.Context, app submission.App, req submission.Request) (submission.Receipt, error) {
	g.mu.Lock()
	g.inFlight++
	g.acceptCalls++
	g.peak = max(g.peak, g.inFlight)
	g.mu.Unlock()
	g.started <- struct{}{}
	<-g.release
	g.mu.Lock()
	g.inFlight--
	defer g.mu.Unlock()
	return g.fakeService.Accept(ctx, app, req)
}

// largeResendBody is a valid Resend request with a body above maxBody.
func largeResendBody() string {
	return fmt.Sprintf(`{"from":"a@example.com","to":"b@example.net","subject":"x","text":"t","attachments":[{"filename":"a.bin","content":%q}]}`, encodedBytes(maxBody))
}

func serve(s *Server, ctx context.Context, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/emails", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer valid-key")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func newGate() *gateService {
	return &gateService{fakeService: fakeService{app: submission.App{ID: "app"}}, started: make(chan struct{}, 16), release: make(chan struct{})}
}

func TestLargeBodiesAreBoundedByTheBudget(t *testing.T) {
	body := largeResendBody()
	weight := int64(len(body))
	gate := newGate()
	s := NewWithOptions(gate, Options{MaxAttachmentBytes: submission.MaxAttachmentBytes})
	s.large = semaphore.NewWeighted(2*weight + 100) // room for two
	const n = 6
	var wg sync.WaitGroup
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- serve(s, context.Background(), body).Code }()
	}
	for i := 0; i < 2; i++ {
		<-gate.started
	}
	select {
	case <-gate.started:
		t.Fatal("a third large request ran concurrently")
	case <-time.After(300 * time.Millisecond):
	}
	for i := 0; i < n; i++ {
		gate.release <- struct{}{}
		if i < n-2 {
			<-gate.started
		}
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Fatalf("status %d", c)
		}
	}
	if gate.peak != 2 || gate.acceptCalls != n {
		t.Fatalf("peak %d calls %d", gate.peak, gate.acceptCalls)
	}
}

func TestSmallBodiesNeverWaitForTheBudget(t *testing.T) {
	gate := newGate()
	s := NewWithOptions(gate, Options{})
	s.large = semaphore.NewWeighted(1) // nothing large fits
	close(gate.release)
	body := `{"from":"a@example.com","to":"b@example.net","subject":"x","text":"t"}`
	if w := serve(s, context.Background(), body); w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestCanceledWaiterGetsTryAgainResponse(t *testing.T) {
	body := largeResendBody()
	gate := newGate()
	s := NewWithOptions(gate, Options{})
	s.large = semaphore.NewWeighted(int64(len(body)))
	done := make(chan int)
	go func() { done <- serve(s, context.Background(), body).Code }()
	<-gate.started
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan *httptest.ResponseRecorder)
	go func() { waiter <- serve(s, ctx, body) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	var w *httptest.ResponseRecorder
	select {
	case w = <-waiter:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled waiter did not return")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("canceled waiter: status %d", w.Code)
	}
	gate.release <- struct{}{}
	if c := <-done; c != 200 {
		t.Fatal(c)
	}
	if gate.acceptCalls != 1 {
		t.Fatal("canceled waiter reached Accept")
	}
}

// SendGrid single-message requests may be large, but a request with several
// personalizations is a batch and keeps the batch body limit.
func TestSendGridBatchKeepsBatchBodyLimit(t *testing.T) {
	two := `[{"to":[{"email":"b@example.net"}]},{"to":[{"email":"c@example.net"}]}]`
	build := func(personalizations string, attachment int) string {
		return fmt.Sprintf(`{"from":{"email":"a@example.com"},"personalizations":%s,"subject":"x","content":[{"type":"text/plain","value":"t"}],"attachments":[{"filename":"a.bin","content":%q,"type":"application/octet-stream"}]}`, personalizations, encodedBytes(attachment))
	}
	one := `[{"to":[{"email":"b@example.net"}]}]`
	headers := map[string]string{"Authorization": "Bearer valid-key"}
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"single over 2 MiB", build(one, maxBody), 202},
		{"batch over 2 MiB", build(two, maxBody), 400},
		{"batch under 2 MiB", build(two, 1<<20), 202},
	} {
		f := &batchFake{fakeService: fakeService{app: submission.App{ID: "app"}}, receipt: submission.BatchReceipt{IDs: []string{"one", "two"}}}
		w := request(t, NewWithOptions(f, Options{}), "/v3/mail/send", tc.body, headers)
		if w.Code != tc.want {
			t.Fatalf("%s: status %d", tc.name, w.Code)
		}
		if tc.want == 400 && (f.batchCalls != 0 || len(f.requests) != 0) {
			t.Fatalf("%s: rejected request reached the service", tc.name)
		}
	}
}

// slowAccept is a service whose Accept takes a fixed time.
type slowAccept struct {
	fakeService
	delay time.Duration
}

func (s *slowAccept) Accept(context.Context, submission.App, submission.Request) (submission.Receipt, error) {
	time.Sleep(s.delay)
	return submission.Receipt{ID: "m", AcceptedAt: time.Now()}, nil
}

func post(url string, r io.Reader) (int, string) {
	req, _ := http.NewRequest(http.MethodPost, url+"/emails", r)
	req.Header.Set("Authorization", "Bearer valid-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1, err.Error()
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// A request admitted after the server's read deadline passed must still be read
// (deadline renewed), and one whose bounded wait expires gets 503, never 400.
func TestQueuedRequestSurvivesReadTimeoutAndWaitIsBounded(t *testing.T) {
	body := largeResendBody()
	s := NewWithOptions(&slowAccept{fakeService{app: submission.App{ID: "app"}}, 1500 * time.Millisecond}, Options{})
	s.large = semaphore.NewWeighted(int64(len(body)))
	s.waitTimeout = 3 * time.Second
	ts := httptest.NewUnstartedServer(s)
	ts.Config.ReadTimeout = time.Second
	ts.Config.WriteTimeout = 4 * time.Second
	ts.Start()
	defer ts.Close()
	codes := make(chan int, 3)
	go func() { c, _ := post(ts.URL, strings.NewReader(body)); codes <- c }()
	time.Sleep(200 * time.Millisecond)
	// Queued 1.3s: past ReadTimeout (1s) when admitted.
	c, msg := post(ts.URL, strings.NewReader(body))
	if c != 200 {
		t.Fatalf("queued request: %d %s", c, msg)
	}
	if first := <-codes; first != 200 {
		t.Fatal(first)
	}
	// Hold the budget longer than the wait bound.
	s.service = &slowAccept{fakeService{app: submission.App{ID: "app"}}, 5 * time.Second}
	go func() { c, _ := post(ts.URL, strings.NewReader(body)); codes <- c }()
	time.Sleep(200 * time.Millisecond)
	if c, msg := post(ts.URL, strings.NewReader(body)); c != 503 {
		t.Fatalf("expired wait: %d %s", c, msg)
	}
}

// A tiny chunked request must not wait while the budget is fully held.
func TestSmallChunkedRequestSkipsTheBudget(t *testing.T) {
	body := largeResendBody()
	s := NewWithOptions(&slowAccept{fakeService{app: submission.App{ID: "app"}}, time.Second}, Options{})
	s.large = semaphore.NewWeighted(int64(len(body)))
	ts := httptest.NewServer(s)
	defer ts.Close()
	go post(ts.URL, strings.NewReader(body))
	time.Sleep(200 * time.Millisecond)
	small := `{"from":"a@example.com","to":"b@example.net","subject":"x","text":"t"}`
	start := time.Now()
	// io.MultiReader hides the length, so the request is chunked.
	if c, msg := post(ts.URL, io.MultiReader(strings.NewReader(small))); c != 200 || time.Since(start) > 1400*time.Millisecond {
		t.Fatalf("small chunked request: %d %s after %v", c, msg, time.Since(start))
	}
}
