package rawmime

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

// splitPrepared separates the DKIM field, the other header fields and the body.
func splitPrepared(t *testing.T, out []byte) (sig, rest, body string) {
	t.Helper()
	s, err := Scan(out)
	if err != nil {
		t.Fatalf("prepared output does not scan: %v", err)
	}
	f, _, err := nextField(out[:s.HeaderEnd], 0)
	if err != nil || !nameIs(out, f, "DKIM-Signature") {
		t.Fatalf("no leading DKIM-Signature")
	}
	return string(out[:f.end]), string(out[f.end:s.HeaderEnd]), string(out[s.BodyStart:])
}

func fieldNames(head string) []string {
	var names []string
	for _, line := range strings.Split(head, "\r\n") {
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			names = append(names, strings.ToLower(line[:strings.IndexByte(line, ':')]))
		}
	}
	return names
}

func count(names []string, name string) int {
	n := 0
	for _, x := range names {
		if x == name {
			n++
		}
	}
	return n
}

func TestPrepareSurgery(t *testing.T) {
	raw := string(fixture(t, "basic.eml"))
	out, in := prepared(t, raw, "")
	sig, rest, body := splitPrepared(t, out)
	if !strings.HasPrefix(sig, "DKIM-Signature:") || !strings.Contains(sig, "d=example.com") || !strings.Contains(sig, "s=sel") || !strings.Contains(sig, "a=rsa-sha256") || !strings.Contains(sig, "c=relaxed/relaxed") {
		t.Fatalf("sig %q", sig)
	}
	names := fieldNames(rest)
	for _, gone := range []string{"bcc", "received", "x-thinpost-enhanced-protection", "dkim-signature"} {
		if count(names, gone) != 0 {
			t.Errorf("%s not dropped: %v", gone, names)
		}
	}
	if count(names, "message-id") != 1 || count(names, "date") != 1 || count(names, "from") != 1 {
		t.Fatalf("%v", names)
	}
	if !strings.Contains(rest, "Message-ID: <"+in.MessageID+"@example.com>\r\n") || strings.Contains(rest, "orig@example.com") {
		t.Fatalf("message-id: %q", rest)
	}
	// The Message-ID takes the removed original's position: after From, before Date.
	if want := []string{"from", "to", "cc", "subject", "message-id", "date", "mime-version", "content-type"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("order %v", names)
	}
	if !strings.Contains(rest, "Date: Tue, 17 Nov 2026 10:00:00 +0000\r\n") || body != "Hello there.\r\nSecond line.\r\n" {
		t.Fatalf("date or body: %q %q", rest, body)
	}
	if strings.Contains(string(out), "hidden@example.org") {
		t.Fatal("Bcc leaked")
	}
}

func TestPrepareInsertsFromMessageIDAndDate(t *testing.T) {
	raw := string(fixture(t, "nofrom.eml"))
	out, in := prepared(t, raw, "bounce@example.com")
	_, rest, body := splitPrepared(t, out)
	want := "From: <bounce@example.com>\r\nMessage-ID: <" + in.MessageID + "@example.com>\r\nDate: Fri, 20 Nov 2026 12:00:00 +0000\r\nTo: bob@example.net\r\n"
	if !strings.HasPrefix(rest, want) {
		t.Fatalf("got %q", rest)
	}
	if body != "body 8bit caf\xc3\xa9\r\n" {
		t.Fatalf("body %q", body)
	}
}

func TestPrepareMessageIDAfterFromWhenNone(t *testing.T) {
	out, in := prepared(t, "Subject: x\r\nFrom: al@example.com\r\nTo: b@example.net\r\nDate: Tue, 17 Nov 2026 10:00:00 +0000\r\n\r\nbody", "")
	_, rest, _ := splitPrepared(t, out)
	want := "Subject: x\r\nFrom: al@example.com\r\nMessage-ID: <" + in.MessageID + "@example.com>\r\nTo: b@example.net\r\n"
	if !strings.HasPrefix(rest, want) {
		t.Fatalf("got %q", rest)
	}
}

