package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/slimslenderslacks/work/internal/postoffice"
)

// Receiver tools (§4.5): next_message, reply and list_inbox, for callers
// whose sandbox is a registered recipient (Config.Recipients). They are
// refused for everyone else, and hidden from callers known not to be
// recipients (senderTools).

// inboxKey is the s.waiters key woken when a message for recipient is put.
// It cannot collide with a message id.
func inboxKey(recipient string) string { return "inbox:" + recipient }

// receiver returns the caller's recipient name, or the rejection for a
// caller that is not a registered recipient.
func (s *Server) receiver(c Caller, tool string) (string, *result) {
	name, ok := s.recipientOf(c)
	if !ok {
		s.logf("post-office: %s called %s, which is only for registered recipients", c.Sender.Sandbox, tool)
		return "", rejected(tool + " is only for registered recipients (" + s.knownAgents() + ")")
	}
	return name, nil
}

func (s *Server) knownAgents() string {
	return strings.TrimPrefix(strings.TrimPrefix(s.knownRecipients(), postoffice.RecipientOperator), ", ")
}

type nextMessageIn struct {
	TimeoutSeconds *int `json:"timeout_seconds"`
}

// nextMessage returns the oldest open message addressed to the caller that
// it has not received yet, marking it received, or waits for one (§4.5).
// Store.MarkReceived is a compare-and-set, so each message is returned once,
// even to concurrent calls and across post-office restarts.
func (s *Server) nextMessage(ctx context.Context, req *mcp.CallToolRequest, c Caller, in nextMessageIn) *result {
	name, r := s.receiver(c, "next_message")
	if r != nil {
		return r
	}
	wait, r := s.waitArg(in.TimeoutSeconds, "timeout_seconds", 0)
	if r != nil {
		return r
	}
	return s.block(ctx, req, inboxKey(name), wait,
		func(bool) (*result, bool) { return s.take(name) },
		func() string { return "waiting for messages to " + name })
}

// take receives the oldest unreceived open message for recipient. It
// reports false, with status: empty, when there is none.
func (s *Server) take(recipient string) (*result, bool) {
	open, err := s.cfg.Store.ListOpen()
	if err != nil {
		return rejected("could not read the inbox: " + err.Error()), true
	}
	now := s.now()
	for _, m := range open {
		if m.Recipient != recipient || m.ReceivedAt != nil || m.Expired(now) {
			continue
		}
		if _, claimed, _ := s.cfg.Store.Claimed(m.ID); claimed {
			continue
		}
		got, err := s.cfg.Store.MarkReceived(m.ID, now)
		switch {
		case err == nil:
		case errors.Is(err, postoffice.ErrAlreadyReceived), errors.Is(err, postoffice.ErrAlreadyClosed), errors.Is(err, postoffice.ErrNotFound):
			continue // another call took it, or it closed meanwhile
		default:
			s.logf("post-office: receive %s: %v", m.ID, err)
			return rejected("could not receive " + m.ID + ": " + err.Error()), true
		}
		// A claim that landed between the check above and the mark closed
		// it; there is nothing to act on.
		if _, claimed, _ := s.cfg.Store.Claimed(m.ID); claimed {
			continue
		}
		s.logf("post-office: %s %s received by %s", got.Kind, got.ID, recipient)
		if got.Kind == postoffice.KindNotification {
			s.dismissReceived(got, recipient)
		}
		return messageResult(got), true
	}
	return newResult("empty"), false
}

// dismissReceived closes a notification once its agent recipient has it: it
// needs no reply, and leaving it open would keep it in list_inbox and the
// operator's "To agents" rows until it expired.
func (s *Server) dismissReceived(m postoffice.Message, recipient string) {
	_, err := s.cfg.Store.Claim(postoffice.Resolution{
		ID: m.ID, Outcome: postoffice.OutcomeDismissed,
		Responder: &postoffice.Responder{Kind: postoffice.ResponderAgent, Via: recipient},
	})
	if err != nil && !errors.Is(err, postoffice.ErrAlreadyClosed) {
		s.logf("post-office: dismiss received %s: %v", m.ID, err)
		return
	}
	s.finalize(m.ID)
}

