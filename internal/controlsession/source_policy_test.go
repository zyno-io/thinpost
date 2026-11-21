package controlsession

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/egress"
)

// The Edge applies no destination policy, so control refuses an Edge whose
// Ready reports a source its own infrastructure ranges or route exceptions do
// not allow, exactly as the Edge itself used to at startup.
func TestReadySourcesMustSatisfyTheSourcePolicy(t *testing.T) {
	policy, err := egress.NewPolicy(
		[]netip.Prefix{netip.MustParsePrefix("45.0.1.0/24"), netip.MustParsePrefix("45.0.0.0/24")},
		egress.Exception{Recipient: "test@mailhost.example", MX: "mail.mailhost.example", Address: netip.MustParseAddr("45.0.0.50")},
	)
	if err != nil {
		t.Fatal(err)
	}
	run := func(sources []string) error {
		work := &capsWork{seen: make(chan struct{}, 1)}
		runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
		runner.Work, runner.Sources = work, policy
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		open := (<-out).GetOpen()
		frame := readyFrame(open)
		frame.GetReady().SourceAddresses = sources
		for i := range sources {
			frame.GetReady().SourceHostnames = append(frame.GetReady().SourceHostnames, fmt.Sprintf("mx%d.example.net", i))
		}
		in <- frame
		select {
		case err := <-done:
			return err
		case <-time.After(300 * time.Millisecond):
			cancel()
			<-done
			return nil
		}
	}
	for name, sources := range map[string][]string{
		"extra source outside the infrastructure ranges":   {"45.0.1.22", "8.8.8.8"},
		"primary outside the infrastructure ranges":        {"8.8.4.4"},
		"extra source named by a route exception":          {"45.0.1.22", "45.0.0.50"},
		"only the exception address as the primary source": {"45.0.0.50"},
	} {
		if err := run(sources); err == nil || !strings.Contains(err.Error(), "invalid edge readiness") {
			t.Errorf("%s: session continued: %v", name, err)
		}
	}
	if err := run([]string{"45.0.1.22", "45.0.0.10"}); err != nil && strings.Contains(err.Error(), "invalid edge readiness") {
		t.Errorf("sources inside the ranges were refused: %v", err)
	}
}
