package postgres

import (
	"strings"
	"testing"
)

func TestInboundWebhookFormatsAndColumns(t *testing.T) {
	f := newWebhookFixture(t, "inb_formats_")
	app, other := f.app("one"), f.app("two")
	insert := func(format string, secret bool) error {
		var ciphertext any
		if secret {
			ciphertext = []byte("sealed")
		}
		_, err := f.owner.ExecContext(f.ctx, `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,format,secret_ciphertext) VALUES ($1,$2,'https://hooks.example.com/',$3,$4)`, f.id(), app, format, ciphertext)
		return err
	}
	for _, ok := range []struct {
		format string
		secret bool
	}{{"thinpost", true}, {"resend", true}, {"mailgun", true}, {"postmark", false}, {"thinpost_inbound", true}, {"postmark_inbound", false}} {
		if err := insert(ok.format, ok.secret); err != nil {
			t.Fatal("refused", ok.format, ok.secret, err)
		}
	}
	for _, bad := range []struct {
		format string
		secret bool
	}{{"postmark", true}, {"postmark_inbound", true}, {"thinpost_inbound", false}, {"thinpost", false}, {"bogus", true}} {
		if insert(bad.format, bad.secret) == nil {
			t.Fatal("accepted", bad.format, bad.secret)
		}
	}
	hash := `decode(repeat('00',32),'hex')`
	message := f.id()
	f.exec(`INSERT INTO inbound_messages(id,app_id,domain,object_key,size_bytes,fingerprint,content_expires_at,client_ip,helo,tls_version,tls_cipher,receiver,authentication) VALUES ($1,$2,'mail.example.com','inbound/v1/0000000000/'||$1,10,`+hash+`,now(),'192.0.2.1','mx.example.org','TLS1.3','TLS_AES_128_GCM_SHA256','a@mail.example.com','{"spf":{"result":"pass"},"dkim":[],"dmarc":{"domain":"example.org"},"arc":false}')`, message, app)
	bad := func(what, query string, args ...any) {
		t.Helper()
		if _, err := f.owner.ExecContext(f.ctx, query, args...); err == nil {
			t.Fatal("accepted:", what)
		}
	}
	bad("a non-object authentication", `UPDATE inbound_messages SET authentication='[]'`)
	bad("an oversized authentication", `UPDATE inbound_messages SET authentication=jsonb_build_object('x',(SELECT string_agg(md5(i::text),'') FROM generate_series(1,1000) i))`)
	bad("a long HELO", `UPDATE inbound_messages SET helo=repeat('a',256)`)
	// Correlation is app-scoped.
	otherMessage := f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection) VALUES ($1,$2,'k'||$1,true)`, otherMessage, other)
	bad("a correlation to another app's message", `UPDATE inbound_messages SET correlated_message_id=$1,correlation_header='in_reply_to'`, otherMessage)
	mine := f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection) VALUES ($1,$2,'k'||$1,true)`, mine, app)
	f.exec(`UPDATE inbound_messages SET correlated_message_id=$1,correlation_header='references'`, mine)

	// The event-lane claim can use its own queue index.
	conn, err := f.owner.Conn(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(f.ctx, `SET enable_seqscan=off`); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(f.ctx, `EXPLAIN SELECT 1 FROM webhook_deliveries w WHERE w.lane='event' AND w.state='queued' AND w.next_attempt_at<=clock_timestamp() ORDER BY w.next_attempt_at,w.event_id LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
	}
	if !strings.Contains(plan.String(), "webhook_deliveries_event_queue_idx") {
		t.Fatal("the outbound claim does not use the event-lane index:", plan.String())
	}
}
