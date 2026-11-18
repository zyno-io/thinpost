package submission

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func validMessage() Message {
	return Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "hello", Text: "body"}
}

func TestEnvelopeOrderKindsAndLimits(t *testing.T) {
	m := validMessage()
	m.To = []string{"t1@example.net", "t2@example.net"}
	m.Cc = []string{"c1@example.net"}
	m.Bcc = []string{"b1@example.net", "b2@example.net"}
	if got := m.Envelope(); !reflect.DeepEqual(got, []string{"t1@example.net", "t2@example.net", "c1@example.net", "b1@example.net", "b2@example.net"}) {
		t.Fatal("envelope order", got)
	}
	if got := m.RecipientKinds(); !reflect.DeepEqual(got, []string{"to", "to", "cc", "bcc", "bcc"}) {
		t.Fatal("kinds", got)
	}
	for i, want := range []string{"to", "to", "cc", "bcc", "bcc", "", ""} {
		if m.RecipientKind(i) != want {
			t.Fatal("kind at", i)
		}
	}
	if m.RecipientKind(-1) != "" || m.Validate() != nil {
		t.Fatal("negative index or valid message")
	}
	// The cap is 100 across To, Cc and Bcc together, and To is required.
	many := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "u" + strings.Repeat("a", i) + "@example.net"
		}
		return out
	}
	m = validMessage()
	m.To, m.Cc = many(60), many(40)
	if m.Validate() != nil {
		t.Fatal("100 recipients must be valid")
	}
	m.Bcc = []string{"x@example.net"}
	if m.Validate() == nil {
		t.Fatal("101 recipients must be invalid")
	}
	m = validMessage()
	m.To, m.Cc = nil, []string{"c@example.net"}
	if m.Validate() == nil {
		t.Fatal("a message without To must be invalid")
	}
	m = validMessage()
	m.Bcc = []string{"bad\r\naddress@example.net"}
	if m.Validate() == nil {
		t.Fatal("injection in Bcc must be invalid")
	}
}

// The stored source and fingerprint of a message without Cc or Bcc must not
// change: these bytes are what earlier versions wrote, apart from the version.
func TestMessageWithoutCcOrBccEncodesIdentically(t *testing.T) {
	data, err := json.Marshal(validMessage())
	if err != nil {
		t.Fatal(err)
	}
	const golden = `{"From":"a@example.com","To":["b@example.net"],"Subject":"hello","Text":"body","HTML":"","EnhancedProtection":false}`
	if string(data) != golden {
		t.Fatal("message JSON changed:", string(data))
	}
	source, err := EncodeSource(validMessage())
	if err != nil || string(source) != `{"version":2,"message":`+golden+`}` {
		t.Fatal("source JSON changed:", string(source), err)
	}
}

func TestDecodeSource(t *testing.T) {
	good := `{"version":2,"message":{"From":"a@example.com","To":["b@example.net"],"Cc":["c@example.net"],"Subject":"s","Text":"t","EnhancedProtection":false}}`
	source, err := DecodeSource([]byte(good), false)
	if err != nil || source.Version != 2 || !source.ListsRecipients() || len(source.Message.Cc) != 1 {
		t.Fatal("version 2 source", err)
	}
	v1 := `{"version":1,"message":{"From":"a@example.com","To":["b@example.net"],"Subject":"s","Text":"t","EnhancedProtection":true}}`
	if source, err = DecodeSource([]byte(v1), true); err != nil || source.Version != 1 || source.ListsRecipients() {
		t.Fatal("version 1 source", err)
	}
	for name, tc := range map[string]struct {
		data      string
		protected bool
	}{
		"v1 with cc":             {`{"version":1,"message":{"From":"a@example.com","To":["b@example.net"],"Cc":["c@example.net"],"Subject":"s","Text":"t"}}`, false},
		"v1 with bcc":            {`{"version":1,"message":{"From":"a@example.com","To":["b@example.net"],"Bcc":["c@example.net"],"Subject":"s","Text":"t"}}`, false},
		"v3":                     {`{"version":3,"message":{"From":"a@example.com","To":["b@example.net"],"Subject":"s","Text":"t"}}`, false},
		"v0":                     {`{"message":{"From":"a@example.com","To":["b@example.net"],"Subject":"s","Text":"t"}}`, false},
		"unknown field":          {`{"version":2,"message":{"From":"a@example.com","To":["b@example.net"],"Subject":"s","Text":"t","Extra":1}}`, false},
		"trailing data":          {good + `{}`, false},
		"protection mismatch":    {good, true},
		"invalid message":        {`{"version":2,"message":{"From":"a@example.com","To":[],"Subject":"s","Text":"t"}}`, false},
		"not json":               {`nope`, false},
		"unknown envelope field": {`{"version":2,"extra":1,"message":{}}`, false},
	} {
		if _, err := DecodeSource([]byte(tc.data), tc.protected); !errors.Is(err, ErrInvalidSource) {
			t.Fatalf("%s: accepted (%v)", name, err)
		}
	}
}
