// Package customdomain checks the DNS of customer-owned domains that Thinpost
// uses on a customer's behalf, and decides how each check moves the domain
// between states. It has no database or HTTP dependencies: the store, the admin
// API and the background checker all share these rules.
//
// A bounce domain is a strict subdomain of a verified sender domain that is a
// CNAME to the HELO name of the app's sending IP. Whether to use it is decided
// at the moment a delivery is claimed from current rows (never from a state
// remembered here); this package only keeps those rows honest.
package customdomain

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

// Resolver is the one DNS operation a bounce check needs. *net.Resolver
// satisfies it.
type Resolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
}

// Result is the outcome of one check, stored as bounce_domains.last_result.
type Result string

const (
	// ResultOK: the name is a CNAME to the expected HELO name.
	ResultOK Result = "ok"
	// ResultMissing: the name does not exist.
	ResultMissing Result = "missing"
	// ResultNotCNAME: the name exists but is not a CNAME. This is also what a
	// proxied record (a CDN's "orange cloud") looks like: its answers are the
	// proxy's addresses, never the CNAME.
	ResultNotCNAME Result = "not_cname"
	// ResultOtherIP: the CNAME points at another of Thinpost's sending names,
	// so the app sends from a different address than the record names.
	ResultOtherIP Result = "other_ip"
	// ResultWrongTarget: the CNAME points somewhere else.
	ResultWrongTarget Result = "wrong_target"
	// ResultLookupFailed: DNS did not answer (timeout, SERVFAIL, no resolver).
	ResultLookupFailed Result = "lookup_failed"
	// ResultNoSendingIP: Thinpost does not yet know the app's sending name.
	ResultNoSendingIP Result = "no_sending_ip"
)

// Valid reports whether r is one of the stored results.
func (r Result) Valid() bool {
	switch r {
	case ResultOK, ResultMissing, ResultNotCNAME, ResultOtherIP, ResultWrongTarget, ResultLookupFailed, ResultNoSendingIP:
		return true
	}
	return false
}

// Indeterminate results say nothing definite about the customer's DNS: the
// answer could not be had, or there was nothing to compare it with. They are
// retried before an active domain is given up.
func (r Result) Indeterminate() bool { return r == ResultLookupFailed || r == ResultNoSendingIP }

// States of a bounce domain.
const (
	StatePending = "pending"
	StateActive  = "active"
	StateFailing = "failing"
)

// Timing. Freshness is enforced by the claim query (interval '3 hours'); the
// checker rechecks active domains well inside it.
const (
	// Freshness is how long an ok check keeps a domain usable.
	Freshness = 3 * time.Hour
	// ActiveEvery is the recheck period of an active domain.
	ActiveEvery = 30 * time.Minute
	// FailuresToFail is how many indeterminate failures in a row take an
	// active domain out of use.
	FailuresToFail = 2
	// maxFailures bounds the stored counter.
	maxFailures = 1 << 20
)

// Target is what a bounce domain must resolve to for one app.
type Target struct {
	// Name is the HELO name of the app's current sending IP (the pinned IP,
	// else the main IP of the latest edge report); empty when unknown.
	Name string
	// Address is that name's IP, for confirming the name in DNS before it is
	// shown to people.
	Address string
	// Reported lists every HELO name the edge reports, for telling a record
	// that names another of our IPs from one that names something else.
	Reported []string
}

// Check is a check in flight: a row leased under Token. Only a result written
// with the token still current is kept.
type Check struct {
	ID, AppID, Domain, Token string
	Target                   Target
}

// CheckBounce resolves domain and classifies the answer against the target.
// observed is the name the CNAME resolved to (empty when none). The lookup uses
// the rooted name so the resolver's search domains never apply.
func CheckBounce(ctx context.Context, resolver Resolver, domain string, target Target) (result Result, observed string) {
	if target.Name == "" {
		return ResultNoSendingIP, ""
	}
	if resolver == nil {
		return ResultLookupFailed, ""
	}
	canonical, err := resolver.LookupCNAME(ctx, strings.TrimSuffix(domain, ".")+".")
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return ResultMissing, ""
		}
		return ResultLookupFailed, ""
	}
	observed = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(canonical)), ".")
	if len(observed) > 253 {
		observed = observed[:253]
	}
	switch {
	case observed == "" || observed == domain:
		return ResultNotCNAME, ""
	case observed == target.Name:
		return ResultOK, observed
	}
	for _, name := range target.Reported {
		if observed == name {
			return ResultOtherIP, observed
		}
	}
	return ResultWrongTarget, observed
}

// Status is the part of a row the transition reads.
type Status struct {
	State    string
	Failures int
}

// Next applies one check result to a row's status. The table:
//
//	result          pending                  active                       failing
//	ok              active                   active                       active
//	definitive      pending                  failing                      failing
//	indeterminate   pending (failures++)     active, failing at 2 in a row failing
//
// A passing check resets the failure count. Definitive failures (missing,
// not_cname, other_ip, wrong_target) take an active domain out of use at once;
// the claim already stopped using it if the target moved.
func Next(current Status, result Result) Status {
	if result == ResultOK {
		return Status{State: StateActive}
	}
	failures := current.Failures + 1
	if failures > maxFailures {
		failures = maxFailures
	}
	next := Status{State: current.State, Failures: failures}
	if current.State == StateActive && (!result.Indeterminate() || failures >= FailuresToFail) {
		next.State = StateFailing
	}
	return next
}

// Interval is how long to wait before checking again. age is the time since
// the state began: since creation for pending, since the last passing check for
// failing (a domain that has never passed uses its creation time).
//
//	pending: 10 minutes for the first 48 hours, then 6 hours
//	active:  30 minutes
//	failing: 10 minutes for the first 24 hours, then hourly
func Interval(state string, age time.Duration) time.Duration {
	switch state {
	case StateActive:
		return ActiveEvery
	case StateFailing:
		if age < 24*time.Hour {
			return 10 * time.Minute
		}
		return time.Hour
	}
	if age < 48*time.Hour {
		return 10 * time.Minute
	}
	return 6 * time.Hour
}

// StrictSubdomain reports whether name is a proper subdomain of parent.
func StrictSubdomain(name, parent string) bool {
	return parent != "" && len(name) > len(parent)+1 && strings.HasSuffix(name, "."+parent)
}
