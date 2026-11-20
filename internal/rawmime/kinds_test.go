package rawmime

import (
	"reflect"
	"testing"
)

func TestMailboxKey(t *testing.T) {
	cases := []struct {
		in, key string
		ok      bool
	}{
		{"Alice@Example.COM", "Alice@example.com", true},
		{"<Alice@Example.COM>", "Alice@example.com", true},
		{"Alice <alice@example.com>", "alice@example.com", true},
		{`"team ops"@Example.com`, "team ops@example.com", true},
		{"nope", "", false},
		{"a@example.com\r\nBcc: x@example.net", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		key, ok := MailboxKey(c.in)
		if key != c.key || ok != c.ok {
			t.Errorf("%q: %q %v", c.in, key, ok)
		}
	}
}

func TestKindsCaseSensitiveLocalPart(t *testing.T) {
	s := mustScan(t, "To: Alice@example.com\r\n\r\nbody")
	rcpts, kinds, err := Kinds([]string{"Alice@example.com", "alice@example.com"}, s)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rcpts, []string{"Alice@example.com", "alice@example.com"}) || !reflect.DeepEqual(kinds, []Kind{KindTo, KindBcc}) {
		t.Fatalf("%v %v", rcpts, kinds)
	}
}

func TestKindsDomainCaseAndDedupe(t *testing.T) {
	s := mustScan(t, "To: Bob <bob@EXAMPLE.net>\r\nCc: carol@example.org\r\n\r\nbody")
	envelope := []string{"hidden@example.net", "carol@example.org", "bob@example.net", "BOB@example.net", "bob@Example.NET", "carol@EXAMPLE.org", "dan@example.net"}
	rcpts, kinds, err := Kinds(envelope, s)
	if err != nil {
		t.Fatal(err)
	}
	// bob@example.net appears first as bob@example.net (kept spelling); BOB@ is a
	// different local part and so another (bcc) recipient.
	wantR := []string{"bob@example.net", "carol@example.org", "hidden@example.net", "BOB@example.net", "dan@example.net"}
	wantK := []Kind{KindTo, KindCc, KindBcc, KindBcc, KindBcc}
	if !reflect.DeepEqual(rcpts, wantR) || !reflect.DeepEqual(kinds, wantK) {
		t.Fatalf("%v %v", rcpts, kinds)
	}
}

func TestKindsKeepsFirstSpelling(t *testing.T) {
	s := mustScan(t, "To: x@example.com\r\n\r\nb")
	rcpts, _, err := Kinds([]string{"bob@Example.net", "bob@example.NET"}, s)
	if err != nil || len(rcpts) != 1 || rcpts[0] != "bob@Example.net" {
		t.Fatalf("%v %v", rcpts, err)
	}
}

func TestKindsHeaderOnlyAddressGetsNoDelivery(t *testing.T) {
	s := mustScan(t, "To: a@example.com, ghost@example.com\r\n\r\nb")
	rcpts, kinds, err := Kinds([]string{"a@example.com"}, s)
	if err != nil || len(rcpts) != 1 || kinds[0] != KindTo {
		t.Fatalf("%v %v %v", rcpts, kinds, err)
	}
}

func TestKindsInvalidRecipient(t *testing.T) {
	s := mustScan(t, "To: a@example.com\r\n\r\nb")
	if _, _, err := Kinds([]string{"nonsense"}, s); err == nil {
		t.Fatal("want error")
	}
}

func TestKindsAllBcc(t *testing.T) {
	s := mustScan(t, "Subject: x\r\n\r\nb")
	_, kinds, err := Kinds([]string{"a@example.com", "b@example.com"}, s)
	if err != nil || !reflect.DeepEqual(kinds, []Kind{KindBcc, KindBcc}) {
		t.Fatalf("%v %v", kinds, err)
	}
}

func TestKindsRefusesNonPlainRecipients(t *testing.T) {
	s := mustScan(t, "To: ops@example.net\r\n\r\nbody")
	for _, rcpt := range []string{`"ops"@example.net`, `"\"ops\""@example.net`, `"a b"@example.net`, `Ops <ops@example.net>`, `<ops@example.net>`, `a..b@example.net`, `.a@example.net`, `a.@example.net`} {
		if _, _, err := Kinds([]string{rcpt}, s); err != ErrInvalid {
			t.Errorf("%q: %v", rcpt, err)
		}
	}
	rcpts, _, err := Kinds([]string{"First.Last+tag@Example.NET", "o'brien@example.net"}, s)
	if err != nil || len(rcpts) != 2 {
		t.Fatalf("%v %v", rcpts, err)
	}
}
