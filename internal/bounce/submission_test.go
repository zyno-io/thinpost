package bounce

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	smtp "github.com/zyno-io/thinpost/internal/thirdparty/gosmtp"
)

func TestSubmissionEHLOOmitsChunkingAndAdvertisesSize(t *testing.T) {
	for name, o := range map[string]subOptions{
		"587":      {},
		"465":      {implicit: true},
		"internal": {internal: true},
	} {
		t.Run(name, func(t *testing.T) {
			env := startSubmission(t, o)
			c := env.dial()
			caps := c.ehlo()
			if !env.implicit {
				if strings.Contains(caps, "AUTH") {
					t.Fatalf("AUTH offered before STARTTLS: %q", caps)
				}
				if !strings.Contains(caps, "STARTTLS") {
					t.Fatalf("STARTTLS not offered: %q", caps)
				}
				c.startTLS()
				caps = c.ehlo()
			}
			if strings.Contains(caps, "CHUNKING") {
				t.Fatalf("CHUNKING advertised: %q", caps)
			}
			if !strings.Contains(caps, "AUTH PLAIN LOGIN") || !strings.Contains(caps, "SIZE 7864320") {
				t.Fatalf("missing AUTH or SIZE: %q", caps)
			}
			// BDAT gets exactly one 502 and the connection closes.
			c.send("BDAT 5 LAST\r\nhello")
			if code, _ := c.reply(); code != 502 {
				t.Fatalf("BDAT: %d", code)
			}
			if !c.readEOF() {
				t.Fatal("connection stayed open after BDAT")
			}
		})
	}
}

func TestSubmissionAuthRequiresTLSAndPrecedesMail(t *testing.T) {
	env := startSubmission(t, subOptions{})
	c := env.dial()
	c.ehlo()
	c.expect(523, "AUTH PLAIN "+plainIR("", testCredID, testCredSecret))
	c.expect(530, "MAIL FROM:<sender@example.com>")
	if env.sub.verifies != 0 {
		t.Fatal("credential was checked before TLS")
	}
}

func TestSubmissionPlainAndLogin(t *testing.T) {
	for name, steps := range map[string]func(*subClient){
		"plain with initial response": func(c *subClient) {
			c.expect(235, "AUTH PLAIN "+plainIR("", testCredID, testCredSecret))
		},
		"plain with matching authzid": func(c *subClient) {
			c.expect(235, "AUTH PLAIN "+plainIR(testCredID, testCredID, testCredSecret))
		},
		"plain challenge": func(c *subClient) {
			c.expect(334, "AUTH PLAIN")
			c.expect(235, plainIR("", testCredID, testCredSecret))
		},
		"login prompts": func(c *subClient) {
			if msg := c.expect(334, "AUTH LOGIN"); msg != b64("Username:") {
				t.Fatalf("challenge %q", msg)
			}
			if msg := c.expect(334, b64(testCredID)); msg != b64("Password:") {
				t.Fatalf("challenge %q", msg)
			}
			c.expect(235, b64(testCredSecret))
		},
		"login initial response": func(c *subClient) {
			c.expect(334, "AUTH LOGIN "+b64(testCredID))
			c.expect(235, b64(testCredSecret))
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := startSubmission(t, subOptions{implicit: true})
			c := env.dial()
			if caps := c.ehlo(); !strings.Contains(caps, "AUTH PLAIN LOGIN") {
				t.Fatalf("AUTH not offered at once on 465: %q", caps)
			}
			steps(c)
			if code, msg := c.message(simpleBody); code != 250 || !strings.Contains(msg, "OK: queued as "+testMessageID) {
				t.Fatalf("%d %q", code, msg)
			}
			m := env.sub.last()
			if m.Credential.ID != testCredID || m.Port != 465 || m.TLSVersion == "" || string(m.Body) != simpleBody || m.MailFrom != "sender@example.com" || len(m.Rcpts) != 1 || m.Rcpts[0] != "to@example.net" {
				t.Fatalf("unexpected message %+v", m.MailFrom)
			}
		})
	}
}

