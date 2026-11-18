//go:build linux || darwin

package journal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestJournalOpenRejectsLinksBeforeModifyingTheirTargets(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	original := []byte("unrelated private data")
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}

	symlink := filepath.Join(dir, "symlink")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	// Exercise the bbolt open callback directly as well as Open: the callback
	// must reject a swap after the earlier path inspection.
	if f, err := openJournalFile(symlink, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600); err == nil {
		_ = f.Close()
		t.Fatal("open callback followed a symlink")
	}
	if j, err := Open(symlink, 1); err == nil {
		_ = j.Close()
		t.Fatal("journal followed a symlink")
	}

	hardlink := filepath.Join(dir, "hardlink")
	if err := os.Link(target, hardlink); err != nil {
		t.Fatal(err)
	}
	if f, err := openJournalFile(hardlink, os.O_RDWR|os.O_TRUNC, 0600); err == nil {
		_ = f.Close()
		t.Fatal("open callback truncated a multiply linked file")
	}
	if j, err := Open(hardlink, 1); err == nil {
		_ = j.Close()
		t.Fatal("journal opened a multiply linked file")
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("rejected journal changed its target", err)
	}
}

func TestJournalOpenDefersTruncationUntilAfterValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copy")
	if err := os.WriteFile(path, []byte("previous data"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := openJournalFile(path, os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatal("validated file was not truncated", err)
	}
}

func TestJournalOpenRejectsDanglingSymlinkWithoutCreatingItsTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "not-created")
	link := filepath.Join(dir, "journal")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openJournalFile(link, os.O_RDWR|os.O_CREATE, 0600); err == nil {
		_ = f.Close()
		t.Fatal("open callback followed a dangling symlink")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("open callback created the symlink target", err)
	}
}

func TestJournalOpenRejectsAnotherOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing a file owner requires a privileged local test")
	}
	path := filepath.Join(t.TempDir(), "foreign-owner")
	original := []byte("unrelated private data")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 65534, -1); err != nil {
		t.Fatal(err)
	}
	if j, err := Open(path, 1); err == nil {
		_ = j.Close()
		t.Fatal("journal opened a file owned by another user")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("rejected journal changed the foreign-owned file", err)
	}
}
