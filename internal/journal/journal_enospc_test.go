//go:build linux

package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// This destructive-to-free-space test must run on an explicitly supplied,
// disposable filesystem, never on a shared host or live journal volume.
func TestFullFilesystemRequiresReopen(t *testing.T) {
	root := os.Getenv("THINPOST_TEST_DISPOSABLE_FULL_FILESYSTEM")
	if root == "" {
		t.Skip("no disposable filesystem supplied")
	}
	dir, err := os.MkdirTemp(root, "thinpost-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "journal.db")
	j, err := Open(path, 100000)
	if err != nil {
		t.Fatal(err)
	}
	fence := Fence{ControllerID: "controller", Epoch: 1}
	if err = j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	if err = j.Begin("attempt", "delivery", fence, time.Now()); err != nil {
		t.Fatal(err)
	}
	fillerPath := filepath.Join(dir, "filler")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(fillerPath) })
	block := make([]byte, 64<<10)
	for written := 0; written < 16<<20; written += len(block) {
		_, err = filler.Write(block)
		if errors.Is(err, syscall.ENOSPC) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("disposable filesystem did not fill within the 16 MiB limit")
	}
	if err = filler.Close(); err != nil {
		t.Fatal(err)
	}
	// bbolt may reuse already allocated pages, so add records until a real
	// allocation fails while the original attempt remains in flight.
	for i := 0; i < 10000; i++ {
		err = j.Begin(fmt.Sprintf("attempt-%d", i), fmt.Sprintf("delivery-%d", i), fence, time.Now())
		if err != nil {
			break
		}
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("journal did not report the full filesystem", err)
	}
	if err = os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	if _, err = j.Remaining(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("freeing disk revived a faulted journal", err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = Open(path, 100000)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err = j.Recover(time.Now()); err != nil {
		t.Fatal(err)
	}
	attempts, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, attempt := range attempts {
		if attempt.ID == "attempt" && attempt.Outcome == Uncertain {
			found = true
		}
	}
	if !found {
		t.Fatal("in-flight attempt did not survive as uncertain")
	}
}