func TestSubmissionAuthRefusals(t *testing.T) {
	foreign := "tps_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	cases := map[string]struct {
		line string
		want int
	}{
		"wrong secret":      {"AUTH PLAIN " + plainIR("", testCredID, foreign), 535},
		"malformed secret":  {"AUTH PLAIN " + plainIR("", testCredID, "password"), 535},
		"malformed id":      {"AUTH PLAIN " + plainIR("", "alice", testCredSecret), 535},
		"foreign authzid":   {"AUTH PLAIN " + plainIR("someone", testCredID, testCredSecret), 535},
		"unknown mechanism": {"AUTH CRAM-MD5", 504},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			limiter := &fakeLimiter{}
			env := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{AuthLimiter: limiter}})
			c := env.dial()
			c.ehlo()
			c.expect(tc.want, tc.line)
			c.expect(530, "MAIL FROM:<sender@example.com>")
			if checks, refunds := limiter.counts(); tc.want != 504 && (checks != 1 || refunds != 0) {
				t.Fatalf("the attempt was charged %d times and refunded %d", checks, refunds)
			}
		})
	}
}

func TestSubmissionAuthVerdicts(t *testing.T) {
	cases := map[string]struct {
		result AuthResult
		want   int
		closed bool
	}{
		"tempfail":    {AuthResult{Reply: Reply{Code: 454}}, 454, false},
		"unspecified": {AuthResult{}, 454, false},
		"unavailable": {AuthResult{Reply: Reply{Code: 421, Enhanced: "4.3.2", Text: "Service not available, closing connection", Close: true}}, 421, true},
		"invalid":     {AuthResult{Invalid: true}, 535, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeSubmission()
			f.verify = func(Credential) AuthResult { return tc.result }
			env := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{Submission: f}})
			c := env.dial()
			c.ehlo()
			c.expect(tc.want, "AUTH PLAIN "+plainIR("", testCredID, testCredSecret))
			if tc.closed && !c.readEOF() {
				t.Fatal("connection stayed open")
			}
		})
	}
	t.Run("no control session", func(t *testing.T) {
		f := newFakeSubmission()
		f.setDown(true)
		env := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{Submission: f}})
		c := env.dial()
		c.ehlo()
		c.expect(454, "AUTH PLAIN "+plainIR("", testCredID, testCredSecret))
		if f.verifies != 0 {
			t.Fatal("verify called without a session")
		}
	})
}

func TestSubmissionThreeAuthAttemptsThenClose(t *testing.T) {
	env := startSubmission(t, subOptions{implicit: true})
	c := env.dial()
	c.ehlo()
	bad := "AUTH PLAIN " + plainIR("", testCredID, "tps_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
	for i := 0; i < 3; i++ {
		c.expect(535, bad)
	}
	// Even the right credential is refused on the fourth attempt.
	c.expect(421, "AUTH PLAIN "+plainIR("", testCredID, testCredSecret))
	if !c.readEOF() {
		t.Fatal("connection stayed open")
	}
}

func TestSubmissionAuthIPLimiter(t *testing.T) {
	limiter := &fakeLimiter{gate: AuthGateBlocked}
	stats := &subStats{}
	env := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{AuthLimiter: limiter, Stats: stats}})
	c := env.dial()
	c.ehlo()
	c.expect(421, "AUTH PLAIN "+plainIR("", testCredID, testCredSecret))
	if !c.readEOF() {
		t.Fatal("connection stayed open")
	}
	if env.sub.verifies != 0 || stats.get("auth_blocked") != 1 {
		t.Fatal("a blocked client reached control")
	}
	limiter.gate = AuthGateBusy
	c = env.dial()
	c.ehlo()
	c.expect(454, "AUTH PLAIN "+plainIR("", testCredID, testCredSecret))
}

// Auth is called for any AUTH command once TLS is up, even mid-transaction.
func TestSubmissionAuthDuringTransactionIs503(t *testing.T) {
	core := &submissionBackend{cfg: SubmissionConfig{Submission: newFakeSubmission(), maxBytes: MaxSubmissionBytes}}
	s := &submissionSession{backend: core}
	s.txn = &submissionTxn{}
	_, err := s.Auth("PLAIN")
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != 503 || se.EnhancedCode != (smtp.EnhancedCode{5, 5, 1}) {
		t.Fatalf("got %v", err)
	}
	if s.authAttempts != 0 {
		t.Fatal("a refused AUTH must not count as an attempt")
	}
}