func TestPrepareMessageIDAtFirstOriginalPosition(t *testing.T) {
	out, in := prepared(t, "Message-ID: <one@example.com>\r\nFrom: al@example.com\r\nMessage-ID: <two@example.com>\r\nTo: b@example.net\r\nDate: Tue, 17 Nov 2026 10:00:00 +0000\r\n\r\nbody", "")
	_, rest, _ := splitPrepared(t, out)
	want := "Message-ID: <" + in.MessageID + "@example.com>\r\nFrom: al@example.com\r\nTo: b@example.net\r\n"
	if !strings.HasPrefix(rest, want) || count(fieldNames(rest), "message-id") != 1 {
		t.Fatalf("got %q", rest)
	}
}

func TestPrepareKeepsOriginalFromBytesAndFolds(t *testing.T) {
	out, _ := prepared(t, "From: Alice\r\n <al@example.com>\nTo: b@example.net\n\nbody", "")
	_, rest, _ := splitPrepared(t, out)
	if !strings.HasPrefix(rest, "From: Alice\r\n <al@example.com>\r\nMessage-ID:") {
		t.Fatalf("got %q", rest)
	}
}

func TestPrepareRewritesFromAndDropsExtras(t *testing.T) {
	raw := "From: A <a@example.org>\r\nSender: s@example.org\r\nFrom: second@example.org\r\nTo: b@example.net\r\nSubject: x\r\nDKIM-Signature: v=1; a=rsa-sha256; d=example.org; s=k; h=subject; bh=AA; b=BB\r\nReturn-Path: <r@example.org>\r\nARC-Seal: i=1\r\nARC-Message-Signature: i=1\r\nARC-Authentication-Results: i=1\r\nAuthentication-Results: mx\r\nResent-Bcc: x@example.net\r\nX-Thinpost-Anything: 1\r\nX-Other: kept\r\n\r\nbody"
	s := mustScan(t, raw)
	d, err := DecideFrom(s, "", Policy{Mode: FromAddress, Address: "noreply@example.net"}, verifiedSet("example.net"))
	if err != nil {
		t.Fatal(err)
	}
	in := PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("0", 32), Date: acceptedAt, Selector: "sel", Signer: testKey(t)}
	out, err := Prepare([]byte(raw), in)
	if err != nil {
		t.Fatal(err)
	}
	_, rest, _ := splitPrepared(t, out)
	if strings.Join(fieldNames(rest), ",") != "from,message-id,date,to,subject,x-other" {
		t.Fatalf("%q", rest)
	}
	if !strings.HasPrefix(rest, "From: <noreply@example.net>\r\n") {
		t.Fatalf("%q", rest)
	}
}

func TestPrepareDKIMSignatureRefinedDrop(t *testing.T) {
	sig := func(h string) string {
		return "DKIM-Signature: v=1; a=rsa-sha256; d=example.com; s=k;\r\n h=" + h + ";\r\n bh=AA; b=BB\r\n"
	}
	cases := []struct {
		name string
		sig  string
		keep bool
	}{
		{"covers only unchanged fields", sig("from:to:subject"), true},
		{"names message-id", sig("from:to:message-id"), false},
		{"names received", sig("from:received"), false},
		{"names bcc folded", sig("from:to:\r\n b\r\n cc"), false},
		{"names x-thinpost-", sig("from:X-Thinpost-Enhanced-Protection"), false},
		{"names date but present", sig("from:date"), true},
		{"unparsable h=", "DKIM-Signature: v=1; a=rsa-sha256; d=example.com; s=k; bh=AA; b=BB\r\n", false},
		{"empty h=", sig(""), false},
		{"case insensitive", sig("From:TO:Message-Id"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := "From: al@example.com\r\nTo: b@example.net\r\nDate: Tue, 17 Nov 2026 10:00:00 +0000\r\n" + c.sig + "\r\nbody"
			out, _ := prepared(t, raw, "")
			_, rest, _ := splitPrepared(t, out)
			if kept := count(fieldNames(rest), "dkim-signature") == 1; kept != c.keep {
				t.Fatalf("kept=%v\n%s", kept, rest)
			}
		})
	}
	// Date missing: a signature naming date is dropped; a one naming From only is kept.
	out, _ := prepared(t, "From: al@example.com\r\n"+sig("from:date")+"\r\nbody", "")
	if _, rest, _ := splitPrepared(t, out); count(fieldNames(rest), "dkim-signature") != 0 {
		t.Fatalf("date-naming signature kept: %q", rest)
	}
	out, _ = prepared(t, "From: al@example.com\r\n"+sig("from")+"\r\nbody", "")
	if _, rest, _ := splitPrepared(t, out); count(fieldNames(rest), "dkim-signature") != 1 {
		t.Fatalf("signature dropped: %q", rest)
	}
}

