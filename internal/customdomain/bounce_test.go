package customdomain

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type cnameResolver struct {
	answer string
	err    error
	asked  string
}

func (r *cnameResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	r.asked = host
	return r.answer, r.err
}

func TestCheckBounce(t *testing.T) {
	target := Target{Name: "mx.example.net", Reported: []string{"mx.example.net", "smtp2.example.net"}}
	const domain = "bounces.customer.example"
	for name, tc := range map[string]struct {
		target   Target
		answer   string
		err      error
		want     Result
		observed string
	}{
		"a CNAME to the expected name": {target, "mx.example.net.", nil, ResultOK, "mx.example.net"},
		"case and the trailing dot":    {target, "MX.Example.NET.", nil, ResultOK, "mx.example.net"},
		"NXDOMAIN":                     {target, "", &net.DNSError{IsNotFound: true}, ResultMissing, ""},
		"another error":                {target, "", &net.DNSError{IsTimeout: true}, ResultLookupFailed, ""},
		"a plain error":                {target, "", errors.New("boom"), ResultLookupFailed, ""},
		"no CNAME (a proxied record)":  {target, domain + ".", nil, ResultNotCNAME, ""},
		"another reported name":        {target, "smtp2.example.net.", nil, ResultOtherIP, "smtp2.example.net"},
		"anything else":                {target, "cdn.elsewhere.example.", nil, ResultWrongTarget, "cdn.elsewhere.example"},
		"no sending name known":        {Target{}, "mx.example.net.", nil, ResultNoSendingIP, ""},
	} {
		t.Run(name, func(t *testing.T) {
			resolver := &cnameResolver{answer: tc.answer, err: tc.err}
			got, observed := CheckBounce(context.Background(), resolver, domain, tc.target)
			if got != tc.want || observed != tc.observed {
				t.Fatalf("%v %q, want %v %q", got, observed, tc.want, tc.observed)
			}
			if tc.target.Name != "" && resolver.asked != domain+"." {
				t.Fatalf("lookup of %q, want the rooted name", resolver.asked)
			}
			if tc.target.Name == "" && resolver.asked != "" {
				t.Fatal("looked up a name with nothing to compare it with")
			}
		})
	}
	if got, _ := CheckBounce(context.Background(), nil, domain, target); got != ResultLookupFailed {
		t.Fatal("no resolver", got)
	}
}

func TestResultClasses(t *testing.T) {
	for _, r := range []Result{ResultOK, ResultMissing, ResultNotCNAME, ResultOtherIP, ResultWrongTarget, ResultLookupFailed, ResultNoSendingIP} {
		if !r.Valid() {
			t.Error(r)
		}
	}
	if Result("surprise").Valid() {
		t.Error("unknown result is valid")
	}
	for r, want := range map[Result]bool{ResultLookupFailed: true, ResultNoSendingIP: true, ResultMissing: false, ResultNotCNAME: false, ResultOtherIP: false, ResultWrongTarget: false, ResultOK: false} {
		if r.Indeterminate() != want {
			t.Error("indeterminate", r)
		}
	}
}

// The transition table of the plan: a state, a result, and the next state with
// the failure count.
func TestNext(t *testing.T) {
	definitive := []Result{ResultMissing, ResultNotCNAME, ResultOtherIP, ResultWrongTarget}
	indeterminate := []Result{ResultLookupFailed, ResultNoSendingIP}
	for _, state := range []string{StatePending, StateActive, StateFailing} {
		if got := Next(Status{State: state, Failures: 5}, ResultOK); got != (Status{State: StateActive}) {
			t.Errorf("%s + ok = %+v", state, got)
		}
	}
	for _, r := range definitive {
		if got := Next(Status{State: StatePending}, r); got != (Status{State: StatePending, Failures: 1}) {
			t.Errorf("pending + %s = %+v", r, got)
		}
		if got := Next(Status{State: StateActive}, r); got != (Status{State: StateFailing, Failures: 1}) {
			t.Errorf("active + %s = %+v", r, got)
		}
		if got := Next(Status{State: StateFailing, Failures: 3}, r); got != (Status{State: StateFailing, Failures: 4}) {
			t.Errorf("failing + %s = %+v", r, got)
		}
	}
	for _, r := range indeterminate {
		if got := Next(Status{State: StatePending}, r); got != (Status{State: StatePending, Failures: 1}) {
			t.Errorf("pending + %s = %+v", r, got)
		}
		if got := Next(Status{State: StateActive}, r); got != (Status{State: StateActive, Failures: 1}) {
			t.Errorf("active + %s once = %+v", r, got)
		}
		if got := Next(Status{State: StateActive, Failures: 1}, r); got != (Status{State: StateFailing, Failures: 2}) {
			t.Errorf("active + %s twice = %+v", r, got)
		}
		if got := Next(Status{State: StateFailing, Failures: 2}, r); got != (Status{State: StateFailing, Failures: 3}) {
			t.Errorf("failing + %s = %+v", r, got)
		}
	}
	// A passing check in between resets the count, so two failures that are
	// not consecutive never fail an active domain.
	s := Next(Status{State: StateActive}, ResultLookupFailed)
	s = Next(s, ResultOK)
	if s = Next(s, ResultLookupFailed); s.State != StateActive || s.Failures != 1 {
		t.Errorf("non-consecutive failures: %+v", s)
	}
	if got := Next(Status{State: StateActive, Failures: maxFailures}, ResultLookupFailed); got.Failures != maxFailures {
		t.Error("the counter is unbounded")
	}
}

func TestInterval(t *testing.T) {
	for _, tc := range []struct {
		state string
		age   time.Duration
		want  time.Duration
	}{
		{StatePending, 0, 10 * time.Minute},
		{StatePending, 47 * time.Hour, 10 * time.Minute},
		{StatePending, 49 * time.Hour, 6 * time.Hour},
		{StateActive, 0, 30 * time.Minute},
		{StateActive, 1000 * time.Hour, 30 * time.Minute},
		{StateFailing, time.Hour, 10 * time.Minute},
		{StateFailing, 23 * time.Hour, 10 * time.Minute},
		{StateFailing, 25 * time.Hour, time.Hour},
	} {
		if got := Interval(tc.state, tc.age); got != tc.want {
			t.Errorf("%s at %v: %v, want %v", tc.state, tc.age, got, tc.want)
		}
	}
	if Freshness != 3*time.Hour || ActiveEvery >= Freshness {
		t.Error("an active domain must be rechecked well inside the freshness window")
	}
}

func TestStrictSubdomain(t *testing.T) {
	for _, tc := range []struct {
		name, parent string
		want         bool
	}{
		{"bounces.example.com", "example.com", true},
		{"a.b.example.com", "example.com", true},
		{"example.com", "example.com", false},
		{".example.com", "example.com", false},
		{"not" + "example.com", "example.com", false},
		{"bounces.example.org", "example.com", false},
		{"x.example.com", "", false},
	} {
		if StrictSubdomain(tc.name, tc.parent) != tc.want {
			t.Errorf("%q under %q", tc.name, tc.parent)
		}
	}
}
