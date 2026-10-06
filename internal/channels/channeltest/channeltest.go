// Package channeltest provides an in-memory fake channels.Channel for tests
// of code that sends to, or receives from, a channel.
package channeltest

import (
	"context"
	"fmt"
	"sync"

	"github.com/slimslenderslacks/work/internal/channels"
)

// Fake is a scriptable in-memory Channel. Set the exported error/func fields
// before Start; everything else is safe for concurrent use.
type Fake struct {
	// StartErr, SendErr, CloseErr make the corresponding call fail.
	StartErr, SendErr, CloseErr error
	// SendFunc, if set, replaces the default Send behavior (after recording).
	SendFunc func(ctx context.Context, msg channels.OutboundMessage) (string, error)
	// PanicOnSend makes Send panic, to test isolation.
	PanicOnSend bool

	name string

	mu         sync.Mutex
	handler    channels.InboundHandler
	startCtx   context.Context
	started    int
	closed     int
	sent       []channels.OutboundMessage
	nextID     int
	closeOrder *[]string
}

var _ channels.Channel = (*Fake)(nil)

// New returns a Fake named name.
func New(name string) *Fake { return &Fake{name: name} }

// RecordCloseOrderIn makes Close append the channel's name to *order, for
// asserting shutdown ordering across several fakes.
func (f *Fake) RecordCloseOrderIn(order *[]string) *Fake {
	f.closeOrder = order
	return f
}

func (f *Fake) Name() string { return f.name }

func (f *Fake) Start(ctx context.Context, h channels.InboundHandler) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StartErr != nil {
		return f.StartErr
	}
	f.started++
	f.handler = h
	f.startCtx = ctx
	return nil
}

func (f *Fake) Send(ctx context.Context, msg channels.OutboundMessage) (string, error) {
	if f.PanicOnSend {
		panic("channeltest: send panic")
	}
	f.mu.Lock()
	f.sent = append(f.sent, msg)
	f.nextID++
	id := fmt.Sprintf("%s-%d", f.name, f.nextID)
	sendErr, sendFunc := f.SendErr, f.SendFunc
	f.mu.Unlock()

	if sendFunc != nil {
		return sendFunc(ctx, msg)
	}
	if sendErr != nil {
		return "", sendErr
	}
	return id, nil
}

func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	if f.closeOrder != nil {
		*f.closeOrder = append(*f.closeOrder, f.name)
	}
	return f.CloseErr
}

// Inject simulates an inbound message, delivering it to the handler passed to
// Start. It fails if the channel is not started.
func (f *Fake) Inject(msg channels.InboundMessage) error {
	f.mu.Lock()
	h, ctx := f.handler, f.startCtx
	f.mu.Unlock()
	if h == nil {
		return fmt.Errorf("channeltest: %s not started", f.name)
	}
	h(ctx, msg)
	return nil
}

// Sent returns a copy of every message passed to Send, in order.
func (f *Fake) Sent() []channels.OutboundMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]channels.OutboundMessage(nil), f.sent...)
}

// StartCount is how many times Start succeeded.
func (f *Fake) StartCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.started }

// CloseCount is how many times Close was called.
func (f *Fake) CloseCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.closed }
