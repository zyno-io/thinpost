//go:build linux

package journal

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type linuxReserve struct {
	file  *os.File
	bytes int64
	armed bool
}

func openStorageReserve(path string, size int64) (storageReserve, error) {
	if size <= 0 {
		return nil, errors.New("reserve size must be positive")
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	r := &linuxReserve{file: file, bytes: size}
	if _, err = r.checkFile(); err == nil {
		err = r.arm()
	}
	// Fail before starting SMTP on filesystems that can allocate but cannot
	// release the reserve. Re-arm before returning the worker to service.
	if err == nil {
		err = r.release()
	}
	if err == nil {
		err = r.arm()
	}
	if err == nil {
		var dir *os.File
		dir, err = os.Open(filepath.Dir(path))
		if err == nil {
			err = dir.Sync()
			_ = dir.Close()
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return r, nil
}

func (r *linuxReserve) checkFile() (unix.Stat_t, error) {
	var info unix.Stat_t
	if err := unix.Fstat(int(r.file.Fd()), &info); err != nil {
		return info, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&0077 != 0 || info.Uid != uint32(os.Geteuid()) || info.Nlink != 1 || info.Size > r.bytes {
		return info, errors.New("reserve must be a private, singly linked regular file of the configured size")
	}
	return info, nil
}

func (r *linuxReserve) arm() error {
	info, err := r.checkFile()
	if err != nil {
		return err
	}
	if r.armed && info.Size == r.bytes && info.Blocks >= (r.bytes+511)/512 {
		return nil
	}
	r.armed = false
	if err := unix.Fallocate(int(r.file.Fd()), 0, 0, r.bytes); err != nil {
		return err
	}
	if err := r.file.Sync(); err != nil {
		return err
	}
	info, err = r.checkFile()
	if err != nil {
		return err
	}
	if info.Size != r.bytes || info.Blocks < (r.bytes+511)/512 {
		return errors.New("filesystem did not allocate the reserve")
	}
	r.armed = true
	return nil
}

func (r *linuxReserve) release() error {
	if _, err := r.checkFile(); err != nil {
		return err
	}
	r.armed = false
	if err := unix.Fallocate(int(r.file.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 0, r.bytes); err != nil {
		return err
	}
	return r.file.Sync()
}

func (r *linuxReserve) close() error { return r.file.Close() }