func TestSubmissionEnvelopeRules(t *testing.T) {
	env := startSubmission(t, subOptions{implicit: true})
	c := env.ready()
	c.expect(552, fmt.Sprintf("MAIL FROM:<sender@example.com> SIZE=%d", MaxSubmissionBytes+1))
	c.expect(250, fmt.Sprintf("MAIL FROM:<sender@example.com> SIZE=%d", MaxSubmissionBytes))
	c.expect(501, "RCPT TO:<no-at-sign>")
	c.expect(250, "RCPT TO:<to@example.net>")
	c.expect(250, "RCPT TO:<to@example.net>") // a duplicate is accepted once
	c.expect(354, "DATA")
	c.send(stuff(simpleBody))
	if code, _ := c.reply(); code != 250 {
		t.Fatalf("DATA %d", code)
	}
	if rcpts := env.sub.last().Rcpts; len(rcpts) != 1 {
		t.Fatalf("duplicate recipient kept: %v", rcpts)
	}
	c.expect(501, "MAIL FROM:<sender@example.com\x7f>")
	c.expect(250, "MAIL FROM:<>")
	for i := 0; i < 100; i++ {
		c.expect(250, fmt.Sprintf("RCPT TO:<u%d@example.net>", i))
	}
	if msg := c.expect(452, "RCPT TO:<one-more@example.net>"); !strings.Contains(msg, "4.5.3") {
		t.Fatalf("%q", msg)
	}
	c.expect(250, "RSET")
}

func TestSubmissionRcptPolicyIsNotEnforcedAtTheEdge(t *testing.T) {
	// Any well-formed recipient passes RCPT; the allowlist is control's, at DATA.
	f := newFakeSubmission()
	f.submit = func(context.Context, SubmissionMessage) (Reply, error) {
		return Reply{Code: 550, Enhanced: "5.7.1", Text: "Recipient not permitted"}, nil
	}
	env := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{Submission: f}})
	c := env.ready()
	c.expect(250, "MAIL FROM:<sender@example.com>")
	c.expect(250, "RCPT TO:<anyone@example.org>")
	c.expect(354, "DATA")
	c.send(stuff(simpleBody))
	if code, msg := c.reply(); code != 550 || !strings.Contains(msg, "5.7.1") {
		t.Fatalf("%d %q", code, msg)
	}
}

func TestSubmissionNoControlSession(t *testing.T) {
	f := newFakeSubmission()
	env := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{Submission: f}})
	c := env.ready()
	f.setDown(true)
	c.expect(451, "MAIL FROM:<sender@example.com>")
	f.setDown(false)
	c.expect(250, "MAIL FROM:<sender@example.com>")
	c.expect(250, "RCPT TO:<to@example.net>")
	f.setDown(true) // the session is lost between RCPT and DATA
	c.expect(354, "DATA")
	c.send(stuff(simpleBody))
	if code, msg := c.reply(); code != 451 || !strings.Contains(msg, "4.3.0") {
		t.Fatalf("%d %q", code, msg)
	}
	f.setDown(false)
	if code, _ := c.message(simpleBody); code != 250 {
		t.Fatalf("the connection should stay usable: %d", code)
	}
}

func TestSubmissionReplyMapping(t *testing.T) {
	cases := []struct {
		name  string
		reply Reply
		err   error
		code  int
		text  string
		close bool
	}{
		{"queued", Reply{Code: 250, Enhanced: "2.0.0", Text: "OK: queued as " + testMessageID}, nil, 250, "2.0.0 OK: queued as " + testMessageID, false},
		{"held", Reply{Code: 250, Enhanced: "2.0.0", Text: "OK: held as " + testMessageID}, nil, 250, "2.0.0 OK: held as " + testMessageID, false},
		{"rate limited", Reply{Code: 451, Enhanced: "4.4.5", Text: "Sending rate limit reached, try again later"}, nil, 451, "4.4.5", false},
		{"quota", Reply{Code: 452, Enhanced: "4.3.1", Text: "Sending limit reached, try again later"}, nil, 452, "4.3.1", false},
		{"paused", Reply{Code: 550, Enhanced: "5.7.1", Text: "Sending is paused for this app"}, nil, 550, "5.7.1", false},
		{"invalid", Reply{Code: 554, Enhanced: "5.6.0", Text: "Message headers are invalid"}, nil, 554, "5.6.0", false},
		{"bridge error", Reply{}, errors.New("timeout"), 451, "4.3.0", false},
		{"unavailable", Reply{Code: 421, Enhanced: "4.3.2", Text: "Service not available, closing connection", Close: true}, nil, 421, "4.3.2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSubmission()
			f.submit = func(context.Context, SubmissionMessage) (Reply, error) { return tc.reply, tc.err }
			env := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{Submission: f}})
			c := env.ready()
			code, msg := c.message(simpleBody)
			if code != tc.code || !strings.Contains(msg, tc.text) {
				t.Fatalf("got %d %q", code, msg)
			}
			if tc.close && !c.readEOF() {
				t.Fatal("connection stayed open")
			}
			if !tc.close {
				// Control verdicts are not refusals: the connection continues.
				c.expect(250, "RSET")
			}
		})
	}
}

