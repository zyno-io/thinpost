package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/zyno-io/thinpost/internal/unsubforward"
	"github.com/zyno-io/thinpost/internal/webhook"
)

// unsubscribeForwardWorkers bounds concurrent forwards so one slow sender
// endpoint cannot block the others. Claims use SKIP LOCKED, so loops never
// collide.
const unsubscribeForwardWorkers = 2

// startUnsubscribeForwards runs the workers that send a recipient's one-click
// unsubscribe to the sender's own link (see package unsubforward). They need
// no keys (they read the stored message through the object store) and use the webhook sender's public-destination rules with no
// internal exceptions. They log neither the link, the response nor any error.
func startUnsubscribeForwards(parent context.Context, repository unsubforward.Repository, objects unsubforward.Objects) func() {
	if objects == nil {
		// Without the object store the stored message cannot be read.
		return func() {}
	}
	dispatcher := &unsubforward.Dispatcher{Repository: repository, Sender: unsubforward.NewSender(), Objects: objects}
	ctx, cancel := context.WithCancel(parent)
	var group sync.WaitGroup
	for range unsubscribeForwardWorkers {
		group.Add(1)
		go func() {
			defer group.Done()
			_ = webhook.Run(ctx, dispatcher, 3*time.Second, func() { log.Print("unsubscribe forwarding unavailable; retrying") })
		}()
	}
	var once sync.Once
	return func() { once.Do(func() { cancel(); group.Wait() }) }
}
