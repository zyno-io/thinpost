package format

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemoveForgedResultsKeepsTheBodyByteExact(t *testing.T) {
	const us = "mx.example.com"
	cases := map[string]struct{ in, want string }{
		"last field of the block": {
			"From: a@example.org\r\nAuthentication-Results: mx.example.com; dkim=pass\r\n\r\nbody\r\n",
			"From: a@example.org\r\n\r\nbody\r\n"},
		"LF only": {
			"From: a@example.org\nAuthentication-Results: mx.example.com; dkim=pass\n\nbody\n",
			"From: a@example.org\n\nbody\n"},
		"only field": {
			"Authentication-Results: mx.example.com; dkim=pass\r\n\r\nbody",
			"\r\nbody"},
		"no blank line": {
			"From: a@example.org\r\nAuthentication-Results: mx.example.com; x=y",
			"From: a@example.org\r\n"},
		"folded and surrounded": {
			"A: 1\r\nAuthentication-Results: mx.example.com;\r\n\tdkim=pass\r\nB: 2\r\n\r\nAuthentication-Results: mx.example.com; body\r\n",
			"A: 1\r\nB: 2\r\n\r\nAuthentication-Results: mx.example.com; body\r\n"},
		"other servers kept": {
			"Authentication-Results: other.example; spf=pass\r\n\r\nbody",
			"Authentication-Results: other.example; spf=pass\r\n\r\nbody"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, _ := removeForgedResults([]byte(tc.in), us)
			if string(out) != tc.want {
				t.Fatalf("got %q want %q", out, tc.want)
			}
		})
	}
	// Nothing to remove: the input itself, not a copy.
	in := []byte("From: a@example.org\r\n\r\nbody")
	if out, n := removeForgedResults(in, us); n != 0 || &out[0] != &in[0] {
		t.Fatal("an unchanged message must not be copied")
	}
}

func TestRemoveForgedResultsHasNoHeaderSizeCap(t *testing.T) {
	for _, pad := range []int{70 << 10, 2 << 20} {
		raw := "Authentication-Results: mx.example.com; dkim=pass; dmarc=pass\r\nX-Pad: " + strings.Repeat("a", pad) + "\r\n\r\nbody\r\n"
		out, n := removeForgedResults([]byte(raw), "mx.example.com")
		if n != 1 || bytes.Contains(out, []byte("mx.example.com; dkim=pass")) || !strings.HasSuffix(string(out), "\r\n\r\nbody\r\n") {
			t.Fatalf("pad %d: forged field kept (removed %d)", pad, n)
		}
	}
	// With no blank line at all the whole message is headers.
	raw := "X-Pad: " + strings.Repeat("a", 100<<10) + "\r\nAuthentication-Results: mx.example.com; dkim=pass\r\n"
	if out, n := removeForgedResults([]byte(raw), "mx.example.com"); n != 1 || bytes.Contains(out, []byte("dkim=pass")) {
		t.Fatal("forged field kept in a message that is all headers")
	}
}

func TestAuthservIDForms(t *testing.T) {
	for in, want := range map[string]string{
		" mx.example.com; dmarc=pass":                 "mx.example.com",
		"(x) mx.example.com; dmarc=pass":              "mx.example.com",
		" (a (nested) c)\r\n\t(d) MX.Example.COM.; x": "mx.example.com",
		`"mx.example.com" 1; spf=pass`:                "mx.example.com",
		"(unterminated mx.example.com":                "",
		"":                                            "",
	} {
		if got := authservID(in); got != want {
			t.Errorf("authservID(%q) = %q, want %q", in, got, want)
		}
	}
	for _, field := range []string{
		"Authentication-Results: (x) mx.example.com; dmarc=pass\r\n",
		"Authentication-Results: MX.example.com.; dmarc=pass\r\n",
	} {
		raw := "From: a@b.example\r\n" + field + "\r\nbody"
		if out, n := removeForgedResults([]byte(raw), "mx.example.com"); n != 1 || string(out) != "From: a@b.example\r\n\r\nbody" {
			t.Errorf("not removed: %q", field)
		}
	}
}

func TestHeaderBoundAppliesToStoredBytesNotTheRenderedMessage(t *testing.T) {
	// A header block just under 64 KiB stays parsed once Thinpost's own fields
	// are prepended; one over it is omitted.
	pad := func(total int) string {
		return "From: a@b.example\r\nSubject: s\r\nX-Pad: " + strings.Repeat("a", total-len("From: a@b.example\r\nSubject: s\r\nX-Pad: ")-2) + "\r\n\r\nbody\r\n"
	}
	for size, omitted := range map[int]bool{maxHeaderBlock - 100: false, maxHeaderBlock + 100: true} {
		_, doc := buildInbound(t, inboundInput(pad(size)))
		in := inboundOf(doc)
		if got := len(in["omitted"].([]any)) == 1; got != omitted {
			t.Fatalf("header block of %d bytes: omitted=%v, want %v", size, got, omitted)
		}
		if !omitted && in["headers"].(map[string]any)["subject"] != "s" {
			t.Fatal("headers not parsed")
		}
	}
}

