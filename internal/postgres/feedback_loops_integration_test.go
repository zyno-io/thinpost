package postgres

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/bounceingest"
	"github.com/zyno-io/thinpost/internal/feedback"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

const feedbackBounceToken = "fedcba9876543210fedcba9876543210"

// feedbackDelivery inserts an accepted, bounce-addressed delivery of a message
// from sender.example and returns its message and delivery IDs.
func (f *webhookFixture) feedbackDelivery(app, address, state string) (message, delivery string) {
	f.t.Helper()
	message, delivery = f.id(), f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev,sender_address,sender_name,state)
		VALUES ($1,$2,'k'||$1,false,'secret subject',1,'sender.example','example.sender.','Sender@Sender.example','Sender Name','prepared')`, message, app)
	f.exec(`INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ($1,$2,0,$3,lower($3),split_part(lower($3),'@',2),reverse(split_part(lower($3),'@',2))||'.','to')`, message, app, address)
	hash := sha256.Sum256([]byte(f.token(delivery)))
	f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,bounce_key_version,bounce_token_hash) VALUES ($1,$2,0,'f'||$1,$3,'v1',$4)`, delivery, message, state, hash[:])
	return message, delivery
}

// feedbackSharedMessage inserts a prepared message that used the shared
// Message-ID, with one accepted, bounce-addressed delivery per address.
func (f *webhookFixture) feedbackSharedMessage(app string, addresses ...string) (message string, deliveries []string) {
	f.t.Helper()
	message = f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev,sender_address,sender_name,state,shared_message_id)
		VALUES ($1,$2,'k'||$1,false,'secret subject',$3,'sender.example','example.sender.','Sender@Sender.example','Sender Name','prepared',true)`, message, app, len(addresses))
	for index, address := range addresses {
		delivery := f.id()
		f.exec(`INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ($1,$2,$3,$4,lower($4),split_part(lower($4),'@',2),reverse(split_part(lower($4),'@',2))||'.','to')`, message, app, index, address)
		hash := sha256.Sum256([]byte(f.token(delivery)))
		f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,bounce_key_version,bounce_token_hash) VALUES ($1,$2,$3,'f'||$1,'accepted','v1',$4)`, delivery, message, index, hash[:])
		deliveries = append(deliveries, delivery)
	}
	return message, deliveries
}

// token derives a distinct 32-hex bounce token per delivery.
func (f *webhookFixture) token(delivery string) string {
	sum := sha256.Sum256([]byte("token" + delivery))
	return hex.EncodeToString(sum[:16])
}

