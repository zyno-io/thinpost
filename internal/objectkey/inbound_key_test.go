package objectkey

import (
	"testing"
	"time"
)

func TestInboundKindIsManaged(t *testing.T) {
	issued := time.Unix(1800000000, 0).UTC()
	key, err := New("inbound", "0123456789abcdef0123456789abcdef", issued)
	if err != nil || key != "inbound/v1/1800000000/0123456789abcdef0123456789abcdef" {
		t.Fatal("inbound key", key, err)
	}
	parsed, err := Parse(key)
	if err != nil || parsed.Kind != "inbound" || !parsed.Issued.Equal(issued) {
		t.Fatal("inbound key does not parse", err)
	}
}