func TestSubmissionExactLimitWithHeavyDotStuffing(t *testing.T) {
	env := startSubmission(t, subOptions{implicit: true})
	c := env.ready()
	// Every decoded line is ".\r\n" (3 bytes), sent as "..\r\n" (4 bytes).
	exact := strings.Repeat(".\r\n", MaxSubmissionBytes/3)
	if len(exact) != MaxSubmissionBytes {
		t.Fatal("test body size")
	}
	if code, msg := c.message(exact); code != 250 {
		t.Fatalf("exact limit: %d %q", code, msg)
	}
	if got := env.sub.last().Body; len(got) != MaxSubmissionBytes || string(got) != exact {
		t.Fatalf("body not preserved: %d bytes", len(got))
	}
	// One byte over: 552 after the drain, the reply is read, the connection
	// stays open and the next valid message is accepted.
	over := strings.Repeat(".\r\n", MaxSubmissionBytes/3-1) + "ab\r\n"
	if len(over) != MaxSubmissionBytes+1 {
		t.Fatal("test body size")
	}
	before := env.sub.count()
	if code, msg := c.message(over); code != 552 || !strings.Contains(msg, "5.3.4") {
		t.Fatalf("limit+1: %d %q", code, msg)
	}
	if env.sub.count() != before {
		t.Fatal("an oversize message reached control")
	}
	if code, _ := c.message(simpleBody); code != 250 {
		t.Fatalf("next message: %d", code)
	}
}

func TestSubmissionBudgetIsReleasedAndBoundsBuffering(t *testing.T) {
	budget := smallBudget(MaxSubmissionBytes) // room for exactly one buffering reservation
	env := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{Budget: budget}})
	c := env.ready()
	if code, _ := c.message(simpleBody); code != 250 {
		t.Fatal("first message")
	}
	if !budget.TryAcquire(MaxSubmissionBytes) {
		t.Fatal("budget not released after DATA")
	}
	budget.Release(MaxSubmissionBytes)
	// While a reservation is held elsewhere, DATA waits then answers 451.
	if !budget.TryAcquire(MaxSubmissionBytes) {
		t.Fatal("acquire")
	}
	env2 := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{Budget: budget, budgetWait: 100 * time.Millisecond}})
	c2 := env2.ready()
	if code, msg := c2.message(simpleBody); code != 451 || !strings.Contains(msg, "4.3.0") {
		t.Fatalf("%d %q", code, msg)
	}
	budget.Release(MaxSubmissionBytes)
	if code, _ := c2.message(simpleBody); code != 250 {
		t.Fatal("message after the budget freed")
	}
}

func TestSubmissionThreeRefusalsThenClose(t *testing.T) {
	// maxBytes below the server's limit exercises the session's own limit+1
	// path; equal limits exercise the server's ErrDataTooLarge path. Each
	// refusal must count exactly once: two leave the connection usable, the
	// third makes the next MAIL a 421.
	for name, cfg := range map[string]SubmissionConfig{
		"server limit":  {maxBytes: 100 << 10},
		"session limit": {maxBytes: 100 << 10, serverMaxBytes: MaxSubmissionBytes},
	} {
		t.Run(name, func(t *testing.T) {
			env := startSubmission(t, subOptions{implicit: true, cfg: cfg})
			c := env.ready()
			big := filler(100<<10 + 1)
			for i := 0; i < 3; i++ {
				if code, _ := c.message(big); code != 552 {
					t.Fatalf("refusal %d: %d", i, code)
				}
				if i < 2 {
					c.expect(250, "NOOP")
				}
			}
			c.expect(421, "MAIL FROM:<sender@example.com>")
			if !c.readEOF() {
				t.Fatal("connection stayed open")
			}
			if env.sub.count() != 0 {
				t.Fatal("oversize message reached control")
			}
		})
	}
}