func TestRawRoundTripsByteExactExceptRemovedFields(t *testing.T) {
	raw := "Authentication-Results: smtp.example.com; spf=pass\r\nFrom: a@b.example\r\nSubject: s\r\n\r\nbody\x00\xff\r\n"
	_, doc := buildInbound(t, inboundInput(raw))
	got := rawOf(t, doc)
	i := strings.Index(got, "From: a@b.example")
	if i < 0 || got[i:] != "From: a@b.example\r\nSubject: s\r\n\r\nbody\x00\xff\r\n" {
		t.Fatalf("raw differs from the stored message: %q", got)
	}
}

func TestSplicedStringsMatchEncodingJSON(t *testing.T) {
	var all strings.Builder
	for c := 0; c < 0x80; c++ {
		all.WriteByte(byte(c))
	}
	for _, s := range []string{all.String(), "plain", "", "  and   and é日本 and \"quote\" \\ ", strings.Repeat("\x01", 100)} {
		want, err := marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		writeJSONString(&out, s)
		if `"`+out.String()+`"` != string(want) || jsonEscapedLen(s) != out.Len() {
			t.Fatalf("%q: got %q want %q (len %d vs %d)", s, out.String(), want, jsonEscapedLen(s), out.Len())
		}
	}
}

func bigMessage(size int, ctrl bool) []byte {
	var b strings.Builder
	b.WriteString("From: a@example.org\r\nTo: b@example.com\r\nSubject: x\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=BB\r\n\r\n--BB\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	text := strings.Repeat("hello world ", (1<<20-100)/12)
	if ctrl {
		text = strings.Repeat("\x01", 1<<20-100)
	}
	b.WriteString(text)
	b.WriteString("\r\n--BB\r\nContent-Type: text/html; charset=utf-8\r\n\r\n")
	b.WriteString(text)
	b.WriteString("\r\n--BB\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment\r\n\r\n")
	data := make([]byte, (size-b.Len()-20)*3/4)
	_, _ = rand.Read(data)
	enc := base64.StdEncoding.EncodeToString(data)
	for i := 0; i < len(enc); i += 76 {
		b.WriteString(enc[i:min(i+76, len(enc))])
		b.WriteString("\r\n")
	}
	b.WriteString("--BB--\r\n")
	return []byte(b.String())
}

const forgedLine = "Authentication-Results: mx.example.com; dmarc=pass\r\n"

// hostilePartHeader is a multipart message whose first part has one huge header
// field, about 10 MiB, plain or folded into short lines.
func hostilePartHeader(folded bool) []byte {
	var b strings.Builder
	b.WriteString("From: a@example.org\r\nTo: b@example.com\r\nSubject: x\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=BB\r\n\r\n--BB\r\nContent-Type: text/plain; charset=utf-8\r\nX-Pad: ")
	target := 10<<20 - 200
	if folded {
		for b.Len() < target {
			b.WriteString(strings.Repeat("a", 70))
			b.WriteString("\r\n ")
		}
	} else {
		b.WriteString(strings.Repeat("a", target-b.Len()))
	}
	b.WriteString("\r\n\r\nhello\r\n--BB--\r\n")
	return []byte(b.String())
}

func peakOf(t *testing.T, raw []byte) (extra int64, body []byte) {
	t.Helper()
	in := Input{Event: Event{ID: "evt1"}, AppID: "app", Secret: []byte(strings.Repeat("s", 48)), Now: time.Now(),
		Inbound: &InboundInput{ID: "00000000000000000000000000000001", ReceivedAt: time.Now(), Recipient: "b@example.com", Raw: raw, Receiver: "mx.example.com"}}
	old := debug.SetGCPercent(1)
	defer debug.SetGCPercent(old)
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	base := sample[0].Value.Uint64()
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		for {
			select {
			case <-stop:
				close(done)
				return
			default:
			}
			metrics.Read(s)
			if v := s[0].Value.Uint64(); v > peak.Load() {
				peak.Store(v)
			}
		}
	}()
	req, err := thinpostInboundFormat{}.Build(in)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	return int64(peak.Load()) - int64(base), req.Body
}

