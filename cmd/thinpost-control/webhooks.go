package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/webhook"
)

// webhookWorkers bounds concurrent deliveries so one slow receiver cannot
// block the others. Claims use SKIP LOCKED, so loops never collide.
const webhookWorkers = 4

// startWebhooks runs the dispatchers whenever the object keyring is available;
// without it no secret can be opened and the returned stop is a no-op. The
// inbound lane also needs the object store, to read received messages. Only
// the outbound lane honors internal destinations: inbound payloads carry
// message content and are never sent over a plain in-cluster exception. The
// dispatchers log no destination, event content, or secret values.
func startWebhooks(parent context.Context, repository *postgres.Repository, keys webhook.Keys, objects *objectstore.EncryptedStore, destinations *admin.WebhookDestinations) func() {
	if keys == nil {
		return func() {}
	}
	stopEvents := runWebhooks(parent, repository, keys, destinations)
	if objects == nil {
		return stopEvents
	}
	stopInbound := runInboundWebhooks(parent, repository, keys, inboundObjects{objects})
	return func() { stopEvents(); stopInbound() }
}

// inboundObjects reads and decrypts a stored inbound message, telling a
// message that is gone from a storage failure.
type inboundObjects struct{ store *objectstore.EncryptedStore }

func (o inboundObjects) GetMIME(ctx context.Context, key string, maximum int64) ([]byte, error) {
	data, err := o.store.Get(ctx, key, maximum)
	if objectstore.IsNotFound(err) {
		return nil, webhook.ErrContentMissing
	}
	return data, err
}

// runInboundWebhooks runs the inbound lane: its own workers, one render budget
// shared by them, and a sender with the longer deadline and the larger body.
func runInboundWebhooks(parent context.Context, repository webhook.InboundRepository, keys webhook.Keys, objects webhook.Objects) func() {
	dispatcher := &webhook.InboundDispatcher{Repository: repository, Sender: &webhook.Sender{MaxPayload: webhook.MaxInboundPayload, Timeout: webhook.InboundTimeout}, Keys: keys, Objects: objects, Budget: webhook.NewRenderBudget(webhook.RenderBudgetBytes)}
	ctx, cancel := context.WithCancel(parent)
	var group sync.WaitGroup
	for range webhook.InboundWorkers {
		group.Add(1)
		go func() {
			defer group.Done()
			_ = webhook.Run(ctx, dispatcher, 3*time.Second, func() { log.Print("inbound webhook dispatcher unavailable; retrying") })
		}()
	}
	var once sync.Once
	return func() { once.Do(func() { cancel(); group.Wait() }) }
}

func runWebhooks(parent context.Context, repository webhook.Repository, keys webhook.Keys, destinations *admin.WebhookDestinations) func() {
	dispatcher := &webhook.Dispatcher{Repository: repository, Sender: &webhook.Sender{Destinations: destinations}, Keys: keys}
	ctx, cancel := context.WithCancel(parent)
	var group sync.WaitGroup
	for range webhookWorkers {
		group.Add(1)
		go func() {
			defer group.Done()
			_ = webhook.Run(ctx, dispatcher, 3*time.Second, func() { log.Print("webhook dispatcher unavailable; retrying") })
		}()
	}
	var once sync.Once
	return func() { once.Do(func() { cancel(); group.Wait() }) }
}
