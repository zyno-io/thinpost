package objectgc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type testStore struct {
	keys    []string
	removed []string
}

func (s *testStore) List(_ context.Context, prefix, after string, limit int) ([]string, error) {
	var result []string
	for _, key := range s.keys {
		if strings.HasPrefix(key, prefix) && key > after {
			result = append(result, key)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}
func (s *testStore) RemoveOlderThan(_ context.Context, key string, _ time.Time) (bool, error) {
	s.removed = append(s.removed, key)
	return true, nil
}

type testRepository struct{ kept, fail string }

func (r testRepository) CollectUnreferenced(ctx context.Context, key string, _ time.Time, remove func(context.Context, string, time.Time) (bool, error)) (bool, error) {
	if key == r.fail {
		return false, errors.New("sensitive object details")
	}
	if key == r.kept {
		return false, nil
	}
	return remove(ctx, key, time.Now())
}

func TestSweepBoundsProgressAndRetriesFailedObject(t *testing.T) {
	const prefix = "final/v1/1000000000/"
	a, b, c := prefix+"00000000000000000000000000000001", prefix+"00000000000000000000000000000002", prefix+"00000000000000000000000000000003"
	store := &testStore{keys: []string{a, b, c, "final/v1/not-a-managed-key", "final/legacy"}}
	repo := testRepository{kept: b, fail: c}
	horizon := time.Now()
	first, err := Sweep(context.Background(), repo, store, "final", "", horizon, 2)
	if err != nil || first.Scanned != 2 || first.Deleted != 1 || first.Kept != 1 || first.Cursor != b || first.Done {
		t.Fatal("first bounded page", first, err)
	}
	failed, err := Sweep(context.Background(), repo, store, "final", first.Cursor, horizon, 2)
	if err == nil || strings.Contains(err.Error(), "sensitive") || failed.Cursor != b || failed.Scanned != 0 {
		t.Fatal("failed candidate was skipped or leaked", failed, err)
	}
	repo.fail = ""
	last, err := Sweep(context.Background(), repo, store, "final", failed.Cursor, horizon, 3)
	if err != nil || !last.Done || last.Scanned != 2 || last.Deleted != 1 || last.Skipped != 1 || len(store.removed) != 2 || store.removed[0] != a || store.removed[1] != c {
		t.Fatal("retry or invalid key handling", last, err)
	}
}

func TestSweepRejectsUnboundedOrCrossPrefixRequests(t *testing.T) {
	for _, test := range []struct {
		kind, cursor string
		limit        int
	}{{"other", "", 1}, {"final", "", 0}, {"final", "", 501}, {"final", "submissions/v1/x", 1}} {
		if _, err := Sweep(context.Background(), testRepository{}, &testStore{}, test.kind, test.cursor, time.Now(), test.limit); err == nil {
			t.Fatal("accepted invalid sweep")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Sweep(ctx, testRepository{}, &testStore{}, "final", "", time.Now(), 1); err == nil {
		t.Fatal("canceled sweep reported completion")
	}
	if _, err := Sweep(context.Background(), testRepository{}, &testStore{}, "final", "", time.Time{}, 1); err == nil {
		t.Fatal("sweep accepted an unset horizon")
	}
}

func TestSweepStopsAtNewerTimestamp(t *testing.T) {
	const old = "final/v1/1000000000/00000000000000000000000000000001"
	const newer = "final/v1/1800000000/00000000000000000000000000000002"
	store := &testStore{keys: []string{old, newer}}
	result, err := Sweep(context.Background(), testRepository{}, store, "final", "", time.Unix(1500000000, 0), 2)
	if err != nil || !result.Done || result.Scanned != 1 || result.Cursor != old || len(store.removed) != 1 {
		t.Fatal("sweep did not stop at the first newer key", result, err)
	}
}

func TestSweepAcceptsInboundPrefix(t *testing.T) {
	if _, err := Sweep(context.Background(), testRepository{}, &testStore{}, "inbound", "", time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Sweep(context.Background(), testRepository{}, &testStore{}, "inbound", "final/v1/x", time.Now(), 1); err == nil {
		t.Fatal("cross-prefix cursor accepted")
	}
}
