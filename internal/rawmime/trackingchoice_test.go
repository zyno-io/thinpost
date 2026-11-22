package rawmime

import (
	"errors"
	"testing"
)

func TestTrackingChoice(t *testing.T) {
	b := func(v bool) *bool { return &v }
	eq := func(a, b *bool) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
	cases := []struct {
		name          string
		headers       string
		opens, clicks *bool
		wantErr       bool
	}{
		{"absent", "", nil, nil, false},
		{"opens true", "X-Thinpost-Track-Opens: true\r\n", b(true), nil, false},
		{"clicks false", "X-Thinpost-Track-Clicks: false\r\n", nil, b(false), false},
		{"both", "X-Thinpost-Track-Opens: false\r\nX-Thinpost-Track-Clicks: TRUE\r\n", b(false), b(true), false},
		{"case and name case", "x-thinpost-track-opens: TrUe\r\n", b(true), nil, false},
		{"folded value", "X-Thinpost-Track-Opens:\r\n  true \r\n", b(true), nil, false},
		{"twice", "X-Thinpost-Track-Opens: true\r\nX-Thinpost-Track-Opens: true\r\n", nil, nil, true},
		{"twice clicks", "X-Thinpost-Track-Clicks: true\r\nX-Thinpost-Track-Clicks: false\r\n", nil, nil, true},
		{"junk", "X-Thinpost-Track-Opens: yes\r\n", nil, nil, true},
		{"empty", "X-Thinpost-Track-Clicks:\r\n", nil, nil, true},
		{"one bad one good", "X-Thinpost-Track-Opens: true\r\nX-Thinpost-Track-Clicks: 1\r\n", nil, nil, true},
	}
	for _, c := range cases {
		// Scan itself must never fail on these values.
		s, err := Scan([]byte("From: a@example.com\r\n" + c.headers + "\r\nbody\r\n"))
		if err != nil {
			t.Fatalf("%s: Scan: %v", c.name, err)
		}
		opens, clicks, err := s.TrackingChoice()
		if c.wantErr {
			if !errors.Is(err, ErrInvalid) || opens != nil || clicks != nil {
				t.Errorf("%s: %v %v %v", c.name, opens, clicks, err)
			}
			continue
		}
		if err != nil || !eq(opens, c.opens) || !eq(clicks, c.clicks) {
			t.Errorf("%s: got %v %v %v", c.name, opens, clicks, err)
		}
	}
}
