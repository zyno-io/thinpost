package submission

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const rawMsg = "From: a@example.com\r\nTo: b@example.net\r\nSubject: hi\r\n\r\nbody\r\n"

func rawHeader() RawHeader {
	return RawHeader{
		MailFrom:     "a@example.com",
		Recipients:   []string{"b@example.net", "c@example.net", "d@example.org"},
		Kinds:        []string{"to", "cc", "bcc"},
		SenderDomain: "example.com",
	}
}

func TestRawSourceRoundTrip(t *testing.T) {
	h := rawHeader()
	h.From, h.FromField, h.FromRewritten, h.StripSender = "a@example.com", "From: a@example.com\r\n", true, true
	h.OriginalFrom, h.EnhancedProtection = "x@example.org", true
	data, err := EncodeRawSource(h, []byte(rawMsg))
	if err != nil {
		t.Fatal(err)
	}
	src, err := DecodeSource(data, true)
	if err != nil {
		t.Fatal(err)
	}
	if src.Version != 3 || string(src.Raw) != rawMsg || !src.ListsRecipients() || src.RawHeader.RawSHA256 == "" {
		t.Fatalf("%+v", src)
	}
	if !reflect.DeepEqual(src.Envelope(), h.Recipients) {
		t.Fatal("envelope", src.Envelope())
	}
	for i, want := range []string{"to", "cc", "bcc", "", ""} {
		idx := i
		if i == 4 {
			idx = -1
		}
		if src.RecipientKind(idx) != want {
			t.Fatal("kind", idx)
		}
	}
	// v1/v2 accessors still work through Source.
	v2, err := DecodeSource(mustEncode(t, validMessage()), false)
	if err != nil || v2.Version != 2 || v2.Envelope()[0] != "b@example.net" || v2.RecipientKind(0) != "to" || v2.RecipientKind(1) != "" {
		t.Fatalf("%v %+v", err, v2)
	}
}