func TestPrepareDroppedSignatureInvalidatesSignaturesCoveringSignatures(t *testing.T) {
	sig := func(id, h string) string {
		return "DKIM-Signature: v=1; a=rsa-sha256; d=example.com; s=k; h=" + h + "; bh=AA; b=" + id + "\r\n"
	}
	head := "From: al@example.com\r\nTo: b@example.net\r\nDate: Tue, 17 Nov 2026 10:00:00 +0000\r\n"
	cases := []struct {
		name string
		sigs string
		kept []string // b= markers that survive
	}{
		// The older signature covers Message-ID and goes; the later one covers
		// DKIM-Signature, whose field set just changed, so it goes too.
		{"later covers dkim-signature", sig("OLD", "from:message-id") + sig("NEW", "from:dkim-signature"), nil},
		{"covered case-insensitively", sig("OLD", "from:message-id") + sig("NEW", "from:DKIM-Signature"), nil},
		{"unrelated signatures stay", sig("OLD", "from:message-id") + sig("MID", "from:to") + sig("NEW", "from:subject"), []string{"MID", "NEW"}},
		{"nothing dropped, covers dkim-signature stays", sig("A", "from:to") + sig("B", "from:dkim-signature"), []string{"A", "B"}},
		{"only dropped signature goes", sig("OLD", "from:to:message-id"), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := head + c.sigs + "\r\nbody"
			out, in := prepared(t, raw, "")
			if size, err := PreparedSize([]byte(raw), in); err != nil || size.Total != len(out) {
				t.Fatalf("PreparedSize %v %v, len(Prepare) %d", size, err, len(out))
			}
			_, rest, _ := splitPrepared(t, out)
			for _, id := range []string{"OLD", "MID", "NEW", "A", "B"} {
				want := false
				for _, k := range c.kept {
					want = want || k == id
				}
				if got := strings.Contains(rest, "b="+id+"\r\n"); got != want {
					t.Errorf("%s: kept=%v want %v\n%s", id, got, want, rest)
				}
			}
		})
	}
}

func TestPrepareDropsSenderOnlyWhenStripped(t *testing.T) {
	out, _ := prepared(t, "From: al@example.com\r\nSender: bot@example.com\r\nDate: Tue, 17 Nov 2026 10:00:00 +0000\r\n\r\nb", "")
	if _, rest, _ := splitPrepared(t, out); count(fieldNames(rest), "sender") != 1 {
		t.Fatalf("%q", rest)
	}
	out, _ = prepared(t, "From: al@example.com\r\nSender: bot@example.org\r\nDate: Tue, 17 Nov 2026 10:00:00 +0000\r\n\r\nb", "")
	if _, rest, _ := splitPrepared(t, out); count(fieldNames(rest), "sender") != 0 {
		t.Fatalf("%q", rest)
	}
}

func TestPrepareBareLFToCRLF(t *testing.T) {
	out, _ := prepared(t, string(fixture(t, "barelf_quoted_from.eml")), "")
	if bytes.Contains(bytes.ReplaceAll(out, []byte("\r\n"), nil), []byte("\n")) || bytes.Contains(bytes.ReplaceAll(out, []byte("\r\n"), nil), []byte("\r")) {
		t.Fatalf("bare line endings remain: %q", out)
	}
	_, _, body := splitPrepared(t, out)
	if body != "line one\r\nline two\r\nline three\r\n" {
		t.Fatalf("%q", body)
	}
}

