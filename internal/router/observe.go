package router

import (
	"fmt"
	"strings"

	"github.com/slimslenderslacks/work/internal/acpchat"
)

// observe is the wolf-side of rule 4. It reads the wolf conversation's event
// stream for as long as the attachment lives and relays, to every chat bound to
// that wolf, what the wolf says when someone ELSE (the TUI user) drives it. The
// router's own turns are excluded: their reply already went to the asker.
// (Permission requests are handled by askPermission, not here.)
func (r *Router) observe(cv *conv, events <-chan acpchat.Event) {
	for ev := range events {
		switch ev.Kind {
		case acpchat.EventTurnEnd:
			if !ev.Own && strings.TrimSpace(ev.Text) != "" {
				r.relayForeignTurn(cv.tgt, ev.Text)
			}
		case acpchat.EventGone:
			r.wolfEnded(cv)
		}
	}
	r.dropConv(cv)
}

// boundChats lists the chats bound to the wolf session.
func (r *Router) boundChats(tgt target) []*chat {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*chat
	for _, c := range r.chats {
		if c.wolf != nil && c.wolf.Key == tgt.Key && c.wolf.ID == tgt.ID {
			out = append(out, c)
		}
	}
	return out
}

// relayForeignTurn forwards wolf output produced for another client.
func (r *Router) relayForeignTurn(tgt target, text string) {
	for _, c := range r.boundChats(tgt) {
		r.log("observe_relay", c, "work_stream", tgt.WorkStream)
		r.reply(c, tgt, text)
	}
}

// wolfEnded falls every chat bound to the wolf back to the workingman agent and
// tells them.
func (r *Router) wolfEnded(cv *conv) {
	for _, c := range r.unbindWolf(cv.tgt.Key, cv.tgt.ID) {
		r.log("binding_ended", c, "work_stream", cv.tgt.WorkStream)
		r.notify(c, fmt.Sprintf("The wolf for %s has ended. You're back on the workingman agent.", cv.tgt.WorkStream))
	}
}
