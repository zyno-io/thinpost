package edgeworker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/textproto"
	"path/filepath"
	"strings"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/smtptransport"
)

type fakeDialer struct {
	serverDone chan error
	fail       bool
	dials      int
	source     netip.Addr
	address    netip.Addr
	helo       string
	recipient  string
	mx         string
}

type greetingRejectionDialer struct{}

type timeoutDialer struct{}

func (timeoutDialer) Dial(ctx context.Context, _, _ netip.Addr, _, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type stalledSMTPDialer struct {
	afterData bool
	done      chan error
}

func (d stalledSMTPDialer) Dial(context.Context, netip.Addr, netip.Addr, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		reader := bufio.NewReader(server)
		if d.afterData {
			if _, err := fmt.Fprint(server, "220 ready\r\n"); err != nil {
				d.done <- err
				return
			}
			if _, err := reader.ReadString('\n'); err != nil {
				d.done <- err
				return
			}
			if _, err := fmt.Fprint(server, "250 hello\r\n"); err != nil {
				d.done <- err
				return
			}
			for range 3 {
				line, err := reader.ReadString('\n')
				if err != nil {
					d.done <- err
					return
				}
				response := "250 ok\r\n"
				if strings.HasPrefix(line, "DATA") {
					response = "354 send\r\n"
				}
				if _, err = fmt.Fprint(server, response); err != nil {
					d.done <- err
					return
				}
			}
			if _, err := textproto.NewReader(reader).ReadDotBytes(); err != nil {
				d.done <- err
				return
			}
		}
		// Wait for the worker to close its side, without replying. In the
		// after-DATA case the server may already have accepted the message.
		_, _ = reader.ReadByte()
		d.done <- nil
	}()
	return client, nil
}

func (greetingRejectionDialer) Dial(context.Context, netip.Addr, netip.Addr, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		_, _ = fmt.Fprint(server, "554 endpoint unavailable\r\n")
	}()
	return client, nil
}

func allowStart(context.Context, *controlv1.DeliveryStart) error { return nil }

func (d *fakeDialer) Dial(_ context.Context, source, address netip.Addr, recipient, mx string) (net.Conn, error) {
	d.dials++
	d.source, d.address, d.recipient, d.mx = source, address, recipient, mx
	if d.fail {
		return nil, errors.New("offline")
	}
	client, server := net.Pipe()
	d.serverDone = make(chan error, 1)
	go func() {
		defer server.Close()
		reader := bufio.NewReader(server)
		_, _ = fmt.Fprint(server, "220 ready\r\n")
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "EHLO ") {
			d.serverDone <- errors.New("EHLO")
			return
		}
		d.helo = strings.TrimSpace(strings.TrimPrefix(line, "EHLO "))
		_, _ = fmt.Fprint(server, "250 hello\r\n")
		for _, cmd := range []string{"MAIL FROM:", "RCPT TO:", "DATA"} {
			line, err = reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, cmd) {
				d.serverDone <- err
				return
			}
			response := "250 ok\r\n"
			if cmd == "DATA" {
				response = "354 send\r\n"
			}
			_, _ = fmt.Fprint(server, response)
		}
		_, err = textproto.NewReader(reader).ReadDotBytes()
		if err != nil {
			d.serverDone <- err
			return
		}
		_, _ = fmt.Fprint(server, "250 accepted\r\n")
		line, err = reader.ReadString('\n')
		if err == nil && line == "QUIT\r\n" {
			_, _ = fmt.Fprint(server, "221 bye\r\n")
		}
		d.serverDone <- err
	}()
	return client, nil
}

type failingJournal struct{ *journal.Journal }