func TestFeedbackLoopsPostgres(t *testing.T) {
	f := newWebhookFixture(t, "loops_")
	ctx := f.ctx
	app := f.app("complaints")
	native := f.create(app, "https://native.example.com/")
	pm := f.createFormat(app, "postmark", "https://pm.example.com/", nil, false, &admin.SealedWebhookAuth{Ciphertext: []byte("a"), Summary: admin.WebhookAuthSummary{Username: "u"}})
	rs := f.createFormat(app, "resend", "https://rs.example.com/", nil, true, nil)
	mg := f.createFormat(app, "mailgun", "https://mg.example.com/", []string{"complained"}, true, nil)
	mgOther := f.createFormat(app, "mailgun", "https://mg2.example.com/", []string{"opened"}, true, nil)
	daily := func(result string) int {
		return f.count(`SELECT coalesce(sum(count),0) FROM inbound_feedback_daily WHERE channel='arf' AND result=$1`, result)
	}

	t.Run("runtime grants", func(t *testing.T) {
		for _, statement := range []string{
			`UPDATE feedback_reporters SET domain='x.example'`, `TRUNCATE feedback_reporters`,
			`DELETE FROM feedback_mailbox`, `TRUNCATE feedback_mailbox`,
			`DELETE FROM inbound_feedback_daily`, `TRUNCATE inbound_feedback_daily`,
		} {
			if _, err := f.repo.DB.ExecContext(ctx, statement); err == nil {
				t.Fatalf("runtime permitted %q", statement)
			}
		}
		for _, statement := range []string{
			`SELECT * FROM feedback_mailbox`, `SELECT * FROM feedback_reporters`, `SELECT * FROM inbound_feedback_daily`,
			`DELETE FROM feedback_reporters`, `UPDATE feedback_mailbox SET token=token`, `UPDATE inbound_feedback_daily SET count=count`,
		} {
			if _, err := f.repo.DB.ExecContext(ctx, statement); err != nil {
				t.Fatalf("runtime refused %q: %v", statement, err)
			}
		}
	})

	t.Run("schema constraints", func(t *testing.T) {
		for name, statement := range map[string]string{
			"short mailbox token":     `INSERT INTO feedback_mailbox(token,token_hash) VALUES ('abc','` + strings.Repeat("\\x00", 32) + `')`,
			"uppercase mailbox token": `INSERT INTO feedback_mailbox(token,token_hash) SELECT upper(repeat('a',32)),decode(repeat('00',32),'hex')`,
			"second mailbox":          `INSERT INTO feedback_mailbox(singleton,token,token_hash) SELECT false,repeat('a',32),decode(repeat('00',32),'hex')`,
			"uppercase reporter":      `INSERT INTO feedback_reporters(domain) VALUES ('Upper.example')`,
			"short reporter":          `INSERT INTO feedback_reporters(domain) VALUES ('a.')`,
			"unknown counter channel": `INSERT INTO inbound_feedback_daily(day,channel,result,count) VALUES (current_date,'mailto','accepted',1)`,
			"unknown counter result":  `INSERT INTO inbound_feedback_daily(day,channel,result,count) VALUES (current_date,'arf','bogus',1)`,
			"negative counter":        `INSERT INTO inbound_feedback_daily(day,channel,result,count) VALUES (current_date,'arf','accepted',-1)`,
			"counter above the bound": `INSERT INTO inbound_feedback_daily(day,channel,result,count) VALUES (current_date,'arf','accepted',1000000001)`,
		} {
			if _, err := f.owner.ExecContext(ctx, statement); err == nil {
				t.Errorf("%s was accepted", name)
				f.exec(`DELETE FROM feedback_mailbox; DELETE FROM feedback_reporters; DELETE FROM inbound_feedback_daily`)
			}
		}
	})

	t.Run("an accepted complaint commits feedback, suppression, event, fan-out and counter atomically", func(t *testing.T) {
		message, delivery := f.feedbackDelivery(app, "Victim@Example.NET", "accepted")
		before := f.deliveryState(delivery)
		result, err := f.repo.Record(ctx, delivery, "arf.mail.yahoo.com")
		if err != nil || result != feedback.Accepted {
			t.Fatal(result, err)
		}
		var feedbackID, kind, channel, reporter string
		if err = f.owner.QueryRowContext(ctx, `SELECT id,kind,channel,reporter_domain FROM recipient_feedback WHERE delivery_id=$1`, delivery).Scan(&feedbackID, &kind, &channel, &reporter); err != nil ||
			kind != "spam_complaint" || channel != "arf" || reporter != "arf.mail.yahoo.com" {
			t.Fatal("feedback row", kind, channel, reporter, err)
		}
		if reason, origin, source := f.suppression(app, "victim@example.net"); reason != "spam_complaint" || origin != "recipient" || source != message {
			t.Fatal("suppression", reason, origin, source)
		}
		var outcome string
		var code int
		if err = f.owner.QueryRowContext(ctx, `SELECT outcome,smtp_code FROM webhook_events WHERE feedback_id=$1 AND id=$1`, feedbackID).Scan(&outcome, &code); err != nil || outcome != "complained" || code != 0 {
			t.Fatal("event", outcome, err)
		}
		if f.deliveryState(delivery) != before || before != "accepted" {
			t.Fatal("delivery state changed")
		}
		queued := f.queued(feedbackID)
		if len(queued) != 4 || !queued[native.ID] || !queued[pm.ID] || !queued[rs.ID] || !queued[mg.ID] || queued[mgOther.ID] {
			t.Fatal("fan-out", queued)
		}
		if daily("accepted") != 1 {
			t.Fatal("counter", daily("accepted"))
		}
		// Eight concurrent repeats change nothing but the duplicate counter.
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, recordErr := f.repo.Record(ctx, delivery, "arf.mail.yahoo.com")
				if recordErr == nil && got != feedback.Duplicate {
					recordErr = errors.New("not a duplicate: " + string(got))
				}
				errs <- recordErr
			}()
		}
		wg.Wait()
		close(errs)
		for err = range errs {
			if err != nil {
				t.Fatal("duplicate", err)
			}
		}
		if f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, delivery) != 1 || f.count(`SELECT count(*) FROM webhook_events WHERE delivery_id=$1 AND outcome='complained'`, delivery) != 1 ||
			f.count(`SELECT count(*) FROM webhook_deliveries WHERE event_id=$1`, feedbackID) != 4 || daily("duplicate") != 8 || daily("accepted") != 1 {
			t.Fatal("a duplicate created rows or miscounted")
		}
		// Every format claims and renders it without the subject or report content.
		seen := map[string]bool{}
		for i := 0; i < 4; i++ {
			claim, claimErr := f.repo.ClaimWebhook(ctx, f.id(), time.Minute)
			if claimErr != nil {
				t.Fatal(claimErr)
			}
			if claim.Event.Outcome != "complained" || claim.Event.AttemptID != "" || claim.Event.DeliveryID != delivery {
				t.Fatal("claim", claim.Event)
			}
			impl, _ := format.Lookup(claim.Format)
			req, buildErr := impl.Build(format.Input{Event: claim.Event, Context: claim.Context, Now: time.Now(), Secret: []byte(strings.Repeat("k", 64))})
			if claim.Format == format.Resend {
				req, buildErr = impl.Build(format.Input{Event: claim.Event, Context: claim.Context, Now: time.Now(), Secret: []byte("whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 24)))})
			}
			if buildErr != nil {
				t.Fatal(claim.Format, buildErr)
			}
			body := string(req.Body)
			seen[claim.Format] = strings.Contains(body, "omplain") || strings.Contains(body, "SpamComplaint")
			if strings.Contains(body, "arf.mail.yahoo.com") {
				t.Fatal(claim.Format, "leaked the reporter")
			}
		}
		for name, ok := range seen {
			if !ok {
				t.Fatal(name, "body is not a complaint")
			}
		}
		// The correlation view carries IDs and facts only.
		candidates, err := f.repo.Candidates(ctx, delivery)
		hash := sha256.Sum256([]byte(f.token(delivery)))
		if err != nil || len(candidates) != 1 {
			t.Fatal("delivery view", candidates, err)
		}
		d := candidates[0]
		if d.ID != delivery || d.AppID != app || d.MessageID != message || d.SenderDomain != "sender.example" || d.State != "accepted" || !bytes.Equal(d.BounceTokenHash, hash[:]) {
			t.Fatal("delivery view", d)
		}
		// The message ID of a legacy message correlates nothing.
		if got, err := f.repo.Candidates(ctx, message); len(got) != 0 || err != nil {
			t.Fatal("message ID matched a legacy message", got, err)
		}
		if got, err := f.repo.Candidates(ctx, f.id()); len(got) != 0 || err != nil {
			t.Fatal("unknown delivery", got, err)
		}
	})

	t.Run("a shared Message-ID correlates by the message ID only", func(t *testing.T) {
		message, deliveries := f.feedbackSharedMessage(app, "one@example.net", "two@example.net", "three@example.net")
		got, err := f.repo.Candidates(ctx, message)
		if err != nil || len(got) != 3 {
			t.Fatal("shared candidates", got, err)
		}
		for _, d := range got {
			if d.MessageID != message || d.AppID != app || d.SenderDomain != "sender.example" || d.State != "accepted" || len(d.BounceTokenHash) != 32 {
				t.Fatal("shared candidate", d)
			}
		}
		// A delivery ID of a shared message correlates nothing.
		if got, err = f.repo.Candidates(ctx, deliveries[0]); len(got) != 0 || err != nil {
			t.Fatal("delivery ID matched a shared message", got, err)
		}
	})

	t.Run("a duplicate after a console removal does not suppress again", func(t *testing.T) {
		_, delivery := f.feedbackDelivery(app, "again@example.net", "accepted")
		if got, err := f.repo.Record(ctx, delivery, "arf.mail.yahoo.com"); err != nil || got != feedback.Accepted {
			t.Fatal(got, err)
		}
		var id string
		if err := f.owner.QueryRowContext(ctx, `SELECT id FROM suppressions WHERE app_id=$1 AND address_lower='again@example.net'`, app).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if n, err := f.repo.DeleteSuppressions(ctx, "operator", app, []string{id}); err != nil || n != 1 {
			t.Fatal(n, err)
		}
		if got, err := f.repo.Record(ctx, delivery, "arf.mail.yahoo.com"); err != nil || got != feedback.Duplicate {
			t.Fatal(got, err)
		}
		if reason, _, _ := f.suppression(app, "again@example.net"); reason != "" {
			t.Fatal("a replayed report undid the operator's removal", reason)
		}
		if f.count(`SELECT count(*) FROM webhook_events WHERE delivery_id=$1`, delivery) != 1 {
			t.Fatal("second event")
		}
	})

	t.Run("precedence: a complaint outranks everything and is never downgraded", func(t *testing.T) {
		for _, tc := range []struct{ seed, origin string }{{"manual", "console"}, {"manual", "api"}, {"hard_bounce", "recipient"}, {"unsubscribe", "recipient"}, {"spam_complaint", "recipient"}} {
			address := "prec." + tc.seed + "." + tc.origin + "@example.net"
			_, delivery := f.feedbackDelivery(app, address, "accepted")
			f.exec(`INSERT INTO suppressions(id,app_id,address,address_lower,domain_lower,reason,origin) VALUES ($1,$2,$3,$3,'example.net',$4,$5)`, f.id(), app, address, tc.seed, tc.origin)
			if _, err := f.repo.Record(ctx, delivery, "arf.mail.yahoo.com"); err != nil {
				t.Fatal(err)
			}
			if reason, origin, _ := f.suppression(app, address); reason != "spam_complaint" || origin != "recipient" {
				t.Fatal(tc, reason, origin)
			}
			// A later unsubscribe on the same address never weakens it.
			if err := f.repo.RecordUnsubscribe(ctx, delivery); err != nil {
				t.Fatal(err)
			}
			if reason, _, _ := f.suppression(app, address); reason != "spam_complaint" {
				t.Fatal("downgraded", tc, reason)
			}
		}
	})

	t.Run("a delivery that is not accepted or has no recipient row records nothing but a counter", func(t *testing.T) {
		_, failed := f.feedbackDelivery(app, "failed@example.net", "failed")
		before := daily("not_delivered")
		if got, err := f.repo.Record(ctx, failed, "arf.mail.yahoo.com"); err != nil || got != feedback.NotDelivered {
			t.Fatal(got, err)
		}
		if f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, failed) != 0 || daily("not_delivered") != before+1 {
			t.Fatal("not-delivered recorded or uncounted")
		}
		if reason, _, _ := f.suppression(app, "failed@example.net"); reason != "" {
			t.Fatal("suppressed a delivery that was not accepted")
		}
		message, delivery := f.id(), f.id()
		f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'k'||$1,false,'prepared')`, message, app)
		f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ($1,$2,0,'f'||$1,'accepted')`, delivery, message)
		if got, err := f.repo.Record(ctx, delivery, "arf.mail.yahoo.com"); err != nil || got != feedback.Uncorrelated {
			t.Fatal(got, err)
		}
		if _, err := f.repo.Record(ctx, "bad", "arf.mail.yahoo.com"); err == nil {
			t.Fatal("malformed delivery accepted")
		}
	})

	t.Run("a closed gate fails every operation and writes nothing", func(t *testing.T) {
		_, delivery := f.feedbackDelivery(app, "closed@example.net", "accepted")
		f.exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
		_, recordErr := f.repo.Record(ctx, delivery, "arf.mail.yahoo.com")
		countErr := f.repo.Count(ctx, feedback.Ignored)
		_, mailboxErr := f.repo.IsMailbox(ctx, [32]byte{})
		_, reportersErr := f.repo.Reporters(ctx)
		_, deliveryErr := f.repo.Candidates(ctx, delivery)
		// The admin snapshot is a read-only transaction that works while closed.
		loops, loopsErr := f.repo.FeedbackLoops(ctx)
		f.exec(`UPDATE runtime_gate SET mode='open' WHERE singleton`)
		for _, err := range []error{recordErr, countErr, mailboxErr, reportersErr, deliveryErr} {
			if !errors.Is(err, ErrRuntimeClosed) {
				t.Fatal("operation ran under a closed gate", err)
			}
		}
		if loopsErr != nil || loops.Counts == nil {
			t.Fatal("snapshot under a closed gate", loopsErr)
		}
		if f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, delivery) != 0 {
			t.Fatal("wrote under a closed gate")
		}
	})

	t.Run("mailbox creation, rotation and reporter edits are audited with a NULL app", func(t *testing.T) {
		if mailbox, err := f.repo.FeedbackLoops(ctx); err != nil || mailbox.Mailbox != nil || len(mailbox.Reporters) != 0 {
			t.Fatal("initial state", mailbox, err)
		}
		first, err := f.repo.CreateFeedbackMailbox(ctx, "operator", false)
		if err != nil || len(first.Token) != 32 {
			t.Fatal("create", first, err)
		}
		if _, err = f.repo.CreateFeedbackMailbox(ctx, "operator", false); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("second create without rotate", err)
		}
		hash := sha256.Sum256([]byte(first.Token))
		if ok, err := f.repo.IsMailbox(ctx, hash); err != nil || !ok {
			t.Fatal("mailbox lookup", ok, err)
		}
		second, err := f.repo.CreateFeedbackMailbox(ctx, "operator", true)
		if err != nil || second.Token == first.Token {
			t.Fatal("rotate", err)
		}
		if ok, _ := f.repo.IsMailbox(ctx, hash); ok {
			t.Fatal("the rotated-out token is still a mailbox")
		}
		var rows int
		if err = f.owner.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE app_id IS NULL AND resource_id='feedback_mailbox' AND action IN ('feedback.mailbox.created','feedback.mailbox.rotated') AND details::text NOT LIKE '%'||$1||'%' AND details::text NOT LIKE '%'||$2||'%'`, first.Token, second.Token).Scan(&rows); err != nil || rows != 2 {
			t.Fatal("audit rows", rows, err)
		}

		if got, err := f.repo.SetFeedbackReporters(ctx, "operator", []string{"b.example", "arf.mail.yahoo.com"}); err != nil || strings.Join(got, ",") != "arf.mail.yahoo.com,b.example" {
			t.Fatal("set", got, err)
		}
		if got, err := f.repo.SetFeedbackReporters(ctx, "operator", []string{"arf.mail.yahoo.com", "b.example"}); err != nil || len(got) != 2 {
			t.Fatal("identical set", got, err)
		}
		if f.count(`SELECT count(*) FROM audit_events WHERE action='feedback.reporters.updated' AND app_id IS NULL`) != 1 {
			t.Fatal("identical reporter set audited")
		}
		if got, err := f.repo.SetFeedbackReporters(ctx, "operator", []string{"arf.mail.yahoo.com"}); err != nil || len(got) != 1 {
			t.Fatal("shrink", got, err)
		}
		for _, bad := range [][]string{{"Upper.example"}, {"a.example", "a.example"}, {"nodot"}, make([]string, 33)} {
			if _, err := f.repo.SetFeedbackReporters(ctx, "operator", bad); !errors.Is(err, admin.ErrInvalidInput) {
				t.Fatal("accepted", bad, err)
			}
		}
		domains, err := f.repo.Reporters(ctx)
		if err != nil || len(domains) != 1 || domains[0] != "arf.mail.yahoo.com" {
			t.Fatal("reporters", domains, err)
		}
		loops, err := f.repo.FeedbackLoops(ctx)
		if err != nil || loops.Mailbox == nil || loops.Mailbox.Token != second.Token || len(loops.Counts) == 0 {
			t.Fatal("snapshot", loops, err)
		}
		for _, c := range loops.Counts {
			if (c.Channel != "arf" && c.Channel != "one_click") || c.Count <= 0 || len(c.Day) != 10 {
				t.Fatal("count row", c)
			}
		}
	})

	t.Run("end to end through the bounce ingest dispatch with a DKIM-signed ARF report", func(t *testing.T) {
		const reporter = "arf.mail.yahoo.com"
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		records := map[string][]string{"sel._domainkey." + reporter: {"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)}}
		svc := &bounceingest.Service{Repository: f.repo, Objects: nopObjects{}, Feedback: &feedback.Service{Repository: f.repo, Budget: time.Second,
			LookupTXT: func(_ context.Context, name string) ([]string, error) {
				if r, ok := records[name]; ok {
					return r, nil
				}
				return nil, errors.New("no such host")
			}}}
		mailbox, err := f.repo.CreateFeedbackMailbox(ctx, "operator", true)
		if err != nil {
			t.Fatal(err)
		}
		_, delivery := f.feedbackDelivery(app, "e2e@example.net", "accepted")
		build := func(original string) []byte {
			message := "From: \"Yahoo! Mail AntiSpam Feedback\" <feedback@arf.mail.yahoo.com>\r\nTo: x@mx.example.net\r\nSubject: FW\r\nDate: Wed, 11 Nov 2026 12:05:00 +0000\r\nMIME-Version: 1.0\r\n" +
				"Content-Type: multipart/report; report-type=feedback-report; boundary=\"b\"\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nabuse report\r\n" +
				"--b\r\nContent-Type: message/feedback-report\r\n\r\nFeedback-Type: abuse\r\nVersion: 1\r\n" +
				"--b\r\nContent-Type: message/rfc822\r\n\r\n" + original + "\r\nbody\r\n--b--\r\n"
			var out bytes.Buffer
			if err := dkim.Sign(&out, strings.NewReader(message), &dkim.SignOptions{Domain: reporter, Selector: "sel", Signer: key, HeaderKeys: []string{"From", "To", "Subject", "Date", "Content-Type"}}); err != nil {
				t.Fatal(err)
			}
			return out.Bytes()
		}
		original := "Return-Path: <b+" + f.token(delivery) + "@mx.example.net>\r\nFrom: Sender <sender@sender.example>\r\nMessage-ID: <" + delivery + "@sender.example>\r\n"
		send := func(token string, body []byte) error { return svc.Forward(ctx, token, f.id(), body) }
		if err := send(mailbox.Token, build(original)); err != nil {
			t.Fatal(err)
		}
		if reason, origin, _ := f.suppression(app, "e2e@example.net"); reason != "spam_complaint" || origin != "recipient" {
			t.Fatal("e2e suppression", reason, origin)
		}
		// Return-Path of another delivery: counted uncorrelated, acknowledged, nothing recorded.
		_, other := f.feedbackDelivery(app, "other@example.net", "accepted")
		conflict := strings.Replace(original, f.token(delivery), f.token(other), 1)
		uncorrelated := daily("uncorrelated")
		if err := send(mailbox.Token, build(conflict)); err != nil || daily("uncorrelated") != uncorrelated+1 {
			t.Fatal("conflict", err)
		}
		if reason, _, _ := f.suppression(app, "other@example.net"); reason != "" {
			t.Fatal("conflicting Return-Path suppressed")
		}
		// An unsigned report with a correct original is counted unauthenticated.
		unauth := daily("unauthenticated")
		if err := send(mailbox.Token, []byte(strings.Replace(string(build(original)), "DKIM-Signature", "X-Removed", 1))); err != nil || daily("unauthenticated") != unauth+1 {
			t.Fatal("unauthenticated", err)
		}
		// The rotated-out token is an unknown bounce recipient.
		if _, err = f.repo.CreateFeedbackMailbox(ctx, "operator", true); err != nil {
			t.Fatal(err)
		}
		if err := send(mailbox.Token, build(original)); err == nil {
			t.Fatal("a rotated-out token was accepted")
		}
		// A closed gate: Edge answers 451 (an error), nothing is recorded.
		mailbox, _ = f.repo.FeedbackLoopsToken(ctx)
		_, again := f.feedbackDelivery(app, "gate@example.net", "accepted")
		gated := strings.Replace(original, delivery, again, 2)
		gated = strings.Replace(gated, f.token(delivery), f.token(again), 1)
		f.exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
		err = send(mailbox.Token, build(gated))
		f.exec(`UPDATE runtime_gate SET mode='open' WHERE singleton`)
		if err == nil || f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, again) != 0 {
			t.Fatal("closed gate", err)
		}
		if err = send(mailbox.Token, build(gated)); err != nil || f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, again) != 1 {
			t.Fatal("retry after reopen", err)
		}
		// A shared Message-ID: without a Return-Path a multi-recipient message is
		// uncorrelated; the Return-Path token picks the reporting recipient.
		message, shared := f.feedbackSharedMessage(app, "s1@example.net", "s2@example.net")
		sharedOriginal := "From: Sender <sender@sender.example>\r\nMessage-ID: <" + message + "@sender.example>\r\n"
		uncorrelated = daily("uncorrelated")
		if err = send(mailbox.Token, build(sharedOriginal)); err != nil || daily("uncorrelated") != uncorrelated+1 {
			t.Fatal("shared ID without a Return-Path", err)
		}
		if err = send(mailbox.Token, build("Return-Path: <b+"+f.token(shared[1])+"@mx.example.net>\r\n"+sharedOriginal)); err != nil {
			t.Fatal(err)
		}
		if f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, shared[1]) != 1 || f.count(`SELECT count(*) FROM recipient_feedback WHERE delivery_id=$1`, shared[0]) != 0 {
			t.Fatal("the Return-Path did not pick the reporting recipient")
		}
		// The delivery-ID form is uncorrelated for a shared message.
		uncorrelated = daily("uncorrelated")
		if err = send(mailbox.Token, build("Return-Path: <b+"+f.token(shared[0])+"@mx.example.net>\r\nFrom: Sender <sender@sender.example>\r\nMessage-ID: <"+shared[0]+"@sender.example>\r\n")); err != nil || daily("uncorrelated") != uncorrelated+1 {
			t.Fatal("delivery-ID form for a shared message", err)
		}
	})
}

type nopObjects struct{}

func (nopObjects) PutMIME(context.Context, string, []byte) error {
	return errors.New("unexpected object write")
}

// FeedbackLoopsToken is a test helper returning the current mailbox token.
func (r *Repository) FeedbackLoopsToken(ctx context.Context) (admin.FeedbackMailbox, error) {
	loops, err := r.FeedbackLoops(ctx)
	if err != nil || loops.Mailbox == nil {
		return admin.FeedbackMailbox{}, errors.New("no mailbox")
	}
	return *loops.Mailbox, nil
}

// A one-click unsubscribe counts accepted and duplicate in the same transaction.
func TestOneClickIsCounted(t *testing.T) {
	f := newWebhookFixture(t, "oneclick_")
	app := f.app("oneclick")
	_, delivery := f.feedbackDelivery(app, "oc@example.net", "accepted")
	count := func(result string) int {
		return f.count(`SELECT coalesce(sum(count),0) FROM inbound_feedback_daily WHERE channel='one_click' AND result=$1`, result)
	}
	if err := f.repo.RecordUnsubscribe(f.ctx, delivery); err != nil || count("accepted") != 1 {
		t.Fatal("first", err, count("accepted"))
	}
	if err := f.repo.RecordUnsubscribe(f.ctx, delivery); err != nil || count("accepted") != 1 || count("duplicate") != 1 {
		t.Fatal("repeat", err)
	}
	if err := f.repo.RecordUnsubscribe(f.ctx, f.id()); err == nil || count("accepted")+count("duplicate") != 2 {
		t.Fatal("a missing delivery counted or succeeded", err)
	}
}