func TestPreparedSizeEqualsPrepare(t *testing.T) {
	fixtures := []struct{ name, mailFrom string }{
		{"basic.eml", ""}, {"barelf_quoted_from.eml", ""}, {"signed.eml", ""}, {"nofrom.eml", "bounce@example.com"},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			raw := fixture(t, f.name)
			out, in := prepared(t, string(raw), f.mailFrom)
			size, err := PreparedSize(raw, in)
			if err != nil {
				t.Fatal(err)
			}
			if size.Total != len(out) {
				t.Fatalf("PreparedSize %d, len(Prepare) %d", size.Total, len(out))
			}
			sig, rest, _ := splitPrepared(t, out)
			if size.DKIM != len(sig) || size.Headers != len(rest) || !size.Fits() {
				t.Fatalf("%+v vs sig %d headers %d", size, len(sig), len(rest))
			}
			// A sizing signer of the same width gives the same size.
			sizing := in
			sizing.Signer, _ = SizingSigner(2048)
			if again, err := PreparedSize(raw, sizing); err != nil || again != size {
				t.Fatalf("sizing signer: %+v %v", again, err)
			}
		})
	}
}

func TestPreparedSizeFits(t *testing.T) {
	if (Size{Total: MaxPreparedMessage - SizeSlack, Headers: 1}).Fits() != true || (Size{Total: MaxPreparedMessage - SizeSlack + 1}).Fits() {
		t.Fatal("total limit")
	}
	if (Size{Headers: 60 << 10}).Fits() != true || (Size{Headers: 50 << 10, DKIM: 10<<10 + 1}).Fits() {
		t.Fatal("header limit")
	}
}

func TestPrepareWithRewrittenFromSizeMatches(t *testing.T) {
	raw := []byte("From: " + strings.Repeat("Longname ", 40) + "<al@example.org>\r\nTo: b@example.net\r\n\r\nbody\nmore\n")
	s := mustScan(t, string(raw))
	d, err := DecideFrom(s, "", Policy{Mode: FromDomain, Domain: "example.net", OnlyUnauthorized: true}, verifiedSet("example.net"))
	if err != nil {
		t.Fatal(err)
	}
	in := PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("f", 32), Date: acceptedAt, Selector: "sel", Signer: testKey(t)}
	out, err := Prepare(raw, in)
	if err != nil {
		t.Fatal(err)
	}
	size, err := PreparedSize(raw, in)
	if err != nil || size.Total != len(out) {
		t.Fatalf("%+v %d %v", size, len(out), err)
	}
}

func TestPrepareDeterministicBelowDKIM(t *testing.T) {
	raw := string(fixture(t, "barelf_quoted_from.eml"))
	a, _ := prepared(t, raw, "")
	b, _ := prepared(t, raw, "")
	_, restA, bodyA := splitPrepared(t, a)
	_, restB, bodyB := splitPrepared(t, b)
	if restA != restB || bodyA != bodyB {
		t.Fatal("output below the DKIM field differs")
	}
}

