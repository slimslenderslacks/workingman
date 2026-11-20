package router

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/postoffice"
)

// PostOffice is the message store as the router uses it: answering an
// operator request from a channel, and listing the open ones.
// *postoffice.Store implements it.
type PostOffice interface {
	Claim(r postoffice.Resolution) (postoffice.Resolution, error)
	ListOpen() ([]postoffice.Message, error)
	Lookup(id string) (postoffice.Message, *postoffice.Resolution, error)
}

// shortAnswerRE matches a top-level answer by short id: "#r1t8w staging".
var shortAnswerRE = regexp.MustCompile(`(?s)^#([0-9A-Za-z]{6})(?:\s+(.*))?$`)

// answerPostOffice handles a message that answers a post-office request — a
// reply to the request's channel message, or a top-level "#<short> answer" —
// and reports whether it did. Claims are quick file operations, so they never
// queue behind a running turn.
func (r *Router) answerPostOffice(c *chat, msg channels.InboundMessage, text string) bool {
	if msg.ReplyToID != "" && r.cfg.Index != nil {
		if ct, ok := r.cfg.Index.Lookup(c.channel, msg.ReplyToID); ok && ct.Kind == channels.ConversationKindPostOffice {
			r.claimRequest(c, msg, ct.SessionKey, ct.WorkStream, text)
			return true
		}
	}
	if r.cfg.PostOffice == nil {
		return false
	}
	m := shortAnswerRE.FindStringSubmatch(text)
	if m == nil {
		return false
	}
	short, answer := strings.ToLower(m[1]), strings.TrimSpace(m[2])
	open, err := r.openRequests()
	if err != nil {
		r.log("postoffice_error", c, "err", shorten(err.Error(), 200))
		r.notify(c, "I couldn't read the post-office inbox, so I couldn't pass your answer on. Try again in a moment.")
		return true
	}
	req, found, err := postoffice.MatchShortID(open, short)
	switch {
	case errors.Is(err, postoffice.ErrAmbiguousShortID):
		r.log("postoffice_ambiguous", c)
		r.notify(c, fmt.Sprintf("#%s matches several open requests. Reply to the request's message instead.", short))
		return true
	case !found:
		// Not an open request: perhaps one that has just closed, else ordinary
		// text that happens to start with a hashtag.
		if closed, ok := r.recentlyClosed(short); ok {
			r.notify(c, closedText(closed))
			return true
		}
		return false
	}
	if answer == "" {
		r.notify(c, fmt.Sprintf("Add your answer after the id, e.g. #%s yes.", short))
		return true
	}
	r.claimRequest(c, msg, req.ID, req.From.Display(), answer)
	return true
}

// claimRequest answers request id with text on behalf of the chat's human.
func (r *Router) claimRequest(c *chat, msg channels.InboundMessage, id, from, text string) {
	if r.cfg.PostOffice == nil {
		r.log("postoffice_unavailable", c)
		r.notify(c, "post-office isn't available to this daemon, so I couldn't pass your answer on. Answer it in the orch TUI inbox.")
		return
	}
	won, err := r.cfg.PostOffice.Claim(postoffice.Resolution{
		ID:       id,
		Outcome:  postoffice.OutcomeAnswered,
		Response: text,
		Responder: &postoffice.Responder{
			Kind: postoffice.ResponderOperator,
			Via:  c.channel,
			Name: msg.SenderName,
			Chat: c.id,
		},
	})
	switch {
	case err == nil:
		r.log("postoffice_answered", c, "id", id, "runes", fmt.Sprint(len([]rune(text))))
		if from == "" {
			from = "the agent"
		}
		r.notify(c, "✅ Sent to "+from)
	case errors.Is(err, postoffice.ErrAlreadyClosed):
		r.log("postoffice_already_closed", c, "id", id, "outcome", string(won.Outcome), "via", won.Via())
		r.notify(c, closedText(won))
	case errors.Is(err, postoffice.ErrNotFound):
		r.log("postoffice_not_found", c, "id", id)
		r.notify(c, "That request is no longer in the post-office, so there's nothing to answer.")
	default:
		r.log("postoffice_error", c, "id", id, "err", shorten(err.Error(), 200))
		r.notify(c, fmt.Sprintf("I couldn't record your answer (%s). Try again, or answer in the orch TUI inbox.", shorten(err.Error(), 120)))
	}
}

// closedText is the reply to an answer that lost: the request had already
// closed.
func closedText(r postoffice.Resolution) string {
	switch r.Outcome {
	case postoffice.OutcomeAnswered:
		if via := r.Via(); via != "" {
			return "Already answered via " + via + "."
		}
		return "Already answered."
	case postoffice.OutcomeCancelled:
		return "That request was cancelled: the agent no longer needs an answer."
	case postoffice.OutcomeExpired:
		return "That request has expired: the agent stopped waiting."
	}
	return fmt.Sprintf("That request is already closed (%s).", r.Outcome)
}

// openRequests lists the operator requests that can still be answered,
// oldest first.
func (r *Router) openRequests() ([]postoffice.Message, error) {
	msgs, err := r.cfg.PostOffice.ListOpen()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []postoffice.Message
	for _, m := range msgs {
		if m.Kind == postoffice.KindRequest && m.ToOperator() && !m.Expired(now) {
			if _, res, err := r.cfg.PostOffice.Lookup(m.ID); err == nil && res != nil {
				continue // claimed; finalisation pending
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// recentlyClosed finds the resolution of an operator request with the given
// short id that is open-but-claimed or expired (still in the inbox). Finalised
// requests are not searched: short ids are only meaningful while open.
func (r *Router) recentlyClosed(short string) (postoffice.Resolution, bool) {
	msgs, err := r.cfg.PostOffice.ListOpen()
	if err != nil {
		return postoffice.Resolution{}, false
	}
	now := time.Now()
	for _, m := range msgs {
		if m.Kind != postoffice.KindRequest || !m.ToOperator() || m.ShortID() != short {
			continue
		}
		if _, res, err := r.cfg.PostOffice.Lookup(m.ID); err == nil && res != nil {
			return *res, true
		}
		if m.Expired(now) {
			return postoffice.Resolution{ID: m.ID, Outcome: postoffice.OutcomeExpired}, true
		}
	}
	return postoffice.Resolution{}, false
}

// cmdInbox lists the open operator requests with their short ids.
func (r *Router) cmdInbox(c *chat) {
	if r.cfg.PostOffice == nil {
		r.notify(c, "post-office isn't available to this daemon.")
		return
	}
	open, err := r.openRequests()
	if err != nil {
		r.log("postoffice_error", c, "err", shorten(err.Error(), 200))
		r.notify(c, "I couldn't read the post-office inbox.")
		return
	}
	if len(open) == 0 {
		r.notify(c, "No open requests.")
		return
	}
	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "Open requests (%d):", len(open))
	for _, m := range open {
		fmt.Fprintf(&b, "\n#%s %s: %s (%s ago, expires in %s)", m.ShortID(), m.From.Display(), m.Subject,
			postoffice.HumanDuration(now.Sub(m.CreatedAt)), postoffice.HumanDuration(m.ExpiresAt.Sub(now)))
	}
	b.WriteString("\n\nAnswer with #<id> <answer>, or reply to the request's message.")
	r.notify(c, b.String())
}
