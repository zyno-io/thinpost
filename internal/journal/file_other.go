//go:build !linux && !darwin

package journal

import (
	"errors"
	"os"
)

// Retain portable development support. Production's no-follow, ownership and
// hard-link checks are implemented for Linux and macOS.
func openJournalFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	truncate := flags&os.O_TRUNC != 0
	flags &^= os.O_TRUNC
	file, err := os.OpenFile(path, flags, mode)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || current.Mode().Perm()&0077 != 0 || !os.SameFile(opened, current) {
		_ = file.Close()
		return nil, errors.New("journal must be a private regular file")
	}
	if truncate {
		if err = file.Truncate(0); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}
