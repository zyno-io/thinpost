package main

import (
	"context"
	"errors"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/customdomain"
	"github.com/zyno-io/thinpost/internal/postgres"
)

// The checker loop keeps customer domains honest. Every replica runs it. Each
// tick, for each kind of domain, it leases a few due rows (the store does this
// with FOR UPDATE SKIP LOCKED and a lease token, inside the runtime gate), runs
// the checks with bounded concurrency, and writes each result back only if its
// token is still the row's. A replica that stops mid-check just lets its lease
// expire; a manual check or an IP change takes the row with a new token, so a
// stale result is dropped. While the deploy gate is closed nothing is claimed
// or written.
//
// A kind plugs in by supplying a claim function. Bounce domains and tracking
// domains are the two kinds; only a replica with tracking configured runs the
// tracking kind, so only such a replica claims tracking rows.
const (
	domainCheckEvery       = 30 * time.Second
	domainCheckBatch       = 10
	domainCheckConcurrency = 4
	// domainCheckTimeout bounds one check (lookups included), inside the
	// store's two-minute lease.
	domainCheckTimeout = 30 * time.Second
)

// domainCheck is one leased check. Run does the lookup and records the result.
type domainCheck func(ctx context.Context) error

// domainKind is one kind of customer domain with due checks.
type domainKind struct {
	// name is a fixed label for logs; never a customer value.
	name  string
	claim func(ctx context.Context, limit int) ([]domainCheck, error)
}

// domainChecker runs the loop; the fields are set so tests can shrink them.
type domainChecker struct {
	kinds       []domainKind
	every       time.Duration
	batch       int
	concurrency int
	timeout     time.Duration
}

func newDomainChecker(kinds ...domainKind) *domainChecker {
	return &domainChecker{kinds: kinds, every: domainCheckEvery, batch: domainCheckBatch, concurrency: domainCheckConcurrency, timeout: domainCheckTimeout}
}

// bounceDomainKind checks custom bounce domains.
func bounceDomainKind(repository *postgres.Repository, resolver customdomain.Resolver) domainKind {
	return domainKind{name: "bounce", claim: func(ctx context.Context, limit int) ([]domainCheck, error) {
		due, err := repository.ClaimDueBounceDomains(ctx, limit)
		if err != nil {
			return nil, err
		}
		checks := make([]domainCheck, 0, len(due))
		for _, item := range due {
			item := item
			checks = append(checks, func(ctx context.Context) error {
				// The lookup is bounded below the check's own timeout so there is
				// still time to record a lookup failure.
				lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				result, observed := customdomain.CheckBounce(lookupCtx, resolver, item.Domain, item.Target)
				cancel()
				_, _, err := repository.FinishBounceCheck(ctx, postgres.BackgroundCheckActor, item, result, observed)
				return err
			})
		}
		return checks, nil
	}}
}

// trackingDomainKind checks custom tracking domains (removed ones too, daily).
// The checker proves the HTTPS route reaches this deployment, so it is built
// only where tracking is configured.
func trackingDomainKind(repository trackingDomainStore, checker domainTrackingChecker) domainKind {
	return domainKind{name: "tracking", claim: func(ctx context.Context, limit int) ([]domainCheck, error) {
		due, err := repository.ClaimDueTrackingDomains(ctx, limit)
		if err != nil {
			return nil, err
		}
		checks := make([]domainCheck, 0, len(due))
		for _, item := range due {
			item := item
			checks = append(checks, func(ctx context.Context) error {
				// The check bounds its own lookups and request; the loop's
				// timeout leaves time to record whatever it found.
				outcome := checker.Check(ctx, item.Domain)
				_, _, err := repository.FinishTrackingCheck(ctx, postgres.BackgroundCheckActor, item, outcome)
				return err
			})
		}
		return checks, nil
	}}
}

// trackingHost is the hostname of the deployment's tracking origin (no port):
// what a custom tracking domain's DNS record points at.
func trackingHost(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("invalid tracking origin")
	}
	return strings.ToLower(u.Hostname()), nil
}

// trackingDomainStore is the part of the repository the tracking kind uses.
type trackingDomainStore interface {
	ClaimDueTrackingDomains(ctx context.Context, limit int) ([]customdomain.Check, error)
	FinishTrackingCheck(ctx context.Context, actor string, check customdomain.Check, outcome customdomain.TrackingOutcome) (admin.TrackingDomain, bool, error)
}

// domainTrackingChecker is what the tracking kind needs from a checker.
type domainTrackingChecker interface {
	Check(ctx context.Context, domain string) customdomain.TrackingOutcome
}

// tick claims and runs one round for every kind. It returns when every
// claimed check has finished.
func (c *domainChecker) tick(ctx context.Context) {
	for _, kind := range c.kinds {
		if ctx.Err() != nil {
			return
		}
		checks, err := kind.claim(ctx, c.batch)
		if err != nil {
			// A closed gate is a deploy in progress, not a fault.
			if ctx.Err() == nil && !errors.Is(err, postgres.ErrRuntimeClosed) {
				log.Printf("%s domain checks unavailable", kind.name)
			}
			continue
		}
		var wg sync.WaitGroup
		slots := make(chan struct{}, c.concurrency)
		for _, run := range checks {
			slots <- struct{}{}
			wg.Add(1)
			go func(run domainCheck) {
				defer wg.Done()
				defer func() { <-slots }()
				checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
				defer cancel()
				if err := run(checkCtx); err != nil && ctx.Err() == nil && !errors.Is(err, postgres.ErrRuntimeClosed) {
					log.Printf("%s domain check could not be recorded", kind.name)
				}
			}(run)
		}
		wg.Wait()
	}
}

// startDomainChecks runs the loop until the returned stop function is called.
func startDomainChecks(parent context.Context, checker *domainChecker) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(checker.every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checker.tick(ctx)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); <-done }) }
}