// messageResult renders a received message (§4.5): status: message, its
// fields, and the body last.
func messageResult(m postoffice.Message) *result {
	r := newResult("message").set("kind", string(m.Kind))
	if m.Kind == postoffice.KindRequest {
		r.set("request_id", m.ID)
	} else {
		r.set("message_id", m.ID)
	}
	r.set("from", m.From.Display()).
		set("from_sandbox", m.From.Sandbox).
		set("from_task", m.From.Task).
		set("created_at", stamp(m.CreatedAt)).
		set("expires_at", stamp(m.ExpiresAt)).
		set("subject", m.Subject).
		set("options", m.Options)
	return r.setBlock("body", m.Body)
}

type replyIn struct {
	RequestID string `json:"request_id"`
	Response  string `json:"response"`
}

// reply answers a request addressed to the caller by claiming the outbox
// with an agent responder (§4.5). When another outcome won (the sender
// cancelled, it expired, or the operator answered first) it returns
// status: closed with that outcome.
func (s *Server) reply(_ context.Context, _ *mcp.CallToolRequest, c Caller, in replyIn) *result {
	name, r := s.receiver(c, "reply")
	if r != nil {
		return r
	}
	id := strings.TrimSpace(in.RequestID)
	m, res, r := s.addressed(c, id)
	if r != nil {
		return r
	}
	if m.Kind != postoffice.KindRequest {
		return rejected(id + " is a notification; it needs no reply")
	}
	if strings.TrimSpace(in.Response) == "" {
		return rejected("response is required")
	}
	if res != nil {
		return closedResult(id, *res)
	}
	won, err := s.cfg.Store.Claim(postoffice.Resolution{
		ID: id, Outcome: postoffice.OutcomeAnswered, Response: in.Response,
		Responder: &postoffice.Responder{Kind: postoffice.ResponderAgent, Via: name, Name: name},
	})
	switch {
	case err == nil:
		s.logf("post-office: request %s answered by %s", id, name)
		s.finalize(id)
		return newResult("answered").set("request_id", id).set("option", won.Option)
	case errors.Is(err, postoffice.ErrAlreadyClosed):
		s.finalize(id)
		if _, res, lerr := s.cfg.Store.Lookup(id); lerr == nil && res != nil {
			won = *res
		}
		return closedResult(id, won)
	default:
		s.logf("post-office: reply %s: %v", id, err)
		return rejected("could not reply: " + err.Error())
	}
}

// addressed looks up a message addressed to the caller. res is its
// resolution when it is closed. A non-nil result is the rejection.
func (s *Server) addressed(c Caller, id string) (postoffice.Message, *postoffice.Resolution, *result) {
	if !postoffice.ValidID(id) {
		return postoffice.Message{}, nil, rejected(fmt.Sprintf("invalid request_id %q", id))
	}
	m, res, err := s.cfg.Store.Lookup(id)
	if errors.Is(err, postoffice.ErrNotFound) {
		return postoffice.Message{}, nil, rejected("unknown request_id " + id + "; call list_inbox to see yours")
	}
	if err != nil {
		return postoffice.Message{}, nil, rejected("could not read the request: " + err.Error())
	}
	if !s.isRecipient(m, c) {
		s.logf("post-office: %s asked about %s, addressed to %s", c.Sender.Sandbox, id, m.Recipient)
		return postoffice.Message{}, nil, rejected("not addressed to you")
	}
	return m, res, nil
}

// closedResult reports the outcome that closed a request before a reply.
func closedResult(id string, res postoffice.Resolution) *result {
	r := newResult("closed").set("request_id", id).set("outcome", string(res.Outcome))
	if res.Responder != nil {
		r.set("responder", res.Responder.Kind).set("via", res.Responder.Via)
	}
	return r.set("reason", res.Reason)
}