func TestAssembleAllocatesOnlyTheOutput(t *testing.T) {
	raw := fixture(t, "basic.eml")
	s := mustScan(t, string(raw))
	d, err := DecideFrom(s, "", Policy{Mode: FromOff}, verifiedSet("example.com"))
	if err != nil {
		t.Fatal(err)
	}
	in := PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a", 32), Date: acceptedAt, Selector: "sel", Signer: testKey(t)}
	g, err := in.check(raw)
	if err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(50, func() {
		if _, _, err := assemble(raw, &in, g, 4096); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 1 {
		t.Fatalf("assemble allocates %v times, want 1", allocs)
	}
	out, err := Prepare(raw, in)
	if err != nil || cap(out) != len(out) {
		t.Fatalf("cap %d len %d %v", cap(out), len(out), err)
	}
}

func TestPrepareDKIMVerifies(t *testing.T) {
	for _, name := range []string{"basic.eml", "barelf_quoted_from.eml", "signed.eml"} {
		t.Run(name, func(t *testing.T) {
			out, _ := prepared(t, string(fixture(t, name)), "")
			verifications, err := dkim.VerifyWithOptions(bytes.NewReader(out), &dkim.VerifyOptions{
				LookupTXT: func(domain string) ([]string, error) {
					if domain != "sel._domainkey.example.com" {
						return nil, errors.New("unexpected lookup")
					}
					return []string{txtRecord(t)}, nil
				},
			})
			// signed.eml keeps one foreign (invalid) signature after ours.
			wantCount := 1
			if name == "signed.eml" {
				wantCount = 2
			}
			if err != nil || len(verifications) != wantCount || verifications[0].Err != nil {
				t.Fatalf("%v %+v", err, verifications)
			}
			if verifications[0].Domain != "example.com" {
				t.Fatalf("%+v", verifications[0])
			}
		})
	}
}

func TestPrepareSignedHeaderKeys(t *testing.T) {
	out, _ := prepared(t, "From: al@example.com\r\nTo: a@example.net\r\nTo: b@example.net\r\nSubject: x\r\nX-Other: y\r\nList-Id: <l.example.com>\r\n\r\nb", "")
	s := mustScan(t, string(out))
	keys := SignedHeaderKeys(out[:s.HeaderEnd])
	if strings.Join(keys, ",") != "From,To,Subject,Date,Message-ID,List-Id,From" {
		t.Fatalf("%v", keys)
	}
	sig, _, _ := splitPrepared(t, out)
	unfolded := strings.NewReplacer("\r\n", "", "\t", "", " ", "").Replace(sig)
	if !strings.Contains(unfolded, "h=From:To:Subject:Date:Message-ID:List-Id:From;") {
		t.Fatalf("h= %q", unfolded)
	}
}

func TestPrepareEd25519(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	raw := fixture(t, "basic.eml")
	s := mustScan(t, string(raw))
	d, _ := DecideFrom(s, "", Policy{Mode: FromOff}, verifiedSet("example.com"))
	in := PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a", 32), Date: acceptedAt, Selector: "sel", Signer: key}
	out, err := Prepare(raw, in)
	if err != nil {
		t.Fatal(err)
	}
	if size, err := PreparedSize(raw, in); err != nil || size.Total != len(out) {
		t.Fatalf("%+v %v", size, err)
	}
}

func maxLineLen(b []byte) int {
	longest := 0
	for _, line := range bytes.Split(b, []byte("\r\n")) {
		longest = max(longest, len(line))
	}
	return longest
}

func TestPrepareWorstCaseSignatureFitsReservation(t *testing.T) {
	var b strings.Builder
	b.WriteString("From: al@" + strings.Repeat("a", 60) + ".example.com\r\n")
	for i := 0; i < MaxFields-1; i++ {
		b.WriteString("Content-Transfer-Encoding: 7bit\r\n")
	}
	b.WriteString("\r\nbody")
	raw := []byte(b.String())
	s := mustScan(t, b.String())
	if s.HeaderEnd == 0 {
		t.Fatal("no headers")
	}
	domain := strings.Repeat("a", 60) + ".example.com"
	d, err := DecideFrom(s, "", Policy{Mode: FromOff}, verifiedSet(domain))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := SizingSigner(8192)
	if err != nil {
		t.Fatal(err)
	}
	in := PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a", 32), Date: acceptedAt, Selector: strings.Repeat("s", 63), Signer: signer}
	out, err := Prepare(raw, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := maxLineLen(out); got > MaxLine {
		t.Fatalf("output line of %d octets", got)
	}
	if size, err := PreparedSize(raw, in); err != nil || size.Total != len(out) {
		t.Fatalf("%+v %d %v", size, len(out), err)
	}
}

func TestSignedHeaderKeysOncePerName(t *testing.T) {
	head := []byte("From: a@example.com\r\nTo: a@example.net\r\nTo: b@example.net\r\nContent-Transfer-Encoding: 7bit\r\nContent-Transfer-Encoding: 7bit\r\n")
	if got := strings.Join(SignedHeaderKeys(head), ","); got != "From,To,Content-Transfer-Encoding,From" {
		t.Fatal(got)
	}
}

func TestSizeSinkOverflowIsAnError(t *testing.T) {
	raw := fixture(t, "barelf_quoted_from.eml")
	s := mustScan(t, string(raw))
	d, _ := DecideFrom(s, "", Policy{Mode: FromOff}, verifiedSet("example.com"))
	in := PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a", 32), Date: acceptedAt, Selector: "sel", Signer: testKey(t)}
	in.Scan = &Scanned{}
	*in.Scan = *s
	in.Scan.BareLF = 0 // stale: the body has bare LFs
	if _, err := Prepare(raw, in); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%v", err)
	}
	in.Scan.BareLF = s.BareLF + 5
	if _, err := Prepare(raw, in); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%v", err)
	}
	// Hand-built positions outside the message must be refused, not panic.
	for _, mutate := range []func(*Scanned){
		func(x *Scanned) { x.BareLF = -1000 },
		func(x *Scanned) { x.HeaderEnd = -1 },
		func(x *Scanned) { x.BareLF = len(raw) },
	} {
		*in.Scan = *s
		mutate(in.Scan)
		if _, err := Prepare(raw, in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Prepare: %v", err)
		}
		if _, err := PreparedSize(raw, in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("PreparedSize: %v", err)
		}
	}
}

func TestSizingSignerBounds(t *testing.T) {
	for _, bits := range []int{-1, 0, 1, 1023, 8193} {
		if _, err := SizingSigner(bits); err == nil {
			t.Errorf("%d accepted", bits)
		}
	}
	for _, bits := range []int{1024, 2048, 8192} {
		if _, err := SizingSigner(bits); err != nil {
			t.Errorf("%d: %v", bits, err)
		}
	}
}

func TestPrepareEd25519Verifies(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	raw := fixture(t, "basic.eml")
	s := mustScan(t, string(raw))
	d, _ := DecideFrom(s, "", Policy{Mode: FromOff}, verifiedSet("example.com"))
	in := PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a", 32), Date: acceptedAt, Selector: "ed", Signer: key}
	out, err := Prepare(raw, in)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(out), &dkim.VerifyOptions{
		LookupTXT: func(string) ([]string, error) {
			return []string{"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
		},
	})
	if err != nil || len(vs) != 1 || vs[0].Err != nil {
		t.Fatalf("%v %+v", err, vs)
	}
}

func TestPrepareRejectsMismatchedInput(t *testing.T) {
	raw := fixture(t, "basic.eml")
	s := mustScan(t, string(raw))
	d, _ := DecideFrom(s, "", Policy{Mode: FromOff}, verifiedSet("example.com"))
	in := PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a", 32), Date: acceptedAt, Selector: "sel", Signer: testKey(t)}
	if _, err := Prepare(raw[:len(raw)-1], in); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%v", err)
	}
	bad := in
	bad.MessageID = "<x>"
	if _, err := Prepare(raw, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%v", err)
	}
	bad = in
	bad.Signer = nil
	if _, err := Prepare(raw, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%v", err)
	}
}