// RenderPeakBytes is what the dispatcher reserves for a render of a message of
// the given stored size; see its documentation. This test measures real peaks
// (message already resident, as in the dispatcher) and requires the formula to
// cover them: the worst case of bodies of control characters (JSON escapes them
// at six bytes each), and hostile messages with a forged Authentication-Results
// field, a header block just under the bound, and a part header of about 10 MiB.
func TestRenderPeakStaysWithinTheBudgetFormula(t *testing.T) {
	const size = 10 << 20
	big := func(ctrl bool, forged bool) []byte {
		raw := bigMessage(size, ctrl)
		if forged {
			raw = append([]byte(forgedLine), raw...)
		}
		return raw
	}
	hostile := func(folded bool) []byte { return append([]byte(forgedLine), hostilePartHeader(folded)...) }
	underBound := []byte("X-Pad: " + strings.Repeat("a", maxHeaderBlock-100) + "\r\n" + forgedLine + "\r\n" + string(bigMessage(size-maxHeaderBlock, true)))
	for name, raw := range map[string][]byte{
		"plain": big(false, false), "control characters": big(true, false), "control characters, forged field": big(true, true),
		"forged field, part header": hostile(false), "forged field, folded part header": hostile(true), "header block under the bound": underBound,
	} {
		extra, body := peakOf(t, raw)
		limit := RenderPeakBytes(len(raw)) - int64(len(raw))
		t.Logf("%s: message %.2f MiB, payload %.2f MiB, render peak above the message %.2f MiB, formula %.2f MiB", name, float64(len(raw))/(1<<20), float64(len(body))/(1<<20), float64(extra)/(1<<20), float64(limit)/(1<<20))
		if extra > limit {
			t.Errorf("%s: render peak %d bytes exceeds the formula's %d", name, extra, limit)
		}
	}
}

func TestBareCRCannotHideAForgedField(t *testing.T) {
	for _, raw := range []string{
		"From: a@b.example\r\nX-Foo: a\rAuthentication-Results: mx.example.com; dmarc=pass\r\nSubject: s\r\n\r\nbody\r\n",
		"From: a@b.example\r\nX-Foo: a\r (c) Authentication-Results: x\rAuthentication-Results:(c) MX.example.com.; dmarc=pass\r\n\r\nbody\r\n",
	} {
		out, n := removeForgedResults([]byte(raw), "mx.example.com")
		if n != 1 || bytes.Contains(out, []byte("dmarc=pass")) || !strings.HasSuffix(string(out), "\r\n\r\nbody\r\n") {
			t.Fatalf("forged field survived: %q", out)
		}
		_, doc := buildInbound(t, inboundInput(strings.ReplaceAll(raw, "mx.example.com", "smtp.example.com")))
		if strings.Contains(rawOf(t, doc), "dmarc=pass\r\nSubject") || strings.Count(rawOf(t, doc), "smtp.example.com;") != 1 {
			t.Fatal("the rendered message kept a forged field")
		}
	}
	// Another server's field after a bare CR is left alone, and a value too long
	// to read an id from fails closed.
	if out, n := removeForgedResults([]byte("X: a\rAuthentication-Results: other.example; spf=pass\r\n\r\nb"), "mx.example.com"); n != 0 || !strings.Contains(string(out), "other.example") {
		t.Fatal("another server's field was removed")
	}
	long := "Authentication-Results: (" + strings.Repeat("c", claimWindow+10) + ") mx.example.com; x=y\r\n\r\nb"
	if _, n := removeForgedResults([]byte(long), "mx.example.com"); n != 1 {
		t.Fatal("a field whose id cannot be read must be removed")
	}
}

func TestOversizedPartHeaderOmitsBodiesAndKeepsRaw(t *testing.T) {
	for _, folded := range []bool{false, true} {
		raw := hostilePartHeader(folded)
		_, doc := buildInbound(t, inboundInput(string(raw)))
		in := inboundOf(doc)
		omitted := in["omitted"].([]any)
		if len(omitted) != 2 || omitted[0] != "text" || omitted[1] != "html" {
			t.Fatalf("folded=%v omitted %v", folded, omitted)
		}
		if in["headers"].(map[string]any)["subject"] != "x" || !strings.Contains(rawOf(t, doc), "--BB--") {
			t.Fatal("headers and raw must still be delivered")
		}
	}
	// A normal multipart message is unaffected, and so are delimiter-like lines
	// that are followed by a blank line soon.
	_, doc := buildInbound(t, inboundInput(inboundAlternative))
	if len(inboundOf(doc)["omitted"].([]any)) != 0 {
		t.Fatal("a normal message lost its bodies")
	}
}