func TestSubmissionControlVerdictsAndReadErrorsDoNotCount(t *testing.T) {
	f := newFakeSubmission()
	f.submit = func(context.Context, SubmissionMessage) (Reply, error) {
		return Reply{Code: 550, Enhanced: "5.7.1", Text: "Sender address not permitted"}, nil
	}
	env := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{Submission: f}})
	c := env.ready()
	for i := 0; i < 5; i++ {
		if code, _ := c.message(simpleBody); code != 550 {
			t.Fatalf("message %d: %d", i, code)
		}
	}
	// A read error is cleanup, not an early refusal.
	core := &submissionBackend{cfg: SubmissionConfig{Submission: newFakeSubmission(), Budget: smallBudget(MaxSubmissionBytes), maxBytes: MaxSubmissionBytes, budgetWait: time.Second}}
	cred := Credential{ID: testCredID, Secret: testCredSecret}
	s := &submissionSession{backend: core, auth: &cred}
	ctx, cancel := context.WithCancel(context.Background())
	s.txn = &submissionTxn{mailFrom: "a@example.com", rcpts: []string{"b@example.net"}, ctx: ctx, cancel: cancel}
	err := s.Data(io.MultiReader(strings.NewReader("partial"), errReader{errors.New("connection reset")}))
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != 451 || s.rejected != 0 {
		t.Fatalf("err=%v rejected=%d", err, s.rejected)
	}
	// A refusal of a transaction that is no longer current does not count.
	other := &submissionTxn{ctx: ctx, cancel: cancel}
	s.refuse(other, tooLarge())
	if s.rejected != 0 {
		t.Fatal("stale transaction counted")
	}
	s.refuse(s.txn, tooLarge())
	if s.rejected != 1 {
		t.Fatal("current transaction not counted")
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestSubmissionDrainBounds(t *testing.T) {
	t.Run("a client sending past the drain cap is disconnected", func(t *testing.T) {
		limits := defaultClampLimits()
		limits.drainBytes = 256 << 10
		env := startSubmission(t, subOptions{implicit: false, cfg: SubmissionConfig{maxBytes: 100 << 10}, limits: &limits})
		c := env.ready()
		c.envelope()
		go func() {
			chunk := strings.Repeat("x", 1022) + "\r\n"
			for i := 0; i < 8000; i++ {
				if _, err := c.conn.Write([]byte(chunk)); err != nil {
					return
				}
			}
		}()
		if code, _ := c.reply(); code == 552 || code == 250 {
			t.Fatalf("the reply should be lost when the cap is exceeded, got %d", code)
		}
	})
	t.Run("an exhausted budget drains within bounds and answers 451", func(t *testing.T) {
		budget := smallBudget(MaxSubmissionBytes)
		if !budget.TryAcquire(MaxSubmissionBytes) {
			t.Fatal("acquire")
		}
		env := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{Budget: budget, budgetWait: 50 * time.Millisecond}})
		c := env.ready()
		c.envelope()
		c.send(stuff(filler(300 << 10)))
		if code, msg := c.reply(); code != 451 {
			t.Fatalf("%d %q", code, msg)
		}
		budget.Release(MaxSubmissionBytes)
		if code, _ := c.message(simpleBody); code != 250 {
			t.Fatalf("connection should continue: %d", code)
		}
	})
}

