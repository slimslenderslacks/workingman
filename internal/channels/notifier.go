package channels

import (
	"context"
	"time"

	"github.com/slimslenderslacks/work/internal/notify"
)

// DefaultNotifyTimeout bounds one notification fan-out; notify.Sender has no
// context parameter, so the Notifier supplies its own deadline.
const DefaultNotifyTimeout = 15 * time.Second

// Notifier adapts a Registry to notify.Sender and notify.TopicSender so the
// daemon can notify messaging channels through the same plumbing as macOS
// notifications — typically wrapped with the Osascript sender in notify.Multi:
//
//	daemon.WithNotifier(notify.NewMulti(&notify.Osascript{}, channels.NewNotifier(reg)))
//
// Plain Send uses an empty topic and so reaches only wildcard ("*") routes.
type Notifier struct {
	reg     *Registry
	timeout time.Duration
}

var (
	_ notify.Sender      = (*Notifier)(nil)
	_ notify.TopicSender = (*Notifier)(nil)
)

// NewNotifier returns a Notifier over reg with DefaultNotifyTimeout.
func NewNotifier(reg *Registry) *Notifier {
	return &Notifier{reg: reg, timeout: DefaultNotifyTimeout}
}

// WithTimeout overrides the per-notification deadline (<=0 keeps the default).
func (n *Notifier) WithTimeout(d time.Duration) *Notifier {
	if d > 0 {
		n.timeout = d
	}
	return n
}

func (n *Notifier) Send(title, message string) error {
	return n.SendTopic("", title, message)
}

func (n *Notifier) SendTopic(topic, title, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), n.timeout)
	defer cancel()
	_, err := n.reg.SendTopic(ctx, topic, notificationText(title, message))
	return err
}

func notificationText(title, message string) string {
	switch {
	case title == "":
		return message
	case message == "":
		return title
	}
	return title + "\n" + message
}
