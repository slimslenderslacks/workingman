package router

import (
	"fmt"
	"regexp"
	"strings"
)

var commandRE = regexp.MustCompile(`^/([A-Za-z]+)(?:\s+(.*))?$`)

// parseCommand recognises "/name args". A path ("/etc/hosts") is not a command.
func parseCommand(text string) (name, args string, ok bool) {
	m := commandRE.FindStringSubmatch(text)
	if m == nil {
		return "", "", false
	}
	return strings.ToLower(m[1]), strings.TrimSpace(m[2]), true
}

const helpText = `Commands:
/help – this list
/status – one-line summary of projects, sessions and tasks
/wolf [work-stream] – talk to a running wolf (the only one, or the named project's)
/agent – talk to the workingman agent again
/who – who this chat is talking to

Anything else goes to the workingman agent, or to the wolf you chose with /wolf. Reply to a wolf's message to talk to that wolf.`

// handleCommand answers a router command. Commands never queue behind a running
// turn and never reach an LLM.
func (r *Router) handleCommand(c *chat, name, args string) {
	r.log("command", c, "name", name)
	switch name {
	case "help", "start":
		r.notify(c, helpText)
	case "status":
		r.notify(c, r.cfg.Source.Status())
	case "agent":
		r.mu.Lock()
		c.wolf = nil
		r.mu.Unlock()
		r.notify(c, "You're talking to the workingman agent.")
	case "who":
		r.cmdWho(c)
	case "wolf":
		r.cmdWolf(c, args)
	default:
		r.notify(c, fmt.Sprintf("Unknown command /%s. Try /help.", name))
	}
}

func (r *Router) cmdWho(c *chat) {
	_, bound, ended := r.boundWolf(c) // drops (and announces) a binding whose wolf has ended
	if ended {
		return
	}
	if bound {
		r.mu.Lock()
		ws := c.wolf.WorkStream
		r.mu.Unlock()
		r.notify(c, fmt.Sprintf("You're talking to the wolf for %s. /agent to switch back.", ws))
		return
	}
	r.notify(c, "You're talking to the workingman agent. /wolf to talk to a wolf.")
}

// cmdWolf binds the chat to a wolf.
func (r *Router) cmdWolf(c *chat, arg string) {
	wolves := r.cfg.Source.WolfSessions()
	var pick *Session
	switch {
	case len(wolves) == 0:
		r.notify(c, "No wolf running.")
		return
	case arg == "":
		if len(wolves) > 1 {
			r.notify(c, "Several wolves are running: "+wolfNames(wolves)+". Use /wolf <work-stream>.")
			return
		}
		pick = &wolves[0]
	default:
		matches := matchWolves(wolves, arg)
		switch len(matches) {
		case 0:
			r.notify(c, fmt.Sprintf("No wolf running for %q. Running: %s.", arg, wolfNames(wolves)))
			return
		case 1:
			pick = &matches[0]
		default:
			r.notify(c, fmt.Sprintf("%q matches several wolves: %s. Be more specific.", arg, wolfNames(matches)))
			return
		}
	}
	if pick.Ref == "" {
		r.notify(c, fmt.Sprintf("The wolf for %s runs on the host (not as an ACP session), so I can't talk to it.", pick.WorkStream))
		return
	}

	r.mu.Lock()
	c.wolf = pick
	r.mu.Unlock()
	r.log("bind", c, "work_stream", pick.WorkStream)
	r.notify(c, fmt.Sprintf("Now talking to the wolf for %s. Its messages are labelled [wolf %s]; you'll also see what it says to others, and its permission requests. /agent to switch back.", pick.WorkStream, pick.WorkStream))

	// Attach now so observe mode is live before the first message.
	tgt := target{wolf: true, Session: *pick}
	r.goFn(func() {
		if _, err := r.conversation(r.ctx, tgt); err != nil && r.ctx.Err() == nil {
			r.log("attach_error", c, "work_stream", pick.WorkStream, "err", shorten(err.Error(), 200))
			r.notify(c, fmt.Sprintf("I couldn't attach to the wolf for %s yet (%s). I'll retry when you send a message.", pick.WorkStream, shorten(err.Error(), 120)))
		}
	})
}

// matchWolves returns the wolves whose work stream equals arg (case-insensitive),
// else those that contain it.
func matchWolves(wolves []Session, arg string) []Session {
	arg = strings.ToLower(arg)
	var exact, partial []Session
	for _, w := range wolves {
		ws := strings.ToLower(w.WorkStream)
		switch {
		case ws == arg:
			exact = append(exact, w)
		case strings.Contains(ws, arg):
			partial = append(partial, w)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return partial
}

func wolfNames(wolves []Session) string {
	names := make([]string, len(wolves))
	for i, w := range wolves {
		names[i] = w.WorkStream
	}
	return strings.Join(names, ", ")
}
