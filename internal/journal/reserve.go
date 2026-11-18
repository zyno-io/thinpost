package journal

import (
	"errors"
	"syscall"
)

// storageReserve holds filesystem blocks separately from the bbolt file.
// It cannot reserve capacity in a thin-provisioned backing storage system.
type storageReserve interface {
	arm() error
	release() error
	close() error
}

func (j *Journal) ensureReserve() error {
	if j.reserve == nil {
		return nil
	}
	err := j.reserve.arm()
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return ErrFull
	}
	return err
}

// outcomeUpdate spends the reserve only while committing an outcome or
// recovering interrupted attempts. A successful commit remains reportable if
// the reserve cannot be replenished; no new attempt is admitted until it can.
func (j *Journal) outcomeUpdate(update func() error) error {
	return j.operation(func() error {
		if j.reserve != nil {
			if err := j.reserve.release(); err != nil {
				return err
			}
		}
		if err := update(); err != nil {
			return err
		}
		err := j.ensureReserve()
		if errors.Is(err, ErrFull) {
			return nil
		}
		return err
	})
}
