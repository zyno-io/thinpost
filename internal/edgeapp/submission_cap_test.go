package edgeapp

import (
	"testing"

	"github.com/zyno-io/thinpost/internal/edgeprotocol"
)

// The submission server cap must fit two listeners per sending address plus
// the internal one, or an edge with the maximum addresses would not start.
func TestSubmissionListenerCapFitsEveryAddress(t *testing.T) {
	if want := 2*(MaxAdditionalSources+1) + 1; edgeprotocol.MaxSubmissionListeners != want {
		t.Fatalf("MaxSubmissionListeners = %d, want %d", edgeprotocol.MaxSubmissionListeners, want)
	}
}
