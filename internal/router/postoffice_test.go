package router

import (
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/postoffice"
)

var poSender = postoffice.Sender{Agent: "task", Sandbox: "wm-p-fix-ci", Project: "/o/p/project.yaml", Task: "fix-ci"}

func newPostOfficeHarness(t *testing.T) (*harness, *postoffice.Store) {
	t.Helper()
	s, err := postoffice.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newHarness(t, func(c *Config) { c.PostOffice = s }), s
}

func putReq(t *testing.T, s *postoffice.Store, d postoffice.Draft) postoffice.Message {
	t.Helper()
	if d.Kind == "" {
		d.Kind = postoffice.KindRequest
	}
	if d.From.Sandbox == "" {
		d.From = poSender
	}
	m, err := postoffice.NewMessage(d, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(m); err != nil {
		t.Fatal(err)
	}
	return m
}

// register records a channel message as carrying request m, as the
// Dispatcher does when it sends it.
func (h *harness) register(messageID string, m postoffice.Message) {
	h.t.Helper()
	if err := h.index.Register(chanNm, messageID, channels.ConversationTarget{
		Kind: channels.ConversationKindPostOffice, WorkStream: m.From.Display(), SessionKey: m.ID,
	}); err != nil {
		h.t.Fatal(err)
	}
}

func claimOf(t *testing.T, s *postoffice.Store, id string) postoffice.Resolution {
	t.Helper()
	r, ok, err := s.Claimed(id)
	if err != nil || !ok {
		t.Fatalf("no claim for %s (err=%v)", id, err)
	}
	return r
}

func TestPostOfficeReplyClaimsRequest(t *testing.T) {
	h, s := newPostOfficeHarness(t)
	m := putReq(t, s, postoffice.Draft{Body: "Which bucket?", Options: []string{"staging", "prod"}})
	h.register("req-1", m)

	h.sayReply("2", "req-1")
	got := h.wait(1)
	contains(t, got[0], "✅ Sent to task fix-ci (p)")

	r := claimOf(t, s, m.ID)
	if r.Outcome != postoffice.OutcomeAnswered || r.Response != "2" || r.Option != "prod" {
		t.Fatalf("claim %+v", r)
	}
	if r.Responder == nil || r.Responder.Kind != postoffice.ResponderOperator || r.Responder.Via != chanNm || r.Responder.Chat != chatID {
		t.Fatalf("responder %+v", r.Responder)
	}
	if asks := h.agentFake().Asks(); len(asks) != 0 {
		t.Fatalf("the answer also went to the agent: %q", asks)
	}
	if !strings.Contains(h.audit.all(), "router_postoffice_answered") {
		t.Error("no audit line")
	}
}

func TestPostOfficeReplyAlreadyAnswered(t *testing.T) {
	h, s := newPostOfficeHarness(t)
	m := putReq(t, s, postoffice.Draft{Body: "Which bucket?"})
	h.register("req-1", m)
	if _, err := s.Claim(postoffice.Resolution{ID: m.ID, Outcome: postoffice.OutcomeAnswered, Response: "staging",
		Responder: &postoffice.Responder{Kind: postoffice.ResponderOperator, Via: postoffice.ViaTUI}}); err != nil {
		t.Fatal(err)
	}

	h.sayReply("prod", "req-1")
	contains(t, h.wait(1)[0], "Already answered via tui.")
	if r := claimOf(t, s, m.ID); r.Response != "staging" {
		t.Fatalf("the losing answer replaced the winner: %+v", r)
	}

	// After finalisation the same reply still gets the winner.
	if _, err := s.Finalize(m.ID); err != nil {
		t.Fatal(err)
	}
	h.sayReply("prod", "req-1")
	contains(t, h.wait(2)[1], "Already answered via tui.")
}

func TestPostOfficeReplyCancelledAndExpired(t *testing.T) {
	h, s := newPostOfficeHarness(t)
	c := putReq(t, s, postoffice.Draft{Body: "cancel me"})
	h.register("req-c", c)
	if _, err := s.Claim(postoffice.Resolution{ID: c.ID, Outcome: postoffice.OutcomeCancelled}); err != nil {
		t.Fatal(err)
	}
	h.sayReply("ok", "req-c")
	contains(t, h.wait(1)[0], "was cancelled")

	e := putReq(t, s, postoffice.Draft{Body: "expiring", ExpiresIn: time.Minute})
	h.register("req-e", e)
	s.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	h.sayReply("ok", "req-e")
	contains(t, h.wait(2)[1], "has expired")
}

func TestPostOfficeReplyWithoutStore(t *testing.T) {
	h := newHarness(t, nil)
	m, err := postoffice.NewMessage(postoffice.Draft{Kind: postoffice.KindRequest, From: poSender, Body: "q"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.register("req-1", m)
	h.sayReply("hi", "req-1")
	contains(t, h.wait(1)[0], "post-office isn't available")
	if asks := h.agentFake().Asks(); len(asks) != 0 {
		t.Fatalf("the answer went to the agent: %q", asks)
	}
}

func TestPostOfficeShortIDAnswer(t *testing.T) {
	h, s := newPostOfficeHarness(t)
	m := putReq(t, s, postoffice.Draft{Body: "Which bucket?"})

	h.say("#" + strings.ToUpper(m.ShortID()) + "  use staging\nplease")
	contains(t, h.wait(1)[0], "✅ Sent to task fix-ci (p)")
	if r := claimOf(t, s, m.ID); r.Response != "use staging\nplease" {
		t.Fatalf("response %q", r.Response)
	}

	// A second answer by short id loses.
	h.say("#" + m.ShortID() + " prod")
	contains(t, h.wait(2)[1], "Already answered via "+chanNm)
}

func TestPostOfficeShortIDEdgeCases(t *testing.T) {
	h, s := newPostOfficeHarness(t)
	m := putReq(t, s, postoffice.Draft{Body: "Which bucket?"})

	h.say("#" + m.ShortID())
	contains(t, h.wait(1)[0], "Add your answer after the id")

	// Not an open request id: ordinary text for the agent.
	h.say("#abcdef is my favourite hashtag")
	eventually(t, "the agent to get the hashtag", func() bool {
		asks := h.agentFake().Asks()
		return len(asks) == 1 && strings.HasPrefix(asks[0], "#abcdef")
	})
	if _, ok, _ := s.Claimed(m.ID); ok {
		t.Fatal("the request was claimed")
	}
}

func TestPostOfficeShortIDIgnoresAgentAndNotifications(t *testing.T) {
	h, s := newPostOfficeHarness(t)
	n := putReq(t, s, postoffice.Draft{Kind: postoffice.KindNotification, Subject: "FYI", Body: "done"})
	a := putReq(t, s, postoffice.Draft{Body: "network?", Recipient: "step-up"})
	h.say("#" + n.ShortID() + " ok")
	h.say("#" + a.ShortID() + " ok")
	eventually(t, "both to reach the agent", func() bool { return len(h.agentFake().Asks()) == 2 })
	for _, id := range []string{n.ID, a.ID} {
		if _, ok, _ := s.Claimed(id); ok {
			t.Fatalf("%s was claimed", id)
		}
	}
}

func TestInboxCommand(t *testing.T) {
	h, s := newPostOfficeHarness(t)
	h.say("/inbox")
	contains(t, h.wait(1)[0], "No open requests.")

	a := putReq(t, s, postoffice.Draft{Subject: "Which bucket?", Body: "for the export"})
	b := putReq(t, s, postoffice.Draft{Subject: "Retry?", Body: "ci failed"})
	putReq(t, s, postoffice.Draft{Kind: postoffice.KindNotification, Subject: "FYI", Body: "x"})
	putReq(t, s, postoffice.Draft{Subject: "to step-up", Body: "y", Recipient: "step-up"})
	answered := putReq(t, s, postoffice.Draft{Subject: "answered", Body: "z"})
	if _, err := s.Claim(postoffice.Resolution{ID: answered.ID, Outcome: postoffice.OutcomeCancelled}); err != nil {
		t.Fatal(err)
	}

	h.say("/inbox")
	got := h.wait(2)[1]
	contains(t, got, "Open requests (2):")
	contains(t, got, "#"+a.ShortID()+" task fix-ci (p): Which bucket?")
	contains(t, got, "#"+b.ShortID()+" task fix-ci (p): Retry?")
	for _, no := range []string{"FYI", "to step-up", "answered"} {
		if strings.Contains(got, no) {
			t.Errorf("/inbox lists %q:\n%s", no, got)
		}
	}
	if strings.Index(got, a.ShortID()) > strings.Index(got, b.ShortID()) {
		t.Error("not oldest first")
	}
}

func TestInboxCommandWithoutStore(t *testing.T) {
	h := newHarness(t, nil)
	h.say("/inbox")
	contains(t, h.wait(1)[0], "post-office isn't available")
}
