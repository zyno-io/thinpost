//go:build linux

package edgeworker

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/journal"
	"golang.org/x/sys/unix"
)

type fillingDialer struct {
	dir   string
	fake  fakeDialer
	limit int
}

func (d *fillingDialer) Dial(ctx context.Context, source, address netip.Addr, recipient, mx string) (net.Conn, error) {
	file, err := os.OpenFile(filepath.Join(d.dir, "filler"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	block := make([]byte, 64<<10)
	for written := 0; written < d.limit; written += len(block) {
		_, err = file.Write(block)
		if errors.Is(err, syscall.ENOSPC) {
			return d.fake.Dial(ctx, source, address, recipient, mx)
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("disposable filesystem did not fill within limit")
}

// Runs only on an explicitly supplied disposable, bounded filesystem.
func TestReservedJournalRecordsSMTPAcceptanceOnFullFilesystem(t *testing.T) {
	root := os.Getenv("THINPOST_TEST_DISPOSABLE_FULL_FILESYSTEM")
	if root == "" {
		t.Skip("no disposable filesystem supplied")
	}
	reserveBytes := int64(1 << 20)
	fillerLimit := 16 << 20
	if os.Getenv("THINPOST_TEST_RESERVE_64M") == "1" {
		// The larger test may consume up to 128 MiB. Refuse to run it on a
		// shared parent filesystem or a larger volume even if misconfigured.
		resolved, resolveErr := filepath.EvalSymlinks(root)
		if resolveErr != nil {
			t.Fatal("cannot resolve disposable mount")
		}
		var volume, parent unix.Stat_t
		var capacity unix.Statfs_t
		if unix.Stat(resolved, &volume) != nil || unix.Stat(filepath.Dir(resolved), &parent) != nil || unix.Statfs(resolved, &capacity) != nil || volume.Dev == parent.Dev || capacity.Blocks == 0 || capacity.Bsize <= 0 || capacity.Blocks > uint64((128<<20)/capacity.Bsize) {
			t.Fatal("large reserve test requires a separate filesystem of at most 128 MiB")
		}
		reserveBytes = 64 << 20
		fillerLimit = 128 << 20
	}
	dir, err := os.MkdirTemp(root, "thinpost-worker-reserve-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	journalPath := filepath.Join(dir, "journal.db")
	j, err := journal.OpenWithReserve(journalPath, 3, reserveBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if j != nil {
			_ = j.Close()
		}
	}()
	fence := journal.Fence{ControllerID: "controller", Epoch: 1}
	if err = j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	mime := []byte("Subject: test\r\n\r\nbody\r\n")
	digest := sha256.Sum256(mime)
	start := &controlv1.DeliveryStart{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1,
		MxName: "mx.example", DestinationIp: "8.8.8.8", EnvelopeFrom: "bounce@example.com",
		Recipient: "to@example.net", ExpectedSize: uint64(len(mime)), Sha256: digest[:]}
	dialer := &fillingDialer{dir: dir, limit: fillerLimit}
	worker := &Worker{Journal: j, Dialer: dialer, Recipients: allowRecipients(true), HeloName: "smtp.sender.example"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := worker.Execute(ctx, fence, start, mime, func() bool { return true }, allowStart)
	if err != nil || result == nil || result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED {
		t.Fatal("SMTP acceptance was not durably recorded", result, err)
	}
	if err = <-dialer.fake.serverDone; err != nil {
		t.Fatal(err)
	}
	attempts, err := j.List()
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != journal.Accepted {
		t.Fatal("accepted attempt unavailable for replay", attempts, err)
	}
	if err = os.Remove(filepath.Join(dir, "filler")); err != nil {
		t.Fatal("could not restore disposable space", err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j = nil
	j, err = journal.OpenWithReserve(journalPath, 3, reserveBytes)
	if err != nil {
		t.Fatal("could not reopen the journal after freeing space", err)
	}
	attempts, err = j.List()
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != journal.Accepted {
		t.Fatal("accepted attempt did not survive reopening", attempts, err)
	}
}
