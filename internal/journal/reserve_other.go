//go:build !linux

package journal

import "errors"

func openStorageReserve(string, int64) (storageReserve, error) {
	return nil, errors.New("journal storage reserve requires Linux")
}
