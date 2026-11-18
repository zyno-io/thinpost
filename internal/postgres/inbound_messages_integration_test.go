package postgres

import (
	"errors"
	"testing"

	"github.com/zyno-io/thinpost/internal/admin"
)

func TestInboundMessageListDetailAndRetention(t *testing.T) {
	f := newInboundFixture(t)
	empty, err := f.repo.ListInboundMessages(f.ctx, "operator", f.app, "", 50)
	if err != nil || len(empty.Messages) != 0 || empty.NextCursor != "" {
		t.Fatal("empty list", empty, err)
	}
	if _, err = f.repo.ListInboundMessages(f.ctx, "operator", "ffffffffffffffffffffffffffffffff", "", 50); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("unknown app", err)
	}
	first := f.record(f.app)
	first.Recipients = []string{"reply+one@mail.example.com", "other@mail.example.com"}
	f.commit(first)
	second := f.record(f.app)
	second.Subject = "Second"
	f.commit(second)
	f.exec(`UPDATE inbound_messages SET received_at=received_at-interval '1 minute' WHERE id=$1`, first.ID)

	page, err := f.repo.ListInboundMessages(f.ctx, "operator", f.app, "", 50)
	if err != nil || len(page.Messages) != 2 || page.Messages[0].ID != second.ID || page.Messages[1].ID != first.ID {
		t.Fatal("newest first", page, err)
	}
	row := page.Messages[1]
	if row.Recipient != "reply+one@mail.example.com" || row.RecipientCount != 2 || row.SizeBytes != 100 || row.From == nil || row.From.Address != "alice@example.org" ||
		row.Subject == nil || *row.Subject != "Re: hello" || row.EnhancedProtection || row.ContentExpired || row.SPF != "pass" || row.DKIM != "fail" || row.DMARC != "fail" || row.Correlation.Matched ||
		row.Webhook.Total != 2 || row.Webhook.Pending != 2 || row.Webhook.Status != admin.InboundWebhookPending {
		t.Fatalf("row %+v", row)
	}
	small, err := f.repo.ListInboundMessages(f.ctx, "operator", f.app, "", 1)
	if err != nil || len(small.Messages) != 1 || small.NextCursor == "" {
		t.Fatal("paging", small, err)
	}
	older, err := f.repo.ListInboundMessages(f.ctx, "operator", f.app, small.NextCursor, 1)
	if err != nil || len(older.Messages) != 1 || older.Messages[0].ID != first.ID || older.NextCursor != "" {
		t.Fatal("second page", older, err)
	}
	if other, err := f.repo.ListInboundMessages(f.ctx, "operator", f.other, "", 50); err != nil || len(other.Messages) != 0 {
		t.Fatal("another app's list shows this app's mail", other, err)
	}
	if _, err = f.repo.ListInboundMessages(f.ctx, "operator", f.app, "not-a-cursor", 50); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("bad cursor", err)
	}

	// Enhanced Protection withholds the subject in SQL.
	f.exec(`UPDATE inbound_messages SET enhanced_protection=true WHERE id=$1`, first.ID)
	page, _ = f.repo.ListInboundMessages(f.ctx, "operator", f.app, "", 50)
	if row = page.Messages[1]; !row.EnhancedProtection || row.Subject != nil {
		t.Fatal("EP subject leaked", row)
	}

	// The webhook summary follows delivery states; cancelled ones are not counted.
	f.exec(`UPDATE webhook_deliveries w SET state='delivered' FROM webhook_events e WHERE e.id=w.event_id AND e.inbound_id=$1 AND e.inbound_recipient_index=0`, first.ID)
	f.exec(`UPDATE webhook_deliveries w SET state='failed' FROM webhook_events e WHERE e.id=w.event_id AND e.inbound_id=$1 AND e.inbound_recipient_index=1`, first.ID)
	detail, err := f.repo.GetInboundMessage(f.ctx, "operator", f.app, first.ID)
	if err != nil || detail.Message.Webhook.Status != admin.InboundWebhookPartial || detail.Message.Webhook.Delivered != 1 || detail.Message.Webhook.Failed != 1 {
		t.Fatal("webhook summary", detail.Message.Webhook, err)
	}
	f.exec(`UPDATE webhook_deliveries w SET state='cancelled' FROM webhook_events e WHERE e.id=w.event_id AND e.inbound_id=$1 AND e.inbound_recipient_index=1`, first.ID)
	if detail, _ = f.repo.GetInboundMessage(f.ctx, "operator", f.app, first.ID); detail.Message.Webhook.Status != admin.InboundWebhookDelivered || detail.Message.Webhook.Total != 1 {
		t.Fatal("cancelled counted", detail.Message.Webhook)
	}

	// Detail: envelope, authentication (ARC never evaluated), retention.
	if detail.Message.Subject != nil || len(detail.Envelope.Recipients) != 2 || detail.Envelope.Recipients[1] != "other@mail.example.com" || detail.Envelope.MailFrom != "alice@example.org" ||
		detail.Envelope.ClientIP != "192.0.2.7" || detail.Envelope.Helo != "mx.example.org" || detail.Envelope.TLSVersion != "TLS 1.3" || detail.MessageIDHeader != "<m@example.org>" ||
		detail.Authentication.SPF.Result != "pass" || detail.Authentication.SPF.Identity != "mailfrom" || len(detail.Authentication.DKIM) != 1 || detail.Authentication.DMARC.Policy != "reject" ||
		detail.Authentication.ARC.Evaluated || detail.Correlation.Matched || detail.ContentExpiresAt.Before(detail.Message.ReceivedAt) {
		t.Fatalf("detail %+v", detail)
	}
	if _, err = f.repo.GetInboundMessage(f.ctx, "operator", f.other, first.ID); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("another app's detail", err)
	}
	if _, err = f.repo.GetInboundMessage(f.ctx, "operator", f.app, "ffffffffffffffffffffffffffffffff"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("unknown detail", err)
	}

	// Correlation in the detail.
	message, deliveries := f.feedbackSharedMessage(f.app, "alice@example.org", "bob@example.org")
	f.exec(`UPDATE inbound_messages SET correlated_message_id=$2,correlated_delivery_id=$3,correlation_header='in_reply_to',from_matches_recipient=true WHERE id=$1`, first.ID, message, deliveries[0])
	detail, _ = f.repo.GetInboundMessage(f.ctx, "operator", f.app, first.ID)
	if c := detail.Correlation; !c.Matched || c.MessageID != message || c.DeliveryID != deliveries[0] || c.Header != "in_reply_to" || c.FromMatchesRecipient == nil || !*c.FromMatchesRecipient {
		t.Fatal("correlation", c)
	}
	if page, _ = f.repo.ListInboundMessages(f.ctx, "operator", f.app, "", 50); !page.Messages[1].Correlation.Matched || page.Messages[1].Correlation.MessageID != message {
		t.Fatal("list correlation")
	}

	// Past content_expires_at (database clock) the list says so.
	f.exec(`UPDATE inbound_messages SET content_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID)
	if page, _ = f.repo.ListInboundMessages(f.ctx, "operator", f.app, "", 50); !page.Messages[1].ContentExpired || page.Messages[0].ContentExpired {
		t.Fatal("expired flag")
	}
}

func TestAuthorizeInboundReadAuditsEnhancedProtectionAndRefusesExpired(t *testing.T) {
	f := newInboundFixture(t)
	rec := f.record(f.app)
	f.commit(rec)
	audits := func(action string) int {
		return f.count(`SELECT count(*) FROM audit_events WHERE action=$1 AND resource_id=$2 AND app_id=$3`, action, rec.ID, f.app)
	}
	key, protected, in, err := f.repo.AuthorizeInboundRead(f.ctx, "operator", f.app, rec.ID, InboundBodyReadAction)
	if err != nil || protected || key != rec.ObjectKey || in.ID != rec.ID || in.Receiver != "smtp.mx.example" || in.ClientIP != "192.0.2.7" || len(in.Recipients) != 1 || in.SPF != "pass" || len(in.Authentication) == 0 {
		t.Fatal("authorize", key, in, err)
	}
	if audits(InboundBodyReadAction) != 0 {
		t.Fatal("a non-EP read was audited")
	}
	f.exec(`UPDATE inbound_messages SET enhanced_protection=true WHERE id=$1`, rec.ID)
	for _, action := range []string{InboundBodyReadAction, InboundRawReadAction} {
		if _, protected, _, err = f.repo.AuthorizeInboundRead(f.ctx, "operator", f.app, rec.ID, action); err != nil || !protected || audits(action) != 1 {
			t.Fatal("EP read not audited once", action, err)
		}
	}
	if _, _, _, err = f.repo.AuthorizeInboundRead(f.ctx, "operator", f.other, rec.ID, InboundBodyReadAction); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("another app read the message", err)
	}
	if _, _, _, err = f.repo.AuthorizeInboundRead(f.ctx, "operator", f.app, rec.ID, "message.body.read"); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("unknown action", err)
	}
	f.exec(`UPDATE inbound_messages SET content_expires_at=clock_timestamp() WHERE id=$1`, rec.ID)
	if _, _, _, err = f.repo.AuthorizeInboundRead(f.ctx, "operator", f.app, rec.ID, InboundRawReadAction); !errors.Is(err, admin.ErrContentExpired) {
		t.Fatal("expired content was authorized", err)
	}
	if audits(InboundRawReadAction) != 1 {
		t.Fatal("a refused read was audited")
	}
}

func TestInboundWebhookDeliveriesOfOneMessage(t *testing.T) {
	f := newInboundFixture(t)
	a, b := f.record(f.app), f.record(f.app)
	f.commit(a)
	f.commit(b)
	page, err := f.repo.ListInboundWebhookDeliveries(f.ctx, "operator", f.app, a.ID, "", 50)
	if err != nil || len(page.Deliveries) != 1 || page.Deliveries[0].InboundID != a.ID || page.Deliveries[0].MessageID != "" || page.Deliveries[0].Outcome != "inbound_received" || page.Deliveries[0].State != "queued" {
		t.Fatal("deliveries", page, err)
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE action='webhook.delivery.list' AND resource_id=$1`, a.ID) != 1 {
		t.Fatal("the list was not audited")
	}
	if _, err = f.repo.ListInboundWebhookDeliveries(f.ctx, "operator", f.other, a.ID, "", 50); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("another app", err)
	}
}

func TestOperationsCountsInboundToday(t *testing.T) {
	f := newInboundFixture(t)
	f.commit(f.record(f.app))
	f.commit(f.record(f.app))
	f.exec(`INSERT INTO inbound_daily(app_id,day,result,count) VALUES ($1,(clock_timestamp() AT TIME ZONE 'UTC')::date,'tempfail_limit',3),($1,(clock_timestamp() AT TIME ZONE 'UTC')::date-1,'accepted',50)`, f.other)
	ops, err := f.repo.Operations(f.ctx)
	if err != nil || ops.Inbound.Accepted != 2 || ops.Inbound.TempfailLimit != 3 || ops.Inbound.Duplicate != 0 || len(ops.Inbound.Day) != 10 {
		t.Fatalf("inbound counters %+v %v", ops.Inbound, err)
	}
}
