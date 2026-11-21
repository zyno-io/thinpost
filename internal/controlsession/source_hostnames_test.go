package controlsession

import (
	"fmt"
	"strings"
	"testing"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
)

func readyWithNames(sources, names, domains []string) *controlv1.Ready {
	return &controlv1.Ready{WorkerId: "worker", Epoch: 1, ProtocolVersion: uint32(edgeprotocol.Version), SourceAddresses: sources, SourceHostnames: names, BounceDomains: domains}
}

func TestReadyReportsPerAddressHostnames(t *testing.T) {
	sources := []string{"45.0.1.22", "45.0.2.11"}
	caps, valid := capabilitiesFromReady(readyWithNames(sources, []string{"smtp.mx.example", "smtp.hosting.example"}, []string{"b.example", "smtp.hosting.example"}))
	if !valid || len(caps.Sources) != 2 {
		t.Fatalf("valid names rejected: %+v %v", caps, valid)
	}
	if source, ok := caps.Source("45.0.2.11"); !ok || source.Hostname != "smtp.hosting.example" {
		t.Fatalf("source for the second address: %+v %v", source, ok)
	}
	if primary, ok := caps.Primary(); !ok || primary.Address.String() != "45.0.1.22" || primary.Hostname != "smtp.mx.example" {
		t.Fatalf("primary: %+v %v", primary, ok)
	}
	if _, ok := caps.Source("203.0.113.9"); ok {
		t.Fatal("a source for an address the worker did not report")
	}
}

// An Edge that reports addresses without names is accepted, so its session
// runs, but control invents no name: it yields no sources and nothing is
// claimed for it.
func TestReadyWithoutHostnamesYieldsNoSources(t *testing.T) {
	caps, valid := capabilitiesFromReady(readyWithNames([]string{"45.0.1.22", "45.0.0.10"}, nil, []string{"b.example"}))
	if !valid || len(caps.Sources) != 0 {
		t.Fatalf("an Edge without names was not accepted without sources: %+v %v", caps, valid)
	}
	if _, ok := caps.Primary(); ok || caps.Has("45.0.0.10") {
		t.Fatal("a source was invented for an Edge that reported no names")
	}
}

func TestReadyHostnamesAreValidated(t *testing.T) {
	sources := []string{"45.0.1.22", "45.0.0.10"}
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 40) + ".example"
	var many []string
	for i := 0; i < maxReportedBounceDomains+1; i++ {
		many = append(many, fmt.Sprintf("d%d.example", i))
	}
	for name, ready := range map[string]*controlv1.Ready{
		"fewer names than addresses": readyWithNames(sources, []string{"smtp.a.example"}, []string{"b.example"}),
		"more names than addresses":  readyWithNames(sources, []string{"a.example", "b.example", "c.example"}, []string{"b.example"}),
		"unnormalized name":          readyWithNames(sources, []string{"smtp.a.example", "SMTP.B.example"}, []string{"b.example"}),
		"duplicate name":             readyWithNames(sources, []string{"smtp.a.example", "smtp.a.example"}, []string{"b.example"}),
		"name too long":              readyWithNames(sources, []string{"smtp.a.example", long}, []string{"b.example"}),
		"names without addresses":    readyWithNames(nil, []string{"smtp.a.example"}, nil),
		"more domains than the cap":  readyWithNames(sources, []string{"smtp.a.example", "smtp.b.example"}, many),
	} {
		if _, valid := capabilitiesFromReady(ready); valid {
			t.Errorf("%s: accepted", name)
		}
	}
	// Exactly the cap is fine.
	if _, valid := capabilitiesFromReady(readyWithNames(sources, []string{"smtp.a.example", "smtp.b.example"}, many[:maxReportedBounceDomains])); !valid {
		t.Error("exactly the domain cap was rejected")
	}
}
