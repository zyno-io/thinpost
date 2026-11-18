//go:build linux || darwin

package journal

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Open without following the final symlink and validate the opened inode,
// before bbolt can initialize or modify it. The journal directory remains a
// trusted, access-controlled boundary; this is not distributed worker fencing.
func openJournalFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	// bbolt also uses this callback for CopyFile, which requests O_TRUNC.
	// Defer truncation until after validating the opened inode.
	truncate := flags&os.O_TRUNC != 0
	flags &^= os.O_TRUNC
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var info unix.Stat_t
	if err = unix.Fstat(fd, &info); err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&0077 != 0 || info.Uid != uint32(os.Geteuid()) || info.Nlink != 1 {
		_ = file.Close()
		return nil, errors.New("journal must be a private, singly linked regular file owned by the worker")
	}
	if truncate {
		if err = file.Truncate(0); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}
