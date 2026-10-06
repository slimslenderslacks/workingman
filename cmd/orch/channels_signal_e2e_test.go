package main

// End-to-end test of the Signal channel through the same wiring `orch
// --headless` uses (setupChannels → daemon options → daemonChannels.start),
// against a fake signal-cli daemon:
//
//	fake signal-cli (SSE envelopes) ──► signal channel ──► access policy
//	      ──► router ──► acpchat ──► fake ACP agents (wolf, workingman)
//	daemon ──► wolf-start message ──► signal channel ──► fake signal-cli (JSON-RPC send)

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/router"
)

const (
	sigAccount  = "+15550001111"
	sigOwner    = "+15551234567"
	sigStranger = "+15559998888"
)

// sigSent is one message the daemon sent through the fake signal-cli.
type sigSent struct {
	To, Body string
	QuoteTS  int64
	ID       string // the timestamp signal-cli returned
}

// fakeSignalCLI is an httptest signal-cli: /api/v1/check, an SSE stream fed
// from push, and /api/v1/rpc (version, listAccounts, send, sendTyping).
type fakeSignalCLI struct {
	srv    *httptest.Server
	events chan string

	mu   sync.Mutex
	sent []sigSent
	next int64
	seq  int64
}

func newFakeSignalCLI(t *testing.T) *fakeSignalCLI {
	t.Helper()
	f := &fakeSignalCLI{events: make(chan string, 64), next: 1700000000000}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/check", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/v1/events", f.serveEvents)
	mux.HandleFunc("/api/v1/rpc", f.serveRPC)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSignalCLI) serveEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	_, _ = io.WriteString(w, ": keepalive\n\n")
	fl.Flush()
	for {
		select {
		case ev := <-f.events:
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (f *fakeSignalCLI) serveRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
		ID     any            `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var result any = map[string]any{}
	switch req.Method {
	case "version":
		result = map[string]any{"version": "0.13.0-fake"}
	case "listAccounts":
		result = []map[string]any{{"number": sigAccount}}
	case "send":
		m := sigSent{}
		if rcpt, ok := req.Params["recipient"].([]any); ok && len(rcpt) > 0 {
			m.To, _ = rcpt[0].(string)
		}
		if g, ok := req.Params["groupId"].(string); ok {
			m.To = "group:" + g
		}
		m.Body, _ = req.Params["message"].(string)
		if q, ok := req.Params["quoteTimestamp"].(float64); ok {
			m.QuoteTS = int64(q)
		}
		f.mu.Lock()
		f.next++
		m.ID = strconv.FormatInt(f.next, 10)
		f.sent = append(f.sent, m)
		f.mu.Unlock()
		ts, _ := strconv.ParseInt(m.ID, 10, 64)
		result = map[string]any{"timestamp": ts, "results": []map[string]any{{"type": "SUCCESS"}}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func (f *fakeSignalCLI) messages() []sigSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sigSent(nil), f.sent...)
}

func (f *fakeSignalCLI) find(substr string) []sigSent {
	var out []sigSent
	for _, m := range f.messages() {
		if strings.Contains(m.Body, substr) {
			out = append(out, m)
		}
	}
	return out
}

// say delivers a direct message from `from`; quoteID is the timestamp of the
// message being replied to ("" for a plain message).
func (f *fakeSignalCLI) say(from, text, quoteID string) {
	f.mu.Lock()
	f.seq++
	ts := 1800000000000 + f.seq
	f.mu.Unlock()
	dm := map[string]any{"timestamp": ts, "message": text}
	if quoteID != "" {
		q, _ := strconv.ParseInt(quoteID, 10, 64)
		dm["quote"] = map[string]any{"id": q}
	}
	env, _ := json.Marshal(map[string]any{"envelope": map[string]any{
		"sourceNumber": from, "sourceName": "Tester", "timestamp": ts, "dataMessage": dm,
	}})
	f.events <- string(env)
}

// signalConfigYAML is the signal section: the owner is allowlisted.
func signalConfigYAML(cli *fakeSignalCLI) string {
	return fmt.Sprintf(`  signal:
    type: signal
    options:
      http_url: %q
      account: %q
      access:
        allow_from: [%q]
`, cli.srv.URL, sigAccount, sigOwner)
}

const signalRoutesYAML = `  - {topic: wolf,       channel: signal, chat: "+15551234567"}
  - {topic: workingman, channel: signal, chat: "+15551234567"}
`

// waitSignal waits for a message containing substr and returns it.
func waitSignal(t *testing.T, env *e2eEnv, cli *fakeSignalCLI, substr string) sigSent {
	t.Helper()
	var got []sigSent
	env.waitFor(fmt.Sprintf("a Signal message containing %q (sent: %+v)", substr, cli.messages()), func() bool {
		got = cli.find(substr)
		return len(got) > 0
	})
	if got[0].To != sigOwner {
		t.Errorf("message %q went to %s, want %s", got[0].Body, got[0].To, sigOwner)
	}
	return got[0]
}

func blockProject(t *testing.T, env *e2eEnv) {
	t.Helper()
	projectPath := filepath.Join(env.root, "alpha", ".project.yaml")
	blocked := &project.Project{Description: "e2e", Branch: "feat/alpha", Status: project.StatusBlocked, BlockedReason: "need a decision on the schema"}
	if err := project.SaveAs(projectPath, blocked, project.WriterAgent); err != nil {
		t.Fatal(err)
	}
}

func TestSignalChannelEndToEnd(t *testing.T) {
	var cli *fakeSignalCLI
	env := newE2EEnvWith(t, func(env *e2eEnv) string {
		cli = newFakeSignalCLI(t)
		return "channels:\n" + signalConfigYAML(cli) + "routes:\n" + signalRoutesYAML +
			"router:\n  thinking_after: 1m\n  turn_timeout: 30s\n"
	})
	env.waitFor("the workingman agent to be tracked", func() bool {
		_, ok := env.d.WorkingmanAgentSession()
		return ok
	})

	// A project becomes blocked → the wolf starts → one message, exactly once.
	blockProject(t, env)
	start := waitSignal(t, env, cli, "wolf is running for alpha")
	if !strings.HasPrefix(start.Body, "[wolf] ") {
		t.Errorf("the wolf message is not labelled with the wolf topic: %q", start.Body)
	}
	if !strings.Contains(start.Body, "need a decision on the schema") {
		t.Errorf("the wolf message does not say why the project is blocked: %q", start.Body)
	}
	wolf := env.launcher.ofKind(agent.WolfAgent)
	if wolf == nil {
		t.Fatal("a wolf-start message was sent but no wolf session was launched")
	}
	settle()
	if n := len(cli.find("wolf is running")); n != 1 {
		t.Errorf("the wolf-start message was sent %d times, want exactly 1: %+v", n, cli.messages())
	}

	// A quote-reply to it reaches the wolf, and its answer comes back quoting.
	cli.say(sigOwner, "what is blocking you?", start.ID)
	answer := waitSignal(t, env, cli, "[wolf alpha]")
	if !strings.Contains(answer.Body, "wolf heard: what is blocking you?") {
		t.Errorf("wolf answer = %q", answer.Body)
	}
	if got := wolf.promptsSeen(); len(got) != 1 || got[0] != "what is blocking you?" {
		t.Errorf("the wolf saw prompts %q, want only the owner's question", got)
	}
	wm := env.launcher.ofKind(agent.WorkingmanAgent)
	if got := wm.promptsSeen(); len(got) != 0 {
		t.Errorf("a reply to the wolf must not reach the workingman agent, which saw %q", got)
	}

	// A plain question goes to the workingman agent.
	env.waitFor("the snapshot to show alpha as blocked", func() bool {
		return strings.Contains(workingmanAnswer(env.state, "?"), "alpha is blocked")
	})
	cli.say(sigOwner, "which projects are open?", "")
	waitSignal(t, env, cli, "From the snapshot: alpha is blocked")
	if got := wm.promptsSeen(); len(got) != 2 || got[0] != router.OpeningPrompt || got[1] != "which projects are open?" {
		t.Errorf("the workingman agent saw prompts %q, want its opening prompt then the question", got)
	}

	// A sender outside the allowlist is ignored: no reply, nothing reaches an agent.
	promptsBefore := len(wm.promptsSeen()) + len(wolf.promptsSeen())
	sentBefore := len(cli.messages())
	cli.say(sigStranger, "which projects are open?", "")
	settle()
	if got := len(wm.promptsSeen()) + len(wolf.promptsSeen()); got != promptsBefore {
		t.Errorf("an agent received a prompt from a non-allowlisted sender: workingman %q, wolf %q", wm.promptsSeen(), wolf.promptsSeen())
	}
	// A valid message afterwards still works, proving the stranger's was
	// processed (in order) and dropped.
	cli.say(sigOwner, "still there?", "")
	env.waitFor("an answer to a later question", func() bool { return len(cli.messages()) > sentBefore })
	for _, m := range cli.messages() {
		if m.To == sigStranger {
			t.Errorf("the daemon replied to a sender that is not on the allowlist: %+v", m)
		}
	}

	env.shutdown()
}

// WhatsApp and Signal configured together: the wolf-start message goes out on
// both, and a quote-reply on Signal reaches the wolf like one on WhatsApp.
func TestSignalAndWhatsAppTogether(t *testing.T) {
	var cli *fakeSignalCLI
	env := newE2EEnvWith(t, func(env *e2eEnv) string {
		cli = newFakeSignalCLI(t)
		t.Setenv("E2E_WA_TOKEN", "EAAGe2esupersecrettoken1234567890")
		t.Setenv("E2E_WA_SECRET", e2eAppSecret)
		t.Setenv("E2E_WA_VERIFY", "e2e-verify")
		return fmt.Sprintf(`channels:
  whatsapp:
    type: whatsapp
    credentials:
      access_token: {env: E2E_WA_TOKEN}
      app_secret:   {env: E2E_WA_SECRET}
      verify_token: {env: E2E_WA_VERIFY}
    options:
      phone_number_id: %q
      graph_base_url: %q
      webhook_host: 127.0.0.1
      webhook_port: 0
      access:
        allow_from: ["+1 555 123 4567"]
%sroutes:
  - {topic: wolf,       channel: whatsapp, chat: %q}
  - {topic: workingman, channel: whatsapp, chat: %q}
%srouter:
  thinking_after: 1m
  turn_timeout: 30s
`, e2ePhoneID, env.graph.srv.URL, signalConfigYAML(cli), e2eOwner, e2eOwner, signalRoutesYAML)
	})
	if got := env.dc.reg.Names(); len(got) != 2 {
		t.Fatalf("registry channels = %v, want whatsapp and signal", got)
	}
	env.waitFor("the workingman agent to be tracked", func() bool {
		_, ok := env.d.WorkingmanAgentSession()
		return ok
	})

	blockProject(t, env)
	start := waitSignal(t, env, cli, "wolf is running for alpha")
	env.waitMessage("wolf is running for alpha") // the WhatsApp copy
	settle()
	if n, m := len(cli.find("wolf is running")), len(env.graph.find("wolf is running")); n != 1 || m != 1 {
		t.Errorf("wolf-start sent %d times on Signal and %d on WhatsApp, want once each", n, m)
	}

	cli.say(sigOwner, "status?", start.ID)
	answer := waitSignal(t, env, cli, "[wolf alpha]")
	if !strings.Contains(answer.Body, "wolf heard: status?") {
		t.Errorf("wolf answer on Signal = %q", answer.Body)
	}
	if n := len(env.graph.find("[wolf alpha]")); n != 0 {
		t.Errorf("the reply to a Signal message was also sent on WhatsApp (%d times)", n)
	}
}
