package controlsession

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"regexp"
	"strings"
	"testing"
	"time"
)

type deliveredPart struct {
	mediaType string
	decoded   []byte
}

// deliveredParts splits a delivered message into its leaf parts, decoded.
func deliveredParts(t *testing.T, wire []byte) []deliveredPart {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	var out []deliveredPart
	get := func(h map[string][]string, k string) string {
		if v := h[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	var walk func(h map[string][]string, body io.Reader)
	walk = func(h map[string][]string, body io.Reader) {
		mt, params, _ := mime.ParseMediaType(get(h, "Content-Type"))
		if strings.HasPrefix(mt, "multipart/") {
			mr := multipart.NewReader(body, params["boundary"])
			for {
				p, err := mr.NextRawPart()
				if err != nil {
					return
				}
				walk(p.Header, p)
			}
		}
		raw, _ := io.ReadAll(body)
		switch strings.ToLower(get(h, "Content-Transfer-Encoding")) {
		case "quoted-printable":
			raw, _ = io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
		case "base64":
			raw, _ = base64.StdEncoding.DecodeString(strings.NewReplacer("\r", "", "\n", "").Replace(string(raw)))
		}
		out = append(out, deliveredPart{mt, raw})
	}
	walk(m.Header, m.Body)
	return out
}

// TestSMTPTrackingPipelineWithPostgres: SMTP mail gets open and click tracking
// from the app's defaults, end to end. Each recipient has its own tokens and
// rows, links redirect and record against the budget, the console shows the
// engagement, attachments arrive byte for byte, DKIM verifies at the MX (the
// fake MX checks it), and a revoked mapping object holds the delivery.
func TestSMTPTrackingPipelineWithPostgres(t *testing.T) {
	r := newSubmissionRig(t, rigOptions{tracking: true})
	stop := r.start("controller", r.lossy)
	defer stop()
	r.waitAvailable()
	r.exec(`UPDATE apps SET default_track_opens=true,default_track_clicks=true WHERE id=$1`, r.app)

	attachment := make([]byte, 4000)
	if _, err := rand.Read(attachment); err != nil {
		t.Fatal(err)
	}
	var encoded strings.Builder
	for enc := base64.StdEncoding.EncodeToString(attachment); len(enc) > 0; {
		n := min(76, len(enc))
		encoded.WriteString(enc[:n] + "\r\n")
		enc = enc[n:]
	}
	message := func(subject string, rcpt ...string) string {
		return "From: Printer <" + e2eFrom + ">\r\nTo: " + strings.Join(rcpt, ", ") + "\r\nSubject: " + subject + "\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=\"MIX\"\r\n\r\n--MIX\r\n" +
			"Content-Type: multipart/alternative; boundary=\"ALT\"\r\n\r\n" +
			"--ALT\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nplain https://plain.example.org/p\r\n" +
			"--ALT\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
			"<html><body><a href=3D\"https://shop.example.org/a?x=3D1\">one</a> <a href=3D\"https://shop.example.org/b\">two</a></body></html>\r\n--ALT--\r\n" +
			"--MIX\r\nContent-Type: application/pdf; name=\"a.pdf\"\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
			encoded.String() + "--MIX--\r\n"
	}
	c := r.dial()
	if code, msg := c.auth(r.cred, r.secret); code != 235 {
		t.Fatalf("AUTH: %d %q", code, msg)
	}
	code, msg := c.send(e2eFrom, []string{e2eRcpt, "second@example.net"}, message("Tracked over SMTP", e2eRcpt, "second@example.net"))
	id := queuedID(t, code, msg)
	if err := r.preparer.PrepareNext(r.ctx); err != nil {
		t.Fatal(err)
	}
	var delivered [][]byte
	for len(delivered) < 2 {
		select {
		case wire := <-r.received:
			delivered = append(delivered, wire)
		case err := <-r.failures:
			t.Fatal("the remote MX refused the message:", err)
		case <-r.ctx.Done():
			t.Fatal("nothing was delivered")
		}
	}
	if r.count(`SELECT count(*) FROM deliveries WHERE message_id=$1 AND prepared_format=2`, id) != 2 ||
		r.count(`SELECT count(*) FROM tracking_objects t JOIN deliveries d ON d.id=t.delivery_id WHERE d.message_id=$1`, id) != 2 ||
		r.count(`SELECT count(*) FROM tracking_links l JOIN deliveries d ON d.id=l.delivery_id WHERE d.message_id=$1 AND l.kind='click'`, id) != 4 ||
		r.count(`SELECT count(*) FROM tracking_links l JOIN deliveries d ON d.id=l.delivery_id WHERE d.message_id=$1 AND l.kind='open'`, id) != 2 {
		t.Fatal("tracking rows are not one object, two clicks and one pixel per delivery")
	}
	linkRE := regexp.MustCompile(`https://tracking\.example\.com(/t/([oc])/v3/[^"'\s>]+)`)
	var htmls []string
	for _, wire := range delivered {
		parts := deliveredParts(t, wire)
		if len(parts) != 3 || string(parts[0].decoded) != "plain https://plain.example.org/p" || !bytes.Equal(parts[2].decoded, attachment) || !bytes.Contains(wire, []byte(encoded.String()+"--MIX--")) {
			t.Fatal("text part or attachment changed")
		}
		html := string(parts[1].decoded)
		if strings.Contains(html, "shop.example.org") || len(linkRE.FindAllString(html, -1)) != 3 {
			t.Fatalf("html: %s", html)
		}
		htmls = append(htmls, html)
	}
	if htmls[0] == htmls[1] || bytes.Equal(delivered[0], delivered[1]) {
		t.Fatal("recipients share tracking tokens")
	}
	for _, link := range linkRE.FindAllStringSubmatch(htmls[0], -1) {
		if strings.Contains(htmls[1], link[1]) {
			t.Fatal("a link is shared between recipients")
		}
	}

	// Click and open the first recipient's links.
	visit := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.tracking.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	var clicked []string
	for _, link := range linkRE.FindAllStringSubmatch(htmls[0], -1) {
		w := visit(link[1])
		switch link[2] {
		case "c":
			if w.Code != http.StatusFound {
				t.Fatal("click did not redirect", w.Code)
			}
			clicked = append(clicked, w.Header().Get("Location"))
		case "o":
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/gif" {
				t.Fatal("pixel", w.Code)
			}
		}
	}
	if len(clicked) != 2 || clicked[0] != "https://shop.example.org/a?x=1" || clicked[1] != "https://shop.example.org/b" {
		t.Fatalf("destinations %q", clicked)
	}
	var used int
	if err := r.db.QueryRowContext(r.ctx, `SELECT tracking_quota_used FROM apps WHERE id=$1`, r.app).Scan(&used); err != nil || used != 3 {
		t.Fatalf("budget used %d %v", used, err)
	}
	if r.count(`SELECT count(*) FROM tracking_events e JOIN deliveries d ON d.id=e.delivery_id WHERE d.message_id=$1 AND e.kind='click'`, id) != 2 ||
		r.count(`SELECT count(*) FROM webhook_events WHERE message_id=$1 AND outcome='clicked'`, id) != 2 ||
		r.count(`SELECT count(*) FROM webhook_events WHERE message_id=$1 AND outcome='opened'`, id) != 1 {
		t.Fatal("events were not recorded for the clicks and the open")
	}
	detail, err := r.repo.GetMessage(r.ctx, "operator", r.app, id)
	if err != nil {
		t.Fatal(err)
	}
	engaged := 0
	for _, d := range detail.Deliveries {
		if d.Tracking != nil && d.Tracking.Clicks == 2 && d.Tracking.Opens == 1 {
			engaged++
		}
	}
	if engaged != 1 {
		t.Fatal("the console does not show the engagement of one delivery")
	}
	eventually(t, "both deliveries to be accepted", func() bool {
		return r.count(`SELECT count(*) FROM deliveries WHERE message_id=$1 AND state='accepted'`, id) == 2
	})

	// A revoked mapping object holds the delivery instead of sending untracked.
	r.exec(`CREATE FUNCTION test_delay_deliveries() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.retry_after := clock_timestamp() + interval '1 hour'; RETURN NEW; END $$`)
	r.exec(`CREATE TRIGGER test_delay BEFORE INSERT ON deliveries FOR EACH ROW EXECUTE FUNCTION test_delay_deliveries()`)
	code, msg = c.send(e2eFrom, []string{e2eRcpt}, message("Tracked then revoked", e2eRcpt))
	heldID := queuedID(t, code, msg)
	if err = r.preparer.PrepareNext(r.ctx); err != nil {
		t.Fatal(err)
	}
	if r.count(`SELECT count(*) FROM tracking_objects t JOIN deliveries d ON d.id=t.delivery_id WHERE d.message_id=$1`, heldID) != 1 {
		t.Fatal("no tracking object for the second message")
	}
	r.exec(`UPDATE tracking_objects SET revoked_at=clock_timestamp() WHERE delivery_id IN (SELECT id FROM deliveries WHERE message_id=$1)`, heldID)
	r.exec(`UPDATE deliveries SET retry_after=clock_timestamp() WHERE message_id=$1`, heldID)
	eventually(t, "the delivery to be held", func() bool {
		return r.count(`SELECT count(*) FROM deliveries WHERE message_id=$1 AND state='tracking_held'`, heldID) == 1
	})
	select {
	case <-r.received:
		t.Fatal("a delivery with a revoked mapping object was sent")
	case err = <-r.failures:
		t.Fatal(err)
	case <-time.After(300 * time.Millisecond):
	}
}
