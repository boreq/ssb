package blobstore

import (
	"context"
	refs "go.mindeco.de/ssb-refs"
	"sync"
)

type PushBlobPubsub struct {
	subscriptions []channelWithContext
	lock          sync.Mutex
}

func NewPushBlobPubsub() *PushBlobPubsub {
	return &PushBlobPubsub{}
}

func (g *PushBlobPubsub) Subscribe(ctx context.Context) <-chan refs.BlobRef {
	ch := make(chan refs.BlobRef)

	g.addSubscription(ctx, ch)

	go func() {
		<-ctx.Done()
		g.removeSubscription(ch)
		close(ch)
	}()

	return ch
}

func (g *PushBlobPubsub) Publish(value refs.BlobRef) {
	g.lock.Lock()
	defer g.lock.Unlock()

	for _, sub := range g.subscriptions {
		select {
		case sub.Ch <- value:
		case <-sub.Ctx.Done():
		}
	}
}

func (g *PushBlobPubsub) addSubscription(ctx context.Context, ch chan refs.BlobRef) {
	g.lock.Lock()
	defer g.lock.Unlock()

	g.subscriptions = append(g.subscriptions, channelWithContext{Ch: ch, Ctx: ctx})
}

func (g *PushBlobPubsub) removeSubscription(ch chan refs.BlobRef) {
	g.lock.Lock()
	defer g.lock.Unlock()

	for i := range g.subscriptions {
		if g.subscriptions[i].Ch == ch {
			g.subscriptions = append(g.subscriptions[:i], g.subscriptions[i+1:]...)
			return
		}
	}

	panic("somehow the subscription was already removed, this must be a bug")
}

type channelWithContext struct {
	Ch  chan refs.BlobRef
	Ctx context.Context
}