func TestSubmissionDeadlines(t *testing.T) {
	limits := clampLimits{idle: 400 * time.Millisecond, transaction: time.Minute, lifetime: time.Minute, drainTotal: time.Minute, drainIdle: time.Minute, drainBytes: 1 << 30}
	t.Run("a slow but steady upload beyond the idle time succeeds", func(t *testing.T) {
		env := startSubmission(t, subOptions{implicit: true, limits: &limits})
		c := env.ready()
		c.envelope()
		start := time.Now()
		for i := 0; i < 12; i++ {
			c.send(strings.Repeat("s", 60) + "\r\n")
			time.Sleep(100 * time.Millisecond)
		}
		c.send(".\r\n")
		if code, _ := c.reply(); code != 250 {
			t.Fatalf("slow upload: %d", code)
		}
		if time.Since(start) < limits.idle {
			t.Fatal("the upload was not slower than the idle time")
		}
	})
	t.Run("a stalled upload is cut at idle", func(t *testing.T) {
		env := startSubmission(t, subOptions{implicit: true, limits: &limits})
		c := env.ready()
		c.envelope()
		c.send("partial line\r\n")
		start := time.Now()
		code, _ := c.reply()
		if code == 250 || time.Since(start) > 5*time.Second {
			t.Fatalf("stalled upload not cut: %d after %v", code, time.Since(start))
		}
		if env.sub.count() != 0 {
			t.Fatal("a stalled upload reached control")
		}
	})
	t.Run("the transaction cap holds", func(t *testing.T) {
		l := limits
		l.transaction = 500 * time.Millisecond
		env := startSubmission(t, subOptions{implicit: true, limits: &l})
		c := env.ready()
		c.envelope()
		go func() {
			for i := 0; i < 50; i++ {
				if _, err := c.conn.Write([]byte("tick\r\n")); err != nil {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
		start := time.Now()
		for {
			code, _ := c.reply()
			if code == 0 || code == 421 {
				break
			}
			if time.Since(start) > 5*time.Second {
				t.Fatal("transaction cap did not cut the upload")
			}
		}
	})
	t.Run("the connection lifetime holds", func(t *testing.T) {
		l := limits
		l.lifetime = 500 * time.Millisecond
		env := startSubmission(t, subOptions{implicit: true, limits: &l})
		c := env.ready()
		start := time.Now()
		for {
			code, _ := c.cmd("NOOP")
			if code != 250 || time.Since(start) > 5*time.Second {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if time.Since(start) > 4*time.Second {
			t.Fatal("lifetime cap did not cut the connection")
		}
	})
}

func TestClampConnModes(t *testing.T) {
	limits := clampLimits{idle: 200 * time.Millisecond, transaction: time.Minute, lifetime: time.Minute, drainTotal: time.Minute, drainIdle: 200 * time.Millisecond, drainBytes: 100}
	server, client := net.Pipe()
	defer client.Close()
	c := newClampConn(server, limits)
	// Passthrough forwards the server's own deadline unchanged.
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	start := time.Now()
	if _, err := c.Read(make([]byte, 1)); err == nil || time.Since(start) > time.Second {
		t.Fatalf("passthrough deadline not honoured: %v", err)
	}
	// Authenticated clamps an absent or longer server deadline to the lifetime.
	c.authenticate()
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	start = time.Now()
	if _, err := c.Read(make([]byte, 1)); err == nil || time.Since(start) > time.Second {
		t.Fatalf("authenticated deadline: %v", err)
	}
	// Transaction ignores the server's absolute deadline and uses the idle time.
	c.beginTransaction()
	_ = c.SetReadDeadline(time.Now().Add(time.Millisecond))
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = client.Write([]byte("x"))
	}()
	if n, err := c.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("transaction mode honoured the server's deadline: %d %v", n, err)
	}
	// Drain closes the connection past the byte cap: the writer offers far more
	// than the cap and must not get it all through.
	c.beginDrain()
	written := make(chan int, 1)
	go func() {
		total := 0
		for total < 4000 {
			n, err := client.Write(bytes.Repeat([]byte("z"), 100))
			total += n
			if err != nil {
				break
			}
		}
		written <- total
	}()
	var total int
	for {
		n, err := c.Read(make([]byte, 64))
		total += n
		if err != nil {
			break
		}
	}
	if total > 100+1 {
		t.Fatalf("drain read %d bytes past a cap of 100", total)
	}
	if got := <-written; got >= 4000 {
		t.Fatalf("the writer got all %d bytes through", got)
	}
	c.endTransaction()
	if c.currentMode() != modeAuthenticated {
		t.Fatal("endTransaction should return to authenticated")
	}
	c.logout()
	if c.currentMode() != modePassthrough {
		t.Fatal("logout should return to passthrough")
	}
}

func TestMXStaysUnchangedOnPort25(t *testing.T) {
	servers, err := NewServers(Config{Hostname: "smtp.mx.example", Domains: []string{"bounce.example"}, MaxBytes: 1 << 20, MaxConcurrent: 1, ForwardTimeout: time.Second, Forwarder: &fakeForwarder{received: make(chan string, 1), release: make(chan error, 1)}},
		[]Listener{{Hostname: "smtp.mx.example", TLS: &tls.Config{Certificates: []tls.Certificate{selfSigned(t, "smtp.mx.example")}}}})
	if err != nil {
		t.Fatal(err)
	}
	server := servers[0]
	if server.DisableCHUNKING || server.ReadTimeout != 30*time.Second {
		t.Fatal("the MX server changed")
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(raw) }()
	t.Cleanup(func() { _ = server.Close() })
	env := &subEnv{t: t, addr: raw.Addr().String(), hostname: "smtp.mx.example"}
	c := env.dial()
	caps := c.ehlo()
	if !strings.Contains(caps, "CHUNKING") || strings.Contains(caps, "AUTH") {
		t.Fatalf("MX capabilities changed: %q", caps)
	}
	c.expect(523, "AUTH PLAIN "+plainIR("", testCredID, testCredSecret))
	c.startTLS()
	c.ehlo()
	// After STARTTLS the MX still has no AUTH: 504 5.7.4, as before.
	if msg := c.expect(504, "AUTH PLAIN "+plainIR("", testCredID, testCredSecret)); !strings.Contains(msg, "5.7.4") {
		t.Fatalf("%q", msg)
	}
}

func TestEachSubmissionListenerPresentsItsOwnCertificate(t *testing.T) {
	var listeners []SubmissionListener
	for _, name := range []string{"smtp.first.example", "smtp.second.example"} {
		listeners = append(listeners, SubmissionListener{Hostname: name, TLS: &tls.Config{Certificates: []tls.Certificate{selfSigned(t, name)}}, Port: 465})
	}
	servers, err := NewSubmissionServers(SubmissionConfig{Submission: newFakeSubmission()}, listeners)
	if err != nil {
		t.Fatal(err)
	}
	for i, server := range servers {
		raw, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = server.Serve(tls.NewListener(ClampListener(raw), server.TLSConfig)) }()
		t.Cleanup(func() { _ = server.Close() })
		conn, err := tls.Dial("tcp", raw.Addr().String(), &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		leaf := conn.ConnectionState().PeerCertificates[0]
		if leaf.Subject.CommonName != listeners[i].Hostname {
			t.Fatalf("listener %d presented %q", i, leaf.Subject.CommonName)
		}
	}
}

func TestSubmissionConfigurationIsValidated(t *testing.T) {
	good := SubmissionListener{Hostname: "smtp.submission.example", TLS: &tls.Config{Certificates: []tls.Certificate{selfSigned(t, "smtp.submission.example")}}, Port: 587}
	if _, err := NewSubmissionServers(SubmissionConfig{}, []SubmissionListener{good}); err == nil {
		t.Fatal("a missing Submission was accepted")
	}
	bad := good
	bad.Port = 25
	if _, err := NewSubmissionServers(SubmissionConfig{Submission: newFakeSubmission()}, []SubmissionListener{bad}); err == nil {
		t.Fatal("port 25 was accepted")
	}
	bad = good
	bad.TLS = nil
	if _, err := NewSubmissionServers(SubmissionConfig{Submission: newFakeSubmission()}, []SubmissionListener{bad}); err == nil {
		t.Fatal("a missing TLS configuration was accepted")
	}
}

type panicSubmission struct{ *fakeSubmission }

func (panicSubmission) Submit(context.Context, SubmissionMessage) (Reply, error) {
	panic("victim@example.com " + testCredSecret)
}

func TestSubmissionLogsAndFormattingAreRedacted(t *testing.T) {
	var out lockedBuffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	env := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{Submission: panicSubmission{newFakeSubmission()}}})
	c := env.ready()
	c.envelope()
	c.send(stuff(simpleBody))
	c.reply()
	eventually(t, "the panic log", func() bool { return strings.Contains(out.String(), "submission SMTP handler panic") })
	logged := out.String()
	for _, secret := range []string{testCredSecret, testCredID, "victim@example.com", "127.0.0.1", "sender@example.com"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaks %q: %s", secret, logged)
		}
	}
	cred := Credential{ID: testCredID, Secret: testCredSecret}
	msg := SubmissionMessage{Credential: cred, Body: []byte("secret body")}
	for _, got := range []string{fmt.Sprintf("%v %+v %#v %s", cred, cred, cred, cred), fmt.Sprintf("%v %+v %#v", msg, msg, msg)} {
		if strings.Contains(got, testCredSecret) || strings.Contains(got, "secret body") || strings.Contains(got, testCredID) {
			t.Fatalf("formatting leaks: %s", got)
		}
	}
}