func mustEncode(t *testing.T, m Message) []byte {
	b, err := EncodeSource(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRawSourceV12FailClosedOnV3AndBack(t *testing.T) {
	data, err := EncodeRawSource(rawHeader(), []byte(rawMsg))
	if err != nil {
		t.Fatal(err)
	}
	var env sourceEnvelope
	if json.Unmarshal(data, &env) == nil {
		t.Fatal("v3 must not be JSON")
	}
	// A JSON object carrying "version": 3 is not a v3 source.
	if _, err := DecodeSource([]byte(`{"version":3,"message":{}}`), false); !errors.Is(err, ErrInvalidSource) {
		t.Fatal("json version 3")
	}
}

// reframe builds a v3 object from an arbitrary header JSON and raw bytes.
func reframe(header string, raw string) []byte {
	out := []byte(rawSourceMagic)
	out = binary.BigEndian.AppendUint32(out, uint32(len(header)))
	return append(append(out, header...), raw...)
}

func goodHeaderJSON(t *testing.T, mutate func(map[string]any)) string {
	data, err := EncodeRawSource(rawHeader(), []byte(rawMsg))
	if err != nil {
		t.Fatal(err)
	}
	n := binary.BigEndian.Uint32(data[len(rawSourceMagic):])
	start := len(rawSourceMagic) + 4
	var m map[string]any
	if err := json.Unmarshal(data[start:start+int(n)], &m); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestRawSourceRejects(t *testing.T) {
	good, _ := EncodeRawSource(rawHeader(), []byte(rawMsg))
	hl := int(binary.BigEndian.Uint32(good[len(rawSourceMagic):]))
	lenAt := len(rawSourceMagic)

	withLen := func(n uint32) []byte {
		b := bytes.Clone(good)
		binary.BigEndian.PutUint32(b[lenAt:], n)
		return b
	}
	mut := func(f func(map[string]any)) []byte { return reframe(goodHeaderJSON(t, f), rawMsg) }
	hdr := func(f func(*RawHeader)) error {
		h := rawHeader()
		f(&h)
		_, err := EncodeRawSource(h, []byte(rawMsg))
		return err
	}
	cases := map[string][]byte{
		"bad magic":        append([]byte("thinpost-source-v4\n"), good[len(rawSourceMagic):]...),
		"no length":        []byte(rawSourceMagic + "ab"),
		"zero header":      withLen(0),
		"header too long":  withLen(uint32(len(good))),
		"header over max":  withLen(MaxRawHeaderLen + 1),
		"header short":     withLen(uint32(hl - 1)),
		"unknown field":    mut(func(m map[string]any) { m["extra"] = 1 }),
		"sha mismatch":     mut(func(m map[string]any) { m["raw_sha256"] = strings.Repeat("0", 64) }),
		"uppercase sha":    mut(func(m map[string]any) { m["raw_sha256"] = strings.ToUpper(m["raw_sha256"].(string)) }),
		"kinds order":      mut(func(m map[string]any) { m["kinds"] = []any{"cc", "to", "bcc"} }),
		"bad kind":         mut(func(m map[string]any) { m["kinds"] = []any{"to", "cc", "x"} }),
		"kinds short":      mut(func(m map[string]any) { m["kinds"] = []any{"to"} }),
		"ep mismatch":      mut(func(m map[string]any) { m["enhanced_protection"] = true }),
		"no recipients":    mut(func(m map[string]any) { m["recipients"], m["kinds"] = []any{}, []any{} }),
		"bad recipient":    mut(func(m map[string]any) { m["recipients"] = []any{"nope", "c@example.net", "d@example.org"} }),
		"dup recipient":    mut(func(m map[string]any) { m["recipients"] = []any{"b@example.net", "b@EXAMPLE.NET", "d@example.org"} }),
		"upper domain":     mut(func(m map[string]any) { m["sender_domain"] = "Example.com" }),
		"trailing json":    reframe(goodHeaderJSON(t, nil)+" {}", rawMsg),
		"empty raw":        func() []byte { d, _ := EncodeRawSource(rawHeader(), []byte("x")); return d[:len(d)-1] }(),
		"raw tampered":     append(bytes.Clone(good[:len(good)-1]), 'X'),
		"missing from":     mut(func(m map[string]any) { delete(m, "recipients") }),
		"rewrite no field": mut(func(m map[string]any) { m["from_rewritten"] = true }),
	}
	for name, data := range cases {
		if _, err := DecodeSource(data, false); !errors.Is(err, ErrInvalidSource) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	if _, err := DecodeSource(good, true); !errors.Is(err, ErrInvalidSource) {
		t.Error("claim flag mismatch accepted")
	}
	// Encoder-side rejects.
	bad := map[string]func(*RawHeader){
		"kinds order":   func(h *RawHeader) { h.Kinds = []string{"bcc", "cc", "to"} },
		"101 rcpts":     func(h *RawHeader) { h.Recipients, h.Kinds = many(101), repeatKind(101) },
		"field w/o val": func(h *RawHeader) { h.FromField, h.FromRewritten = "From: a@example.com\r\n", true },
		"rewrite keeps sender": func(h *RawHeader) {
			h.From, h.FromField, h.FromRewritten = "a@example.com", "From: a@example.com\r\n", true
		},
		"from domain mismatch": func(h *RawHeader) {
			h.From, h.FromField, h.FromRewritten, h.StripSender = "a@example.net", "From: a@example.net\r\n", true, true
		},
		"field not CRLF": func(h *RawHeader) {
			h.From, h.FromField, h.FromRewritten, h.StripSender = "a@example.com", "From: a@example.com\n", true, true
		},
		"original w/o rewrite": func(h *RawHeader) { h.OriginalFrom = "x@example.org" },
	}
	for name, f := range bad {
		if hdr(f) == nil {
			t.Errorf("encode accepted %s", name)
		}
	}
	if _, err := EncodeRawSource(rawHeader(), make([]byte, MaxRawBytes+1)); err == nil {
		t.Error("oversize raw accepted")
	}
	if hdr(func(h *RawHeader) { h.Recipients, h.Kinds = many(100), repeatKind(100) }) != nil {
		t.Error("100 recipients rejected")
	}
	if hdr(func(h *RawHeader) { h.MailFrom = "<>" }) != nil || hdr(func(h *RawHeader) { h.MailFrom = "" }) != nil {
		t.Error("null path rejected")
	}
}

func many(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "u" + strings.Repeat("a", i) + "@example.net"
	}
	return out
}

func repeatKind(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "to"
	}
	return out
}
