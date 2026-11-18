package objectkey

import (
	"testing"
	"time"
)

func TestKeyReferenceWindowAndIdentity(t *testing.T) {
	issued := time.Unix(1800000000, 0).UTC()
	key, err := New("final", "0123456789abcdef0123456789abcdef", issued)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(key)
	if err != nil || parsed.Kind != "final" || !parsed.Issued.Equal(issued) {
		t.Fatal("invalid round trip", err)
	}
	for _, test := range []struct {
		now time.Time
		ok  bool
	}{{issued.Add(-FutureSkew - time.Second), false}, {issued.Add(-FutureSkew), true}, {issued, true}, {issued.Add(ReferenceLifetime - time.Second), true}, {issued.Add(ReferenceLifetime), false}} {
		if parsed.ReferencableAt(test.now) != test.ok {
			t.Fatalf("incorrect reference window at %v", test.now)
		}
	}
	other, err := New("final", parsed.ID, issued.Add(time.Second))
	if err != nil || other == key {
		t.Fatal("issuance did not change object identity", err)
	}
	for _, invalid := range []string{"final/0123456789abcdef0123456789abcdef", "final/v1/1800000000/0123456789ABCDEF0123456789ABCDEF", "other/v1/1800000000/0123456789abcdef0123456789abcdef", "final/v1/01800000000/0123456789abcdef0123456789abcdef"} {
		if _, err := Parse(invalid); err == nil {
			t.Fatal("accepted invalid key")
		}
	}
}
