package main

import (
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/postoffice"
)

// The post-office Dispatcher and router through the production wiring, on
// Signal: an operator request in the store goes out on the post-office route,
// a quote-reply claims it, and once post-office finalises the claim the other
// route hears it was answered.
func TestPostOfficeOverSignalEndToEnd(t *testing.T) {
	var cli *fakeSignalCLI
	env := newE2EEnvWith(t, func(env *e2eEnv) string {
		cli = newFakeSignalCLI(t)
		return "channels:\n" + signalConfigYAML(cli) + "routes:\n" + signalRoutesYAML +
			"  - {topic: post-office, channel: signal, chat: \"+15551234567\"}\n" +
			"  - {topic: post-office, channel: signal, chat: \"group:ops\"}\n" +
			"router:\n  thinking_after: 1m\n  turn_timeout: 30s\n"
	})
	store := env.dc.postOffice
	if store == nil {
		t.Fatalf("no post-office store; audit log:\n%s", env.audit.String())
	}

	m, err := postoffice.NewMessage(postoffice.Draft{
		Kind: postoffice.KindRequest, Body: "Which bucket for the export?", Options: []string{"staging", "prod"},
		From: postoffice.Sender{Agent: "task", Sandbox: "wm-alpha-export", Project: env.root + "/alpha/project.yaml", Task: "export"},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(m); err != nil {
		t.Fatal(err)
	}

	env.waitFor("both deliveries", func() bool { return len(cli.find("[req "+m.ShortID()+"]")) == 2 })
	var req sigSent
	for _, s := range cli.find("[req " + m.ShortID() + "]") {
		if s.To == sigOwner {
			req = s
		}
	}
	if !strings.Contains(req.Body, "task export (alpha)") || !strings.Contains(req.Body, "Options: 1) staging  2) prod") {
		t.Errorf("request text = %q", req.Body)
	}

	cli.say(sigOwner, "staging", req.ID)
	waitSignal(t, env, cli, "✅ Sent to task export (alpha)")
	var claim postoffice.Resolution
	env.waitFor("the claim", func() bool {
		var ok bool
		claim, ok, _ = store.Claimed(m.ID)
		return ok
	})
	if claim.Option != "staging" || claim.Responder.Via != "signal" || claim.Responder.Chat != sigOwner {
		t.Fatalf("claim = %+v responder %+v", claim, claim.Responder)
	}

	// post-office finalises; only the group (which did not answer) gets a note.
	if _, err := store.Finalize(m.ID); err != nil {
		t.Fatal(err)
	}
	var note sigSent
	env.waitFor("the closing note", func() bool {
		got := cli.find("Answered via signal: staging")
		if len(got) > 0 {
			note = got[0]
		}
		return len(got) > 0
	})
	if note.To != "group:ops" {
		t.Errorf("closing note went to %s, want only the group", note.To)
	}

	// A late answer is told it lost.
	cli.say(sigOwner, "#"+m.ShortID()+" prod", "")
	cli.say(sigOwner, "prod", req.ID)
	waitSignal(t, env, cli, "Already answered via signal.")

	settle()
	if n := len(cli.find("Answered via signal: staging")); n != 1 {
		t.Errorf("closing note sent %d times", n)
	}
	env.shutdown()
}
