package prepared

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/edgeprotocol"
)

const testID = "0123456789abcdef0123456789abcdef"

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

var testHeader = []byte("DKIM-Signature: v=1\r\nSubject: hi\r\n\r\n")

// signedFor builds the message a recipe over base should assemble to, with an
// independent normalizer (the reference below).
func signedFor(header, base []byte, bodyStart int, reps []Replacement) []byte {
	return append(append([]byte{}, header...), reference(base, bodyStart, reps)...)
}

// reference is a byte-at-a-time model of AppendBody: base bytes from bodyStart,
// bare LF to CRLF, replaced spans swapped for their data.
func reference(base []byte, bodyStart int, reps []Replacement) []byte {
	var out []byte
	i := bodyStart
	next := 0
	for i < len(base) {
		if next < len(reps) && i == reps[next].Start {
			out = append(out, reps[next].Data...)
			i = reps[next].End
			next++
			continue
		}
		c := base[i]
		if c == '\n' && (i == 0 || base[i-1] != '\r') {
			out = append(out, '\r')
		}
		out = append(out, c)
		i++
	}
	// A splice at the very end of the base (start == end == len(base)).
	for ; next < len(reps); next++ {
		out = append(out, reps[next].Data...)
	}
	return out
}

func build(t *testing.T, base string, bodyStart int, reps []Replacement) (*Recipe, []byte) {
	t.Helper()
	signed := signedFor(testHeader, []byte(base), bodyStart, reps)
	r, err := New(testID, sha([]byte(base)), bodyStart, signed, len(testHeader), reps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, signed
}

func TestRoundTripNoSplices(t *testing.T) {
	base := "Subject: x\n\nline one\nline two\r\nbare\n"
	r, signed := build(t, base, 12, nil)
	object, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !IsRecipe(object) || IsRecipe(signed) {
		t.Fatal("IsRecipe")
	}
	got, err := Decode(object)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(got.Header(), testHeader) || got.DeliveryID() != testID || got.Size() != len(signed) {
		t.Fatal("decoded fields")
	}
	out, err := got.Assemble([]byte(base), sha([]byte(base)))
	if err != nil || !bytes.Equal(out, signed) {
		t.Fatalf("Assemble: %v\n%q\n%q", err, out, signed)
	}
	if !strings.HasSuffix(string(out), "line one\r\nline two\r\nbare\r\n") {
		t.Fatalf("not normalized: %q", out)
	}
}

func TestRoundTripWithSplices(t *testing.T) {
	base := "S: x\n\nAAAA\nBBBB\nCCCC\nDDDD\n"
	// Replace "BBBB\n" and "DDDD\n" (offsets in base).
	b1 := strings.Index(base, "BBBB")
	d1 := strings.Index(base, "DDDD")
	reps := []Replacement{
		{Start: b1, End: b1 + 5, Data: []byte("<<b>>\r\nsecond\r\n")},
		{Start: d1, End: len(base), Data: []byte("end\r\n")},
	}
	r, signed := build(t, base, 6, reps)
	object, _ := r.Encode()
	got, err := Decode(object)
	if err != nil {
		t.Fatal(err)
	}
	out, err := got.Assemble([]byte(base), sha([]byte(base)))
	if err != nil || !bytes.Equal(out, signed) {
		t.Fatalf("%v %q %q", err, out, signed)
	}
	if !strings.Contains(string(out), "AAAA\r\n<<b>>\r\nsecond\r\nCCCC\r\nend\r\n") {
		t.Fatalf("splice output %q", out)
	}
}

func TestAssembleRejects(t *testing.T) {
	base := "S: x\n\nAAAA\r\nBBBB\n"
	bodyStart := 6
	rep := []Replacement{{Start: bodyStart + 6, End: bodyStart + 10, Data: []byte("zz\r\n")}}
	r, _ := build(t, base, bodyStart, rep)
	if _, err := r.Assemble([]byte(base), sha([]byte(base))); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	for name, tc := range map[string]struct {
		base string
		sha  string
	}{
		"wrong hash argument":   {base, strings.Repeat("0", 64)},
		"different base bytes":  {strings.Replace(base, "AAAA", "AAAB", 1), sha([]byte(base))},
		"shorter than the body": {base[:bodyStart-1], sha([]byte(base))},
		"splice past the end":   {base[:bodyStart+8], sha([]byte(base))},
	} {
		if _, err := r.Assemble([]byte(tc.base), tc.sha); err == nil {
			t.Errorf("%s: assembled", name)
		}
	}
	// A splice boundary between CR and LF.
	cr := bytes.Index([]byte(base), []byte("\r\n"))
	bad := []Replacement{{Start: cr, End: cr + 1, Data: []byte("x\r\n")}}
	signed := signedFor(testHeader, []byte(base), bodyStart, bad)
	br, err := New(testID, sha([]byte(base)), bodyStart, signed, len(testHeader), bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = br.Assemble([]byte(base), sha([]byte(base))); err == nil {
		t.Error("splice dividing CRLF assembled")
	}
	bad = []Replacement{{Start: cr + 1, End: cr + 3, Data: []byte("x\r\n")}}
	signed = signedFor(testHeader, []byte(base), bodyStart, bad)
	br, _ = New(testID, sha([]byte(base)), bodyStart, signed, len(testHeader), bad)
	if _, err = br.Assemble([]byte(base), sha([]byte(base))); err == nil {
		t.Error("splice starting between CR and LF assembled")
	}
	// A body start between a CR and an LF.
	signed = signedFor(testHeader, []byte(base), cr+1, nil)
	br, _ = New(testID, sha([]byte(base)), cr+1, signed, len(testHeader), nil)
	if _, err = br.Assemble([]byte(base), sha([]byte(base))); err == nil {
		t.Error("body start inside a CRLF assembled")
	}
	// Tampered size/hash: the recipe says something else than the assembly.
	signed = []byte(strings.Replace(string(signedFor(testHeader, []byte(base), bodyStart, rep)), "zz", "yy", 1))
	tr, err := New(testID, sha([]byte(base)), bodyStart, signed, len(testHeader), rep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tr.Assemble([]byte(base), sha([]byte(base))); err == nil {
		t.Error("recipe whose hash is not the assembly assembled")
	}
}

func goodObject(t *testing.T) []byte {
	t.Helper()
	r, _ := build(t, "S: x\n\nbody\n", 6, nil)
	object, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return object
}

// withJSON replaces the JSON part of an object and fixes its length.
func withJSON(object []byte, edit func(string) string) []byte {
	n := binary.BigEndian.Uint32(object[len(Magic):])
	j := object[len(Magic)+4 : len(Magic)+4+int(n)]
	rest := object[len(Magic)+4+int(n):]
	nj := edit(string(j))
	out := append([]byte(Magic), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(out[len(Magic):], uint32(len(nj)))
	out = append(out, nj...)
	return append(out, rest...)
}

func TestDecodeStrictRejects(t *testing.T) {
	good := goodObject(t)
	if _, err := Decode(good); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":          {},
		"no magic":       []byte("DKIM-Signature: x\r\n\r\n"),
		"magic only":     []byte(Magic),
		"short length":   append([]byte(Magic), 0, 0),
		"zero json":      append(append([]byte(Magic), 0, 0, 0, 0), good[len(Magic)+4:]...),
		"json too long":  append(append([]byte(Magic), 0xff, 0xff, 0xff, 0xff), good[len(Magic)+4:]...),
		"trailing bytes": append(append([]byte{}, good...), 'x'),
		"truncated":      good[:len(good)-1],
		"unknown field": withJSON(good, func(s string) string {
			return strings.Replace(s, `"version":1`, `"version":1,"extra":1`, 1)
		}),
		"version 2": withJSON(good, func(s string) string { return strings.Replace(s, `"version":1`, `"version":2`, 1) }),
		"base prepared": withJSON(good, func(s string) string {
			return strings.Replace(s, `"raw_source"`, `"prepared"`, 1)
		}),
		"uppercase id":  withJSON(good, func(s string) string { return strings.Replace(s, "abcdef0123", "ABCDEF0123", 1) }),
		"json trailing": withJSON(good, func(s string) string { return s + "{}" }),
		"json garbage":  withJSON(good, func(s string) string { return s + "x" }),
		"size zero":     withJSON(good, func(s string) string { return strings.Replace(s, `"size":`, `"size":0,"x":`, 1) }),
		"negative body": withJSON(good, func(s string) string { return strings.Replace(s, `"body_start":6`, `"body_start":-1`, 1) }),
		"huge size": withJSON(good, func(s string) string {
			i := strings.Index(s, `"size":`)
			j := strings.Index(s[i:], ",")
			return s[:i] + `"size":` + "99999999" + s[i+j:]
		}),
		"header len short": withJSON(good, func(s string) string { return strings.Replace(s, `"header_len":36`, `"header_len":35`, 1) }),
		"splice len beyond data": withJSON(good, func(s string) string {
			return strings.Replace(s, `"splices":[]`, `"splices":[{"start":6,"end":7,"len":5}]`, 1)
		}),
		"splice before body": withJSON(good, func(s string) string {
			return strings.Replace(s, `"splices":[]`, `"splices":[{"start":2,"end":3,"len":0}]`, 1)
		}),
		"splice backwards": withJSON(good, func(s string) string {
			return strings.Replace(s, `"splices":[]`, `"splices":[{"start":8,"end":7,"len":0}]`, 1)
		}),
		"splices overlap": withJSON(good, func(s string) string {
			return strings.Replace(s, `"splices":[]`, `"splices":[{"start":6,"end":9,"len":0},{"start":8,"end":9,"len":0}]`, 1)
		}),
	}
	for name, object := range cases {
		if _, err := Decode(object); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func TestDecodeRejectsBadHeaderBlock(t *testing.T) {
	base := "S: x\n\nbody\n"
	for name, header := range map[string][]byte{
		"no blank line": []byte("DKIM-Signature: v=1\r\nSubject: hi\r\n"),
		"bare LF":       []byte("A: b\n\n"),
		"too large":     append(bytes.Repeat([]byte("A: b\r\n"), MaxHeader/6+1), "\r\n"...),
		"empty":         nil,
	} {
		signed := append(append([]byte{}, header...), reference([]byte(base), 6, nil)...)
		if _, err := New(testID, sha([]byte(base)), 6, signed, len(header), nil); err == nil {
			t.Errorf("%s: built", name)
		}
	}
}

func TestSpliceDataMustBeCRLFOnly(t *testing.T) {
	base := "S: x\n\nbody\n"
	for _, data := range []string{"a\n", "a\rb", "a\r", "\nx", "x\r\ny\n"} {
		reps := []Replacement{{Start: 6, End: 10, Data: []byte(data)}}
		signed := append(append([]byte{}, testHeader...), data...)
		signed = append(signed, "\r\n"...)
		if _, err := New(testID, sha([]byte(base)), 6, signed, len(testHeader), reps); err == nil {
			t.Errorf("splice data %q accepted", data)
		}
	}
}

func TestTooManySplices(t *testing.T) {
	base := strings.Repeat("ab", 300)
	var reps []Replacement
	for i := 0; i <= MaxSplices; i++ {
		reps = append(reps, Replacement{Start: 2 * i, End: 2*i + 1})
	}
	signed := signedFor(testHeader, []byte(base), 0, reps)
	if _, err := New(testID, sha([]byte(base)), 0, signed, len(testHeader), reps); err == nil {
		t.Fatal("257 splices accepted")
	}
	reps = reps[:MaxSplices]
	signed = signedFor(testHeader, []byte(base), 0, reps)
	if _, err := New(testID, sha([]byte(base)), 0, signed, len(testHeader), reps); err != nil {
		t.Fatalf("256 splices: %v", err)
	}
}

func TestSizeLimit(t *testing.T) {
	base := "S: x\n\nbody\n"
	signed := make([]byte, edgeprotocol.MaxMIMEBytes+1)
	copy(signed, testHeader)
	if _, err := New(testID, sha([]byte(base)), 6, signed, len(testHeader), nil); err == nil {
		t.Fatal("oversize accepted")
	}
}

func TestAppendBodyRejectsBadSplices(t *testing.T) {
	base := []byte("0123456789")
	for name, tc := range map[string]struct {
		splices []Splice
		data    string
	}{
		"out of order": {[]Splice{{5, 6, 0}, {2, 3, 0}}, ""},
		"past end":     {[]Splice{{5, 11, 0}}, ""},
		"data short":   {[]Splice{{1, 2, 3}}, "ab"},
		"data extra":   {[]Splice{{1, 2, 1}}, "abc"},
		"backwards":    {[]Splice{{3, 2, 0}}, ""},
	} {
		if _, err := AppendBody(nil, base, 0, tc.splices, []byte(tc.data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := AppendBody(nil, base, 11, nil, nil); err == nil {
		t.Error("body start past base accepted")
	}
}

func TestAppendBodyNormalizes(t *testing.T) {
	got, err := AppendBody([]byte("H"), []byte("\nA\nB\r\nC\r\n\n"), 0, nil, nil)
	if err != nil || string(got) != "H\r\nA\r\nB\r\nC\r\n\r\n" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestMaxOverheadCoversBiggestRecipe(t *testing.T) {
	if MaxOverhead != len(Magic)+4+MaxJSON+MaxHeader {
		t.Fatal("MaxOverhead")
	}
}