func (f failingJournal) CompleteReply(string, string, int, string, string, time.Time) error {
	return errors.New("disk full")
}
func setup(t *testing.T) (*journal.Journal, journal.Fence, *controlv1.DeliveryStart, []byte) {
	t.Helper()
	j, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	fence := journal.Fence{ControllerID: "controller", Epoch: 1}
	if err = j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	mime := []byte("Subject: test\r\n\r\nbody\r\n")
	digest := sha256.Sum256(mime)
	start := &controlv1.DeliveryStart{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, MxName: "mx.example", DestinationIp: "8.8.8.8", EnvelopeFrom: "bounce@example.com", Recipient: "to@example.net", ExpectedSize: uint64(len(mime)), Sha256: digest[:], SourceAddress: "45.0.1.22", HeloName: "smtp.sender.example"}
	return j, fence, start, mime
}
func TestExecutePersistsResultBeforeReturning(t *testing.T) {
	j, fence, start, mime := setup(t)
	dialer := &fakeDialer{}
	worker := &Worker{Journal: j, Dialer: dialer, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := worker.Execute(ctx, fence, start, mime, func() bool { return true }, allowStart)
	if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED || result.SmtpCode != 250 {
		t.Fatalf("result %+v %v", result, err)
	}
	if err = <-dialer.serverDone; err != nil {
		t.Fatal(err)
	}
	attempts, err := j.List()
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != journal.Accepted {
		t.Fatal("accepted result not journaled")
	}
}

func TestSMTPAttemptTimeoutPreservesAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name      string
		afterData bool
		outcome   controlv1.DeliveryOutcome
		journal   string
	}{
		{"before greeting", false, controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED, journal.Rejected},
		{"after DATA", true, controlv1.DeliveryOutcome_DELIVERY_OUTCOME_UNCERTAIN, journal.Uncertain},
	} {
		t.Run(test.name, func(t *testing.T) {
			j, fence, start, mime := setup(t)
			done := make(chan error, 1)
			worker := &Worker{Journal: j, Dialer: stalledSMTPDialer{afterData: test.afterData, done: done}, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames, smtpTimeout: 500 * time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := worker.Execute(ctx, fence, start, mime, func() bool { return true }, allowStart)
			if err != nil || result.Outcome != test.outcome || result.SmtpCode != 0 {
				t.Fatal("incorrect timeout outcome", result, err)
			}
			if err = <-done; err != nil {
				t.Fatal("SMTP fixture failed", err)
			}
			items, err := j.List()
			if err != nil || len(items) != 1 || items[0].Outcome != test.journal {
				t.Fatal("timeout outcome was not journaled", items, err)
			}
		})
	}
}

