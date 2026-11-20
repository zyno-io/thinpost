package edgeworker

import (
	"context"
	"net/netip"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
)

var testSources = []netip.Addr{netip.MustParseAddr("45.0.1.22"), netip.MustParseAddr("45.0.0.10")}

func TestExecuteUsesPrimaryAndOwnHostnameWhenSourceFieldsAreEmpty(t *testing.T) {
	j, fence, start, mime := setup(t)
	dialer := &fakeDialer{}
	worker := &Worker{Journal: j, Dialer: dialer, HeloName: "smtp.sender.example", Sources: testSources}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := worker.Execute(ctx, fence, start, mime, func() bool { return true }, allowStart)
	if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED {
		t.Fatalf("%+v %v", result, err)
	}
	if err = <-dialer.serverDone; err != nil {
		t.Fatal(err)
	}
	if dialer.source.IsValid() || dialer.helo != "smtp.sender.example" {
		t.Fatalf("legacy delivery used source %v helo %q", dialer.source, dialer.helo)
	}
}

func TestExecuteUsesRequestedSourceAndHelo(t *testing.T) {
	j, fence, start, mime := setup(t)
	start.SourceAddress, start.HeloName = "45.0.0.10", "smtp.hosting.example"
	dialer := &fakeDialer{}
	worker := &Worker{Journal: j, Dialer: dialer, HeloName: "smtp.sender.example", Sources: testSources}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := worker.Execute(ctx, fence, start, mime, func() bool { return true }, allowStart)
	if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED {
		t.Fatalf("%+v %v", result, err)
	}
	if err = <-dialer.serverDone; err != nil {
		t.Fatal(err)
	}
	if dialer.source.String() != "45.0.0.10" || dialer.helo != "smtp.hosting.example" {
		t.Fatalf("source %v helo %q", dialer.source, dialer.helo)
	}
}

// An invalid or unreported source is refused before the permit is requested
// and before any dial, with the retryable code-zero REJECTED outcome.
func TestExecuteRefusesUnrequestableSourceBeforePermit(t *testing.T) {
	for name, fields := range map[string][2]string{
		"source without HELO":      {"45.0.0.10", ""},
		"HELO without source":      {"", "smtp.hosting.example"},
		"unreported source":        {"45.0.0.99", "smtp.hosting.example"},
		"primary-looking IPv6":     {"2607:f8b0:4000::1", "smtp.hosting.example"},
		"mapped IPv4":              {"::ffff:45.0.0.10", "smtp.hosting.example"},
		"not an address":           {"mail.example.com", "smtp.hosting.example"},
		"unnormalized source":      {"045.0.0.10", "smtp.hosting.example"},
		"uppercase HELO":           {"45.0.0.10", "SMTP.hosting.example"},
		"HELO with space":          {"45.0.0.10", "smtp hosting.example"},
		"HELO with line break":     {"45.0.0.10", "smtp.hosting.example\r\nRSET"},
		"HELO that is an address":  {"45.0.0.10", "45.0.0.10"},
		"empty-label HELO":         {"45.0.0.10", "smtp..hosting.example"},
		"trailing-dot HELO":        {"45.0.0.10", "smtp.hosting.example."},
		"single-label HELO":        {"45.0.0.10", "smtp"},
		"unreported primary alias": {"45.0.1.23", "smtp.hosting.example"},
	} {
		t.Run(name, func(t *testing.T) {
			j, fence, start, mime := setup(t)
			start.SourceAddress, start.HeloName = fields[0], fields[1]
			dialer := &fakeDialer{}
			worker := &Worker{Journal: j, Dialer: dialer, HeloName: "smtp.sender.example", Sources: testSources}
			permits := 0
			result, err := worker.Execute(context.Background(), fence, start, mime, func() bool { return true }, func(context.Context, *controlv1.DeliveryStart) error { permits++; return nil })
			if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED || result.SmtpCode != 0 || permits != 0 || dialer.dials != 0 {
				t.Fatalf("outcome %v permits %d dials %d err %v", result.GetOutcome(), permits, dialer.dials, err)
			}
		})
	}
}

func TestExecuteRefusesExplicitSourceFromAWorkerWithNoSources(t *testing.T) {
	j, fence, start, mime := setup(t)
	start.SourceAddress, start.HeloName = "45.0.1.22", "smtp.sender.example"
	dialer := &fakeDialer{}
	worker := &Worker{Journal: j, Dialer: dialer, HeloName: "smtp.sender.example"}
	permits := 0
	result, err := worker.Execute(context.Background(), fence, start, mime, func() bool { return true }, func(context.Context, *controlv1.DeliveryStart) error { permits++; return nil })
	if err != nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED || permits != 0 || dialer.dials != 0 {
		t.Fatal("a worker that reported no sources accepted an explicit one", err)
	}
}
