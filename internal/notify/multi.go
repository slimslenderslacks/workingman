package notify

import (
	"errors"
	"fmt"
	"sync"
)

// Multi fans a notification out to several senders — typically the macOS
// Osascript sender plus the messaging-channel notifier — so the daemon can
// notify every destination through a single notify.Sender without changing
// existing callers.
//
// Delivery is isolated: every sender is invoked regardless of whether
// another fails or panics, senders run concurrently so a slow destination
// cannot delay the rest, and all failures are returned joined (errors.Join)
// with the failing sender's index for context.
type Multi struct {
	senders []Sender
}

var (
	_ Sender      = (*Multi)(nil)
	_ TopicSender = (*Multi)(nil)
)

// NewMulti builds a Multi over senders. Nil entries are skipped.
func NewMulti(senders ...Sender) *Multi {
	m := &Multi{}
	for _, s := range senders {
		if s != nil {
			m.senders = append(m.senders, s)
		}
	}
	return m
}

// Len reports how many senders the fan-out targets.
func (m *Multi) Len() int { return len(m.senders) }

// Send delivers a topic-less notification to every sender.
func (m *Multi) Send(title, message string) error {
	return m.fanOut(func(s Sender) error { return s.Send(title, message) })
}

// SendTopic delivers a topic-tagged notification to every sender. Senders
// that implement TopicSender receive the topic; the rest get a plain Send.
func (m *Multi) SendTopic(topic, title, message string) error {
	return m.fanOut(func(s Sender) error { return SendTopic(s, topic, title, message) })
}

func (m *Multi) fanOut(send func(Sender) error) error {
	errs := make([]error, len(m.senders))
	var wg sync.WaitGroup
	for i, s := range m.senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs[i] = fmt.Errorf("panic: %v", r)
				}
			}()
			errs[i] = send(s)
		}()
	}
	wg.Wait()

	var out []error
	for i, err := range errs {
		if err != nil {
			out = append(out, fmt.Errorf("notify: sender %d (%T): %w", i, m.senders[i], err))
		}
	}
	return errors.Join(out...)
}