func TestSMTPConnectTimeoutIsJournaledAsRejected(t *testing.T) {
	j, fence, start, mime := setup(t)
	policy := smtptransport.DefaultTimeouts()
	policy.Connect = time.Second
	worker := &Worker{Journal: j, Dialer: timeoutDialer{}, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames, SMTPTimeouts: &policy}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	result, err := worker.Execute(ctx, fence, start, mime, func() bool { return true }, allowStart)
	if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED {
		t.Fatal("connect timeout not rejected", result, err)
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != journal.Rejected {
		t.Fatal("connect timeout not journaled", items, err)
	}
}

func TestTrackingDeadlineStopsDelayedPermitAndBoundsSMTP(t *testing.T) {
	t.Run("expired before permit", func(t *testing.T) {
		j, fence, start, mime := setup(t)
		start.TrackingSendCutoffUnixMs = time.Now().Add(delivery.TrackingClockMargin - time.Second).UnixMilli()
		dialer := &fakeDialer{}
		worker := &Worker{Journal: j, Dialer: dialer, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
		permits := 0
		result, err := worker.Execute(context.Background(), fence, start, mime, func() bool { return true }, func(context.Context, *controlv1.DeliveryStart) error { permits++; return nil })
		if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED || permits != 0 || dialer.dials != 0 {
			t.Fatal("expired tracking reached the permit or network", err)
		}
	})
	t.Run("expires while awaiting permit", func(t *testing.T) {
		j, fence, start, mime := setup(t)
		start.TrackingSendCutoffUnixMs = time.Now().Add(delivery.TrackingClockMargin + time.Second).UnixMilli()
		dialer := &fakeDialer{}
		worker := &Worker{Journal: j, Dialer: dialer, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
		result, err := worker.Execute(context.Background(), fence, start, mime, func() bool { return true }, func(context.Context, *controlv1.DeliveryStart) error { time.Sleep(1200 * time.Millisecond); return nil })
		if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED || dialer.dials != 0 {
			t.Fatal("delayed permit allowed expired tracking to reach the network", err)
		}
	})
	t.Run("expires while waiting for DATA response", func(t *testing.T) {
		j, fence, start, mime := setup(t)
		start.TrackingSendCutoffUnixMs = time.Now().Add(delivery.TrackingClockMargin + 1500*time.Millisecond).UnixMilli()
		done := make(chan error, 1)
		worker := &Worker{Journal: j, Dialer: stalledSMTPDialer{afterData: true, done: done}, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := worker.Execute(ctx, fence, start, mime, func() bool { return true }, allowStart)
		if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_UNCERTAIN {
			t.Fatal("lost DATA response was not held uncertain at tracking deadline", err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestGreetingRejectionPreservesRouteScopeAndReply(t *testing.T) {
	j, fence, start, mime := setup(t)
	w := &Worker{Journal: j, Dialer: greetingRejectionDialer{}, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := w.Execute(ctx, fence, start, mime, func() bool { return true }, allowStart)
	if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ROUTE_REJECTED || result.SmtpCode != 554 {
		t.Fatal("connection rejection lost its scope or SMTP code", result, err)
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != journal.RouteRejected || items[0].SMTPCode != 554 {
		t.Fatal("route rejection was not durably journaled", items, err)
	}
	wrong := items[0]
	wrong.Outcome = journal.Rejected
	if err = j.Acknowledge(wrong); !errors.Is(err, journal.ErrInvalidTransition) {
		t.Fatal("acknowledgement with a different rejection scope was accepted", err)
	}
	if err = j.Acknowledge(items[0]); err != nil {
		t.Fatal("exact result acknowledgement failed", err)
	}
}

// The Edge enforces neither a recipient allowlist nor an egress deny range:
// control is authoritative for both and has already filtered the route it
// authorized. The worker dials exactly the numeric address it was given, for
// any recipient, and delivers.
func TestWorkerExecutesWhatControlAuthorized(t *testing.T) {
	j, fence, start, mime := setup(t)
	start.DestinationIp = "10.0.0.5"
	start.Recipient = "anyone@example.org"
	d := &fakeDialer{}
	w := &Worker{Journal: j, Dialer: d, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
	result, err := w.Execute(context.Background(), fence, start, mime, func() bool { return true }, allowStart)
	if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED || d.dials != 1 || d.address.String() != "10.0.0.5" || d.recipient != "anyone@example.org" {
		t.Fatalf("worker second-guessed control: %+v dials=%d %v", result, d.dials, err)
	}
}

func TestDialerReceivesAuthorizedRoute(t *testing.T) {
	j, fence, start, mime := setup(t)
	start.EnhancedProtection = true
	d := &fakeDialer{fail: true}
	w := &Worker{Journal: j, Dialer: d, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
	if _, err := w.Execute(context.Background(), fence, start, mime, func() bool { return true }, allowStart); err != nil {
		t.Fatal(err)
	}
	if d.dials != 1 || d.address.String() != start.DestinationIp || d.recipient != start.Recipient || d.mx != start.MxName {
		t.Fatal("dialer did not receive the authorized SMTP route")
	}
}
func TestRequiredTLSAndJournalFailure(t *testing.T) {
	t.Run("missing start permit prevents SMTP", func(t *testing.T) {
		j, fence, start, mime := setup(t)
		d := &fakeDialer{}
		worker := &Worker{Journal: j, Dialer: d, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
		result, err := worker.Execute(context.Background(), fence, start, mime, func() bool { return true }, func(context.Context, *controlv1.DeliveryStart) error { return errors.New("denied") })
		if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED || d.dials != 0 {
			t.Fatal("SMTP started without permit", err)
		}
		items, err := j.List()
		if err != nil || len(items) != 1 || items[0].Outcome != journal.Rejected {
			t.Fatal("denied attempt not journaled", err)
		}
	})
	t.Run("canceled stream cannot start work", func(t *testing.T) {
		j, fence, start, mime := setup(t)
		d := &fakeDialer{}
		worker := &Worker{Journal: j, Dialer: d, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := worker.Execute(ctx, fence, start, mime, func() bool { return true }, allowStart); err == nil || d.dials != 0 {
			t.Fatal("canceled stream started SMTP")
		}
		items, err := j.List()
		if err != nil || len(items) != 0 {
			t.Fatal("canceled stream created attempt", err)
		}
	})
	t.Run("expired lease prevents SMTP", func(t *testing.T) {
		for _, allowedChecks := range []int{1, 2, 3} {
			j, fence, start, mime := setup(t)
			d := &fakeDialer{}
			w := &Worker{Journal: j, Dialer: d, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
			checks := 0
			result, err := w.Execute(context.Background(), fence, start, mime, func() bool { checks++; return checks <= allowedChecks }, allowStart)
			expectedDials := 0
			if allowedChecks == 3 {
				expectedDials = 1
			}
			if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED || d.dials != expectedDials {
				t.Fatalf("expired lease sent mail: %+v %v", result, err)
			}
			if d.serverDone != nil {
				<-d.serverDone
			}
			items, err := j.List()
			if err != nil || len(items) != 1 || items[0].Outcome != journal.Rejected {
				t.Fatal("expired attempt not journaled", err)
			}
		}
	})
	t.Run("no TLS", func(t *testing.T) {
		j, fence, start, mime := setup(t)
		start.EnhancedProtection = true
		d := &fakeDialer{}
		w := &Worker{Journal: j, Dialer: d, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		result, err := w.Execute(ctx, fence, start, mime, func() bool { return true }, allowStart)
		if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED || d.dials != 1 {
			t.Fatalf("%+v %v", result, err)
		}
		<-d.serverDone
		items, _ := j.List()
		if len(items) != 1 || items[0].Outcome != journal.Rejected {
			t.Fatal("missing rejected result")
		}
	})
	t.Run("result write failure", func(t *testing.T) {
		j, fence, start, mime := setup(t)
		d := &fakeDialer{}
		w := &Worker{Journal: failingJournal{j}, Dialer: d, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := w.Execute(ctx, fence, start, mime, func() bool { return true }, allowStart); !errors.Is(err, ErrJournal) || d.dials != 1 {
			t.Fatal("failed journal was reported as success")
		}
		<-d.serverDone
		if err := j.Recover(time.Now()); err != nil {
			t.Fatal(err)
		}
		items, _ := j.List()
		if len(items) != 1 || items[0].Outcome != journal.Uncertain {
			t.Fatal("ambiguous attempt was not recovered")
		}
	})
	t.Run("tampered MIME", func(t *testing.T) {
		j, fence, start, mime := setup(t)
		d := &fakeDialer{}
		w := &Worker{Journal: j, Dialer: d, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
		mime[0] ^= 1
		if _, err := w.Execute(context.Background(), fence, start, mime, func() bool { return true }, allowStart); err == nil || d.dials != 0 {
			t.Fatal("tampered MIME was sent")
		}
	})
}

// replyDialer answers MAIL, RCPT and DATA with scripted replies; everything
// else succeeds. A scripted DATA reply of "" means 354 and an accepted message.
type replyDialer struct{ mail, rcpt, data string }

func (d replyDialer) Dial(context.Context, netip.Addr, netip.Addr, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		reader := bufio.NewReader(server)
		_, _ = fmt.Fprint(server, "220 ready\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			reply := "250 ok\r\n"
			switch {
			case strings.HasPrefix(line, "MAIL"):
				if d.mail != "" {
					reply = d.mail + "\r\n"
				}
			case strings.HasPrefix(line, "RCPT"):
				if d.rcpt != "" {
					reply = d.rcpt + "\r\n"
				}
			case strings.HasPrefix(line, "DATA"):
				if d.data != "" {
					reply = d.data + "\r\n"
				} else {
					_, _ = fmt.Fprint(server, "354 send\r\n")
					if _, err = textproto.NewReader(reader).ReadDotBytes(); err != nil {
						return
					}
					reply = "250 queued\r\n"
				}
			}
			_, _ = fmt.Fprint(server, reply)
		}
	}()
	return client, nil
}

func TestResultsCarryStageAndEnhancedCodeWithoutReplyText(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dialer   replyDialer
		stage    controlv1.SmtpStage
		journal  string
		enhanced string
		code     uint32
	}{
		{"rcpt 5.1.1", replyDialer{rcpt: "550 5.1.1 <to@example.net> unknown user"}, controlv1.SmtpStage_SMTP_STAGE_RCPT, "RCPT", "5.1.1", 550},
		{"rcpt 5.1.10", replyDialer{rcpt: "550 5.1.10 recipient not found"}, controlv1.SmtpStage_SMTP_STAGE_RCPT, "RCPT", "5.1.10", 550},
		{"rcpt no enhanced code", replyDialer{rcpt: "550 no such user"}, controlv1.SmtpStage_SMTP_STAGE_RCPT, "RCPT", "", 550},
		{"rcpt 4xx", replyDialer{rcpt: "450 4.2.1 mailbox busy"}, controlv1.SmtpStage_SMTP_STAGE_RCPT, "RCPT", "4.2.1", 450},
		{"mail from", replyDialer{mail: "550 5.7.1 blocked"}, controlv1.SmtpStage_SMTP_STAGE_MAIL, "MAIL", "5.7.1", 550},
		{"data", replyDialer{data: "554 5.7.1 spam"}, controlv1.SmtpStage_SMTP_STAGE_DATA, "DATA", "5.7.1", 554},
		{"mismatched class", replyDialer{rcpt: "550 4.1.1 odd"}, controlv1.SmtpStage_SMTP_STAGE_RCPT, "RCPT", "", 550},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, fence, start, mime := setup(t)
			w := &Worker{Journal: j, Dialer: tc.dialer, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := w.Execute(ctx, fence, start, mime, func() bool { return true }, allowStart)
			if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED || result.SmtpCode != tc.code || result.SmtpStage != tc.stage || result.SmtpEnhanced != tc.enhanced {
				t.Fatalf("result %+v %v", result, err)
			}
			items, err := j.List()
			if err != nil || len(items) != 1 || items[0].SMTPStage != tc.journal || items[0].SMTPEnhanced != tc.enhanced {
				t.Fatalf("journal %+v %v", items, err)
			}
			raw, _ := json.Marshal(items[0])
			if strings.Contains(string(raw), "example.net") || strings.Contains(string(raw), "unknown user") {
				t.Fatalf("reply text retained: %s", raw)
			}
		})
	}
}

func TestSetupRepliesAndAcceptanceCarryNoStage(t *testing.T) {
	j, fence, start, mime := setup(t)
	w := &Worker{Journal: j, Dialer: greetingRejectionDialer{}, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
	result, err := w.Execute(context.Background(), fence, start, mime, func() bool { return true }, allowStart)
	if err != nil || result.SmtpStage != controlv1.SmtpStage_SMTP_STAGE_UNSPECIFIED || result.SmtpEnhanced != "" {
		t.Fatalf("route rejection carries a stage: %+v %v", result, err)
	}
	j, fence, start, mime = setup(t)
	w = &Worker{Journal: j, Dialer: replyDialer{}, HeloName: "smtp.sender.example", Sources: testSources, SourceHostnames: testHostnames}
	result, err = w.Execute(context.Background(), fence, start, mime, func() bool { return true }, allowStart)
	if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED || result.SmtpStage != controlv1.SmtpStage_SMTP_STAGE_UNSPECIFIED || result.SmtpEnhanced != "" {
		t.Fatalf("acceptance carries a stage: %+v %v", result, err)
	}
}