func FuzzScanPrepare(f *testing.F) {
	for _, name := range []string{"basic.eml", "barelf_quoted_from.eml", "signed.eml", "nofrom.eml"} {
		f.Add(fixtureBytes(name), "bounce@example.com", uint8(0))
	}
	f.Add([]byte("From: a@example.org\n\nx"), "", uint8(1))
	f.Add([]byte("From: a@example.org, b@example.org\nSender: x@example.org\n\nx\ny"), "", uint8(2))
	f.Add([]byte("A: b\rc\r\n\r\n"), "", uint8(0))
	f.Add([]byte("\r\n\r\n\r\n"), "x@example.com", uint8(1))
	f.Add([]byte("DKIM-Signature: h=from;\r\n h=to\r\nFrom: =?UTF-8?Q?x?= <a@example.com>\r\n\r\n"), "", uint8(0))
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	verified := verifiedSet("example.com", "example.net")
	policies := []Policy{
		{Mode: FromOff},
		{Mode: FromDomain, Domain: "example.net", KeepOrigin: true, Address: "noreply@example.net", OnlyUnauthorized: true},
		{Mode: FromAddress, Address: "noreply@example.net"},
	}
	known := []error{ErrInvalid, ErrContentNotSupported, ErrTooLarge, ErrFromNotPermitted, ErrMisconfigured, ErrSigning}
	isKnown := func(err error) bool {
		for _, k := range known {
			if err == k {
				return true
			}
		}
		return false
	}
	f.Fuzz(func(t *testing.T, raw []byte, mailFrom string, mode uint8) {
		s, err := Scan(raw)
		if err != nil {
			if !isKnown(err) {
				t.Fatalf("unexpected error %v", err)
			}
			return
		}
		d, err := DecideFrom(s, mailFrom, policies[int(mode)%len(policies)], verified)
		if err != nil {
			if !isKnown(err) {
				t.Fatalf("unexpected error %v", err)
			}
			return
		}
		in := PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("c", 32), Date: acceptedAt, Selector: "sel", Signer: key}
		out, err := Prepare(raw, in)
		if err != nil {
			if !isKnown(err) {
				t.Fatalf("unexpected error %v", err)
			}
			return
		}
		size, err := PreparedSize(raw, in)
		if err != nil || size.Total != len(out) {
			t.Fatalf("size %+v vs %d: %v", size, len(out), err)
		}
		// The prepared message is itself a valid, normalized message.
		again, err := Scan(out)
		if err != nil {
			if err == ErrTooLarge {
				return
			}
			t.Fatalf("prepared output rejected: %v", err)
		}
		if again.BareLF != 0 || bytes.Contains(out[:again.HeaderEnd], []byte("\n\n")) || !again.HasDate || !again.HasMessageID || again.FromFields != 1 || again.From == nil {
			t.Fatalf("prepared output facts: %+v", again)
		}
		if strings.ToLower(again.From.Address[strings.LastIndexByte(again.From.Address, '@')+1:]) != d.Domain {
			t.Fatal("effective domain changed")
		}
		for _, name := range []string{"Bcc", "Resent-Bcc", "Received", "Return-Path", "Authentication-Results"} {
			for pos := 0; pos < again.HeaderEnd; {
				f, _, _ := nextField(out[:again.HeaderEnd], pos)
				if nameIs(out, f, name) {
					t.Fatalf("%s survived", name)
				}
				pos = f.end
			}
		}
		if bytes.Count(out, []byte("\n")) != bytes.Count(out, []byte("\r\n")) {
			t.Fatal("bare LF in output")
		}
	})
}

func fixtureBytes(name string) []byte {
	b, err := readFixture(name)
	if err != nil {
		panic(err)
	}
	return b
}

// A held message keeps the client's own Date when it is finally sent, and the
// hold option header never leaves Thinpost.
func TestPrepareKeepsClientDateAndStripsHold(t *testing.T) {
	raw := "From: a@example.com\r\nTo: b@example.net\r\nDate: Mon, 16 Nov 2026 08:00:00 +0000\r\nX-Thinpost-Hold: true\r\nSubject: x\r\n\r\nbody"
	s := mustScan(t, raw)
	if !s.Hold {
		t.Fatal("hold not seen")
	}
	out, _ := prepared(t, raw, "")
	_, rest, _ := splitPrepared(t, out)
	if !strings.Contains(rest, "Date: Mon, 16 Nov 2026 08:00:00 +0000\r\n") || strings.Count(rest, "Date:") != 1 {
		t.Fatalf("date not kept: %q", rest)
	}
	if strings.Contains(strings.ToLower(rest), "x-thinpost-hold") {
		t.Fatalf("hold header leaked: %q", rest)
	}
}