// filler returns n decoded bytes of short CRLF-terminated lines (the server
// refuses lines over 2000 bytes).
func filler(n int) string {
	line := strings.Repeat("x", 70) + "\r\n"
	body := strings.Repeat(line, n/len(line)+1)
	return body[:n-2] + "\r\n"
}

// notAnswered sends a command after a stall and reports whether any 250 came
// back: a stalled upload must end the connection, not run the rest of the body
// as commands.
func (c *subClient) notAnswered250() bool {
	_, _ = c.conn.Write([]byte("NOOP\r\nNOOP\r\n"))
	for i := 0; i < 5; i++ {
		code, _ := c.reply()
		if code == 0 {
			return true
		}
		if code == 250 {
			return false
		}
	}
	return true
}

func TestSubmissionStalledUploadClosesTheConnection(t *testing.T) {
	limits := clampLimits{preAuth: time.Minute, idle: 300 * time.Millisecond, transaction: time.Minute, lifetime: time.Minute, drainTotal: time.Minute, drainIdle: 300 * time.Millisecond, drainBytes: 1 << 30}
	t.Run("transaction idle", func(t *testing.T) {
		env := startSubmission(t, subOptions{implicit: true, limits: &limits})
		c := env.ready()
		c.envelope()
		c.send("partial line\r\n")
		time.Sleep(900 * time.Millisecond)
		if !c.notAnswered250() {
			t.Fatal("commands inside a stalled upload were run")
		}
	})
	t.Run("drain idle", func(t *testing.T) {
		env := startSubmission(t, subOptions{implicit: true, limits: &limits, cfg: SubmissionConfig{maxBytes: 64 << 10}})
		c := env.ready()
		c.envelope()
		c.send(filler(80 << 10)) // over the limit, no terminator: refused, then drained
		time.Sleep(900 * time.Millisecond)
		if !c.notAnswered250() {
			t.Fatal("commands inside a stalled drain were run")
		}
	})
}

