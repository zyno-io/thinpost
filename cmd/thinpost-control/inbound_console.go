package main

import (
	"context"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/objectstore"
)

// consoleInboundObjects reads and decrypts a stored inbound message for the
// console. An object that is not there (the bucket's lifecycle rule may delete
// inbound/ before the database retention time) is expired content, not a
// storage failure.
type consoleInboundObjects struct{ store *objectstore.EncryptedStore }

func (o consoleInboundObjects) GetInbound(ctx context.Context, key string, maximum int64) ([]byte, error) {
	data, err := o.store.Get(ctx, key, maximum)
	if objectstore.IsNotFound(err) {
		return nil, admin.ErrContentExpired
	}
	return data, err
}
