package edgesession

import (
	"context"
	"reflect"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
)

func readyFor(t *testing.T, s *Server, ctx context.Context) *controlv1.Ready {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stream := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 8), sent: make(chan *controlv1.EdgeFrame, 8)}
	done := make(chan error, 1)
	go func() { done <- s.Connect(stream) }()
	stream.incoming <- openFrame(s, 1)
	ready := (<-stream.sent).GetReady()
	if ready == nil {
		t.Fatal("session not ready")
	}
	cancel()
	<-done
	return ready
}

func TestReadyCarriesAdvertisedSourcesAndHostnames(t *testing.T) {
	s, _, _, ctx := setup(t, true)
	s.Advertise([]string{"45.0.1.22", "45.0.0.10"}, []string{"smtp.example", "smtp.hosting.example"})
	ready := readyFor(t, s, ctx)
	if !reflect.DeepEqual(ready.SourceAddresses, []string{"45.0.1.22", "45.0.0.10"}) {
		t.Fatalf("ready reported %v %v", ready.SourceAddresses, ready.BounceDomains)
	}
	if len(ready.BounceDomains) != 0 {
		t.Fatalf("ready still sends the deprecated bounce_domains: %v", ready.BounceDomains)
	}
	if !reflect.DeepEqual(ready.SourceHostnames, []string{"smtp.example", "smtp.hosting.example"}) {
		t.Fatalf("ready reported hostnames %v", ready.SourceHostnames)
	}
}

// A worker that never advertises reports nothing, and control sends it no
// deliveries.
func TestReadyWithoutAdvertiseReportsNothing(t *testing.T) {
	s, _, _, ctx := setup(t, true)
	ready := readyFor(t, s, ctx)
	if len(ready.SourceAddresses) != 0 || len(ready.SourceHostnames) != 0 {
		t.Fatal("an unconfigured worker advertised sources")
	}
}
