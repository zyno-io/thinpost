package message

import (
	"testing"

	"github.com/zyno-io/thinpost/internal/rawmime"
)

// rawmime repeats the DKIM key ceiling so it need not import this package.
func TestRawmimeSizingCeilingMatches(t *testing.T) {
	if rawmime.MaxSizingRSABits != MaxDKIMRSABits {
		t.Fatalf("rawmime.MaxSizingRSABits = %d, want MaxDKIMRSABits %d", rawmime.MaxSizingRSABits, MaxDKIMRSABits)
	}
}
