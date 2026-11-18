package submission

import (
	"encoding/json"
	"testing"
)

func TestRequestTracking(t *testing.T) {
	yes, no := true, false
	spec := func(o *TrackingOptions) string {
		if o == nil {
			return "-"
		}
		s := ""
		if o.Opens {
			s += "o"
		}
		if o.Clicks {
			s += "c"
		}
		return s
	}
	for _, tc := range []struct {
		name          string
		opens, clicks *bool
		on, off       string
	}{
		{"absent", nil, nil, "-", "-"},
		{"on", &yes, &yes, "oc", "-"},
		{"off", &no, &no, "-", "oc"},
		{"mixed", &no, &yes, "c", "o"},
		{"one kind", &yes, nil, "o", "-"},
	} {
		var m Message
		m.RequestTracking(tc.opens, tc.clicks)
		if spec(m.Tracking) != tc.on || spec(m.TrackingOff) != tc.off {
			t.Fatalf("%s: tracking %s off %s", tc.name, spec(m.Tracking), spec(m.TrackingOff))
		}
	}
	var a, b Message
	a.RequestTracking(&yes, &no)
	b.RequestTracking(&yes, &no)
	if a.Tracking == b.Tracking || a.TrackingOff == b.TrackingOff {
		t.Fatal("RequestTracking shares options")
	}
}

func TestTrackingOffValidation(t *testing.T) {
	base := Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "t"}
	// An explicit opt-out is valid without HTML.
	m := base
	m.TrackingOff = &TrackingOptions{Opens: true}
	if m.Validate() != nil {
		t.Fatal("opt-out on a text message rejected")
	}
	m.TrackingOff = &TrackingOptions{}
	if m.Validate() == nil {
		t.Fatal("empty TrackingOff accepted")
	}
	m = base
	m.HTML = "<b>x</b>"
	m.Tracking = &TrackingOptions{Opens: true}
	m.TrackingOff = &TrackingOptions{Opens: true, Clicks: true}
	if m.Validate() == nil {
		t.Fatal("overlapping on and off accepted")
	}
	m.TrackingOff = &TrackingOptions{Clicks: true}
	if m.Validate() != nil {
		t.Fatal("disjoint on and off rejected")
	}
}

func TestTrackingOffIsOmittedFromLegacyJSON(t *testing.T) {
	// Frozen pre-defaults shape: earlier fingerprints stay byte-identical.
	m := Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "hello", Text: "body", EnhancedProtection: true}
	got, err := json.Marshal(m)
	if err != nil || string(got) != `{"From":"a@example.com","To":["b@example.net"],"Subject":"hello","Text":"body","HTML":"","EnhancedProtection":true}` {
		t.Fatal("legacy JSON changed", string(got), err)
	}
	m.Tracking = &TrackingOptions{Opens: true}
	if got, _ = json.Marshal(m); string(got) != `{"From":"a@example.com","To":["b@example.net"],"Subject":"hello","Text":"body","HTML":"","EnhancedProtection":true,"Tracking":{"opens":true}}` {
		t.Fatal("tracking JSON changed", string(got))
	}
}
