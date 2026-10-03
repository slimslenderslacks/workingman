package notify

import "fmt"

// Async wraps a sender so that Send/SendTopic return immediately and delivery
// happens on its own goroutine. The daemon calls its notifier from the
// dispatch path, so a slow destination (a messaging channel waiting on the
// network) must not be able to stall it. Delivery errors and panics go to the
// onError callback instead of the caller.
type Async struct {
	inner   Sender
	onError func(error)
}

var (
	_ Sender      = (*Async)(nil)
	_ TopicSender = (*Async)(nil)
)

// NewAsync returns an Async over inner. onError may be nil.
func NewAsync(inner Sender, onError func(error)) *Async {
	return &Async{inner: inner, onError: onError}
}

func (a *Async) Send(title, message string) error {
	a.run(func() error { return a.inner.Send(title, message) })
	return nil
}

func (a *Async) SendTopic(topic, title, message string) error {
	a.run(func() error { return SendTopic(a.inner, topic, title, message) })
	return nil
}

func (a *Async) run(send func() error) {
	go func() {
		var err error
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
			if err != nil && a.onError != nil {
				a.onError(err)
			}
		}()
		err = send()
	}()
}
