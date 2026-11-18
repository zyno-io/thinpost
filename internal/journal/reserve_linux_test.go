//go:build linux

package journal

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReserveRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "symlink")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openStorageReserve(link, 1<<20); err == nil {
		t.Fatal("accepted symlink")
	}
	hardlink := filepath.Join(dir, "hardlink")
	if err := os.Link(target, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := openStorageReserve(hardlink, 1<<20); err == nil {
		t.Fatal("accepted multiply linked file")
	}
	public := filepath.Join(dir, "public")
	if err := os.WriteFile(public, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(public, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := openStorageReserve(public, 1<<20); err == nil {
		t.Fatal("accepted a readable reserve file")
	}
}

func TestReserveFilesystemLifecycle(t *testing.T) {
	root := os.Getenv("THINPOST_TEST_DISPOSABLE_RESERVE_DIR")
	if root == "" {
		t.Skip("no disposable reserve filesystem supplied")
	}
	dir, err := os.MkdirTemp(root, "thinpost-reserve-lifecycle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	reserve, err := openStorageReserve(filepath.Join(dir, "reserve"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer reserve.close()
	var armed, released unix.Statfs_t
	if err = unix.Statfs(dir, &armed); err != nil {
		t.Fatal(err)
	}
	if err = reserve.release(); err != nil {
		t.Fatal(err)
	}
	if err = unix.Statfs(dir, &released); err != nil {
		t.Fatal(err)
	}
	if released.Bavail <= armed.Bavail || (released.Bavail-armed.Bavail)*uint64(released.Bsize) < 32<<20 {
		t.Fatal("reserve release did not return expected filesystem blocks")
	}
	if err = reserve.arm(); err != nil {
		t.Fatal("reserve could not be restored", err)
	}
}

func TestReserveSupportsAnOutcomeOnFullFilesystem(t *testing.T) {
	root := os.Getenv("THINPOST_TEST_DISPOSABLE_FULL_FILESYSTEM")
	if root == "" {
		t.Skip("no disposable filesystem supplied")
	}
	dir, err := os.MkdirTemp(root, "thinpost-reserve-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "journal.db")
	j, err := OpenWithReserve(path, 3, 1<<20)
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
	var info unix.Stat_t
	if err = unix.Stat(path+".reserve", &info); err != nil || info.Blocks*512 < 1<<20 {
		t.Fatal("reserve was not allocated", err)
	}
	fillerPath := filepath.Join(dir, "filler")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
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
	var before, after unix.Statfs_t
	if err = unix.Statfs(dir, &before); err != nil {
		t.Fatal(err)
	}
	if err = j.reserve.release(); err != nil {
		t.Fatal(err)
	}
	if err = unix.Statfs(dir, &after); err != nil {
		t.Fatal(err)
	}
	if after.Bavail <= before.Bavail || (after.Bavail-before.Bavail)*uint64(after.Bsize) < 512<<10 {
		t.Fatal("releasing the reserve did not return filesystem blocks")
	}
	if err = j.reserve.arm(); err != nil {
		t.Fatal("could not restore reserve before completing the outcome", err)
	}
	if err = j.Complete("attempt", Accepted, 250, time.Now()); err != nil {
		t.Fatal("could not commit outcome with the reserved blocks", err)
	}
	attempts, err := j.List()
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != Accepted {
		t.Fatal("accepted result was not retained", attempts, err)
	}
	if err = os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	if remaining, err := j.Remaining(); err != nil || remaining != 2 {
		t.Fatal("reserve did not recover after freeing space", remaining, err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = OpenWithReserve(path, 3, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	attempts, err = j.List()
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != Accepted {
		t.Fatal("accepted result did not survive reopen", attempts, err)
	}
}

// This test fills the supplied disposable filesystem; never point it at a
// shared filesystem or an existing worker journal.
func TestReserveRecoversInterruptedAttemptAfterFullFilesystemRestart(t *testing.T) {
	root := os.Getenv("THINPOST_TEST_DISPOSABLE_FULL_FILESYSTEM")
	if root == "" {
		t.Skip("no disposable filesystem supplied")
	}
	dir, err := os.MkdirTemp(root, "thinpost-restart-full-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "journal.db")
	j, err := OpenWithReserve(path, 3, 1<<20)
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
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	filler, err := os.OpenFile(filepath.Join(dir, "filler"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 4096)
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
	j, err = OpenWithReserve(path, 3, 1<<20)
	if err != nil {
		t.Fatal("could not reopen journal on full filesystem", err)
	}
	defer j.Close()
	if err = j.Recover(time.Now()); err != nil {
		t.Fatal("could not recover interrupted attempt with reserved space", err)
	}
	attempts, err := j.List()
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != Uncertain {
		t.Fatal("interrupted attempt was not preserved as uncertain", attempts, err)
	}
	remaining, err := j.Remaining()
	if err != nil {
		t.Fatal(err)
	}
	if remaining > 0 {
		var info unix.Stat_t
		if err = unix.Stat(path+".reserve", &info); err != nil || info.Blocks*512 < 1<<20 {
			t.Fatal("journal advertised capacity without replenishing its reserve", err)
		}
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "filler")); err != nil {
		t.Fatal(err)
	}
	j, err = OpenWithReserve(path, 3, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	attempts, err = j.List()
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != Uncertain {
		t.Fatal("uncertain result did not survive another restart", attempts, err)
	}
}