func TestSubmissionIdleBeforeAuthIsCutAtThePreAuthLimit(t *testing.T) {
	limits := defaultClampLimits()
	limits.preAuth = 500 * time.Millisecond
	env := startSubmission(t, subOptions{implicit: true, limits: &limits})
	c := env.dial()
	start := time.Now()
	for {
		code, _ := c.cmd("NOOP")
		if code != 250 {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("NOOP kept a pre-auth connection open")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// After AUTH the longer lifetime applies instead.
	env2 := startSubmission(t, subOptions{implicit: true, limits: &limits})
	c2 := env2.dial()
	c2.ehlo()
	c2.expect(235, "AUTH PLAIN "+plainIR("", testCredID, testCredSecret))
	time.Sleep(800 * time.Millisecond)
	c2.expect(250, "NOOP")
}

func TestSubmissionSuccessfulAuthRefundsTheAttempt(t *testing.T) {
	limiter := &fakeLimiter{}
	env := startSubmission(t, subOptions{implicit: true, cfg: SubmissionConfig{AuthLimiter: limiter}})
	c := env.dial()
	c.ehlo()
	c.expect(235, "AUTH PLAIN "+plainIR("", testCredID, testCredSecret))
	if checks, refunds := limiter.counts(); checks != 1 || refunds != 1 {
		t.Fatalf("checks=%d refunds=%d", checks, refunds)
	}
}