// listInbox lists the open messages addressed to the caller, received or
// not (§4.5). Messages already claimed or past their expiry are left out:
// they can no longer be answered.
func (s *Server) listInbox(_ context.Context, _ *mcp.CallToolRequest, c Caller, _ struct{}) *result {
	name, r := s.receiver(c, "list_inbox")
	if r != nil {
		return r
	}
	open, err := s.cfg.Store.ListOpen()
	if err != nil {
		return rejected("could not read the inbox: " + err.Error())
	}
	now := s.now()
	r = newResult("ok")
	var structured []map[string]string
	for _, m := range open {
		// A received notification is being dismissed (dismissReceived).
		if m.Recipient != name || m.Expired(now) || (m.Kind == postoffice.KindNotification && m.ReceivedAt != nil) {
			continue
		}
		if _, claimed, _ := s.cfg.Store.Claimed(m.ID); claimed {
			continue
		}
		state := "new"
		if m.ReceivedAt != nil {
			state = "received"
		}
		r.addLine(strings.Join([]string{m.ID, string(m.Kind), state, m.From.Sandbox, stamp(m.ExpiresAt), oneLine(m.Subject)}, " "))
		structured = append(structured, map[string]string{
			"id": m.ID, "kind": string(m.Kind), "state": state, "from": m.From.Display(),
			"from_sandbox": m.From.Sandbox, "expires_at": stamp(m.ExpiresAt), "subject": m.Subject,
		})
	}
	if len(structured) == 0 {
		r.addLine("(no messages)")
	}
	r.values["messages"] = structured
	return r
}

// inReplyTo decides send_request's in_reply_to for the caller (§2.9, §8.4).
// A registered recipient may name a request addressed to it that is still
// open; with none named, the link is made to the one request it has
// received and not answered, if it holds exactly one. Holding several is
// ambiguous, so the send is refused until it names one. note explains an
// automatic link. Other callers may not set it.
func (s *Server) inReplyTo(c Caller, explicit string) (id, note string, r *result) {
	explicit = strings.TrimSpace(explicit)
	name, isRecipient := s.recipientOf(c)
	if !isRecipient {
		if explicit != "" {
			return "", "", rejected("in_reply_to is only for registered recipients")
		}
		return "", "", nil
	}
	if explicit != "" {
		m, res, r := s.addressed(c, explicit)
		switch {
		case r != nil:
			return "", "", rejected("in_reply_to: " + r.values["reason"].(string))
		case m.Kind != postoffice.KindRequest:
			return "", "", rejected("in_reply_to: " + explicit + " is a notification, not a request")
		case res != nil:
			return "", "", rejected(fmt.Sprintf("in_reply_to: %s is already closed (%s)", explicit, res.Outcome))
		}
		return explicit, "", nil
	}
	held, err := s.held(name)
	if err != nil {
		return "", "", rejected("could not read the inbox: " + err.Error())
	}
	switch len(held) {
	case 0:
		return "", "", nil
	case 1:
		return held[0], "in_reply_to set to " + held[0] + ", the request you hold", nil
	}
	return "", "", rejected(fmt.Sprintf("you hold %d received requests (%s); pass in_reply_to with the one this request serves",
		len(held), strings.Join(held, ", ")))
}

// held returns the ids of the open requests recipient has received and not
// answered.
func (s *Server) held(recipient string) ([]string, error) {
	open, err := s.cfg.Store.ListOpen()
	if err != nil {
		return nil, err
	}
	now := s.now()
	var ids []string
	for _, m := range open {
		if m.Recipient != recipient || m.Kind != postoffice.KindRequest || m.ReceivedAt == nil || m.Expired(now) {
			continue
		}
		if _, claimed, _ := s.cfg.Store.Claimed(m.ID); claimed {
			continue
		}
		ids = append(ids, m.ID)
	}
	return ids, nil
}
