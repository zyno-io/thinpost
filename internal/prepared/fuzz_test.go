package prepared

import (
	"bytes"
	"testing"
)

func FuzzDecode(f *testing.F) {
	base := []byte("S: x\n\nAAAA\nBBBB\nCCCC\n")
	reps := []Replacement{{Start: 11, End: 16, Data: []byte("zz\r\n")}}
	signed := signedFor(testHeader, base, 6, reps)
	r, _ := New(testID, sha(base), 6, signed, len(testHeader), reps)
	object, _ := r.Encode()
	plain, _ := New(testID, sha(base), 6, signedFor(testHeader, base, 6, nil), len(testHeader), nil)
	plainObject, _ := plain.Encode()
	f.Add(object, base)
	f.Add(plainObject, base)
	f.Add([]byte(Magic), []byte(nil))
	f.Add(object[:len(object)/2], base)
	f.Fuzz(func(t *testing.T, object, base []byte) {
		r, err := Decode(object)
		if err != nil {
			return
		}
		// A decoded recipe never panics when assembled, and an assembly always
		// has exactly the recorded size and hash.
		out, err := r.Assemble(base, r.BaseSHA256())
		if err == nil {
			if len(out) != r.Size() || sha(out) != r.SHA256() || !bytes.HasPrefix(out, r.Header()) {
				t.Fatal("assembly disagrees with its recipe")
			}
		}
		// Re-encoding a decoded recipe decodes to the same thing.
		again, err := r.Encode()
		if err != nil {
			t.Fatalf("Encode after Decode: %v", err)
		}
		r2, err := Decode(again)
		if err != nil || r2.Size() != r.Size() || r2.SHA256() != r.SHA256() || !bytes.Equal(r2.Header(), r.Header()) {
			t.Fatal("re-encode round trip")
		}
	})
}

// FuzzAppendBodyParity compares AppendBody with the byte-at-a-time reference on
// random bodies with bare LFs and random splices that do not divide a CRLF.
func FuzzAppendBodyParity(f *testing.F) {
	f.Add([]byte("a\nb\r\nc\n\n\r\n"), uint8(3), []byte{1, 2, 3, 4}, []byte("x\r\ny\r\n"))
	f.Add([]byte("\n\n\n"), uint8(0), []byte{0, 1, 1, 2}, []byte(nil))
	f.Add([]byte("no newline"), uint8(2), []byte{5, 9}, []byte("\r\n"))
	f.Fuzz(func(t *testing.T, base []byte, bodyStart uint8, cuts []byte, data []byte) {
		start := int(bodyStart)
		if start > len(base) {
			start = len(base)
		}
		if divides(base, start) {
			t.Skip("a body never starts between a CR and an LF")
		}
		// Make the data CRLF-only: keep whole CRLF pairs only.
		var clean []byte
		for _, c := range data {
			if c != '\r' && c != '\n' {
				clean = append(clean, c)
			}
		}
		clean = append(clean, "\r\n"...)
		// Positions derived from cuts, increasing, never dividing a CRLF.
		var reps []Replacement
		pos := start
		for i := 0; i+1 < len(cuts) && len(reps) < 8; i += 2 {
			s := pos + int(cuts[i])
			e := s + int(cuts[i+1])
			if e > len(base) {
				break
			}
			if divides(base, s) || divides(base, e) {
				continue
			}
			n := int(cuts[i]) % (len(clean) + 1)
			piece := append(bytes.Repeat([]byte("q"), n), "\r\n"...)
			reps = append(reps, Replacement{Start: s, End: e, Data: piece})
			pos = e
		}
		var splices []Splice
		var all []byte
		for _, p := range reps {
			splices = append(splices, Splice{p.Start, p.End, len(p.Data)})
			all = append(all, p.Data...)
		}
		got, err := AppendBody(nil, base, start, splices, all)
		if err != nil {
			t.Fatalf("AppendBody: %v", err)
		}
		want := reference(base, start, reps)
		if !bytes.Equal(got, want) {
			t.Fatalf("parity\n got %q\nwant %q", got, want)
		}
		// Whatever the input, the output has no bare LF.
		for i, c := range got {
			if c == '\n' && (i == 0 || got[i-1] != '\r') {
				t.Fatalf("bare LF at %d in %q", i, got)
			}
		}
	})
}
