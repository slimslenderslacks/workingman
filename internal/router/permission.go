package router

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/acpchat"
	"github.com/slimslenderslacks/work/internal/audit"
)

// permission is one agent permission request waiting on the human.
type permission struct {
	req   acpchat.PermissionRequest
	tgt   target
	done  chan acpchat.PermissionDecision // buffered; written once
	once  sync.Once
	chats []*chat
}

// resolve delivers the decision (first call wins) and clears the request from
// every chat it was put to.
func (r *Router) resolve(p *permission, d acpchat.PermissionDecision) {
	p.once.Do(func() {
		p.done <- d
		r.mu.Lock()
		for _, c := range p.chats {
			if c.pending == p {
				c.pending = nil
			}
		}
		r.mu.Unlock()
	})
}

// askPermission is acpchat's OnPermission for the conversation with tgt: it puts
// the request to the chat(s) involved and waits for an answer, rejecting on
// timeout, shutdown, or when there is nobody to ask.
func (r *Router) askPermission(ctx context.Context, tgt target, req acpchat.PermissionRequest) acpchat.PermissionDecision {
	p := &permission{req: req, tgt: tgt, done: make(chan acpchat.PermissionDecision, 1)}

	// Who to ask: the chats with a turn in flight on this agent, else (for a wolf)
	// the chats bound to it. A chat already waiting on another request is skipped
	// — one question at a time.
	r.mu.Lock()
	seen := map[*chat]bool{}
	add := func(c *chat) {
		if c.pending == nil && !seen[c] {
			seen[c] = true
			c.pending = p
			p.chats = append(p.chats, c)
		}
	}
	for c := range r.active[tgt.Key] {
		add(c)
	}
	if len(p.chats) == 0 && tgt.wolf {
		for _, c := range r.chats {
			if c.wolf != nil && c.wolf.Key == tgt.Key && c.wolf.ID == tgt.ID {
				add(c)
			}
		}
	}
	chats := append([]*chat(nil), p.chats...)
	r.mu.Unlock()

	if len(chats) == 0 {
		r.audit("permission_rejected", "reason", "no_chat", "kind", kindOf(tgt))
		return req.Reject()
	}
	prompt := permissionPrompt(tgt, req, r.cfg.PermissionTimeout)
	for _, c := range chats {
		r.log("permission_asked", c, "kind", kindOf(tgt))
		r.send(c, tgt.label(), prompt, nil)
	}

	select {
	case d := <-p.done:
		return d
	case <-ctx.Done():
	case <-r.ctx.Done():
	}
	// Timed out (or shut down): reject, and tell the chat so the human knows the
	// agent was not allowed to proceed.
	r.resolve(p, req.Reject())
	for _, c := range chats {
		r.log("permission_timeout", c, "kind", kindOf(tgt))
		if r.ctx.Err() == nil {
			r.send(c, tgt.label(), "No answer in time, so I rejected that request.", nil)
		}
	}
	return req.Reject()
}

// permissionPrompt renders the question. The title may carry tool output, so it
// is redacted and shortened.
func permissionPrompt(tgt target, req acpchat.PermissionRequest, timeout time.Duration) string {
	var b strings.Builder
	title := shorten(audit.Redact(req.Title), 300)
	if title == "" {
		title = "run a tool"
	}
	fmt.Fprintf(&b, "🔐 Permission needed: %s\n", title)
	for i, o := range req.Options {
		fmt.Fprintf(&b, "%d) %s\n", i+1, shorten(audit.Redact(o.Name), 80))
	}
	fmt.Fprintf(&b, "Reply yes or no (or an option number). I'll reject it if there's no answer in %s.", timeout.Round(time.Second))
	return b.String()
}

var (
	yesRE    = regexp.MustCompile(`^(y|yes|yep|yeah|ok|okay|sure|allow|approve|approved|go|go ahead|do it)$`)
	alwaysRE = regexp.MustCompile(`^(always|allow always|yes always)$`)
	noRE     = regexp.MustCompile(`^(n|no|nope|nah|deny|denied|reject|rejected|stop|cancel|don'?t)$`)
)

// answerPermission treats text as the answer to the permission request pending
// for c, if any. It reports whether the message was consumed.
func (r *Router) answerPermission(c *chat, text string) bool {
	r.mu.Lock()
	p := c.pending
	r.mu.Unlock()
	if p == nil {
		return false
	}
	d, ok := parseDecision(p.req, text)
	if !ok {
		r.notify(c, "Please reply yes or no (or an option number) to the permission request above.")
		return true
	}
	allowed := isAllow(p.req, d)
	r.log("permission_answered", c, "allowed", boolStr(allowed))
	r.resolve(p, d)
	if allowed {
		r.notify(c, "👍 Allowed.")
	} else {
		r.notify(c, "🚫 Rejected.")
	}
	return true
}

// parseDecision maps a human reply to one of the request's options.
func parseDecision(req acpchat.PermissionRequest, text string) (acpchat.PermissionDecision, bool) {
	t := strings.ToLower(strings.Trim(strings.TrimSpace(text), ".!? "))
	if n, err := strconv.Atoi(t); err == nil {
		if n >= 1 && n <= len(req.Options) {
			return acpchat.PermissionDecision{OptionID: req.Options[n-1].ID}, true
		}
		return acpchat.PermissionDecision{}, false
	}
	switch {
	case alwaysRE.MatchString(t):
		for _, o := range req.Options {
			if o.Kind == "allow_always" {
				return acpchat.PermissionDecision{OptionID: o.ID}, true
			}
		}
		return req.Allow(), true
	case yesRE.MatchString(t):
		return req.Allow(), true
	case noRE.MatchString(t):
		return req.Reject(), true
	}
	for _, o := range req.Options { // the option's own name, e.g. "Allow once"
		if strings.EqualFold(strings.TrimSpace(o.Name), t) {
			return acpchat.PermissionDecision{OptionID: o.ID}, true
		}
	}
	return acpchat.PermissionDecision{}, false
}

// isAllow reports whether d selects one of the request's allow options.
func isAllow(req acpchat.PermissionRequest, d acpchat.PermissionDecision) bool {
	for _, o := range req.Options {
		if o.ID == d.OptionID {
			return strings.HasPrefix(o.Kind, "allow")
		}
	}
	return false
}
