package main

// End-to-end test of the WhatsApp channels feature, through the same wiring
// `orch --headless` uses (setupChannels → daemon options → daemonChannels.start):
//
//	Meta (fake, signed POSTs) ──► real webhook on an ephemeral port ──► access policy
//	      ──► router ──► acpchat ──► fake ACP agents (wolf, workingman) behind real unix sockets
//	daemon ──► wolf-start / wolf-end messages ──► real Cloud client ──► fake Graph API (httptest)
//
// No sandbox, no tmux and no network beyond loopback: the runner's AcpLauncher
// is a fake that serves each agent's session directory itself.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/channels/whatsapp"
	"github.com/slimslenderslacks/work/internal/daemon"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/router"
	"github.com/slimslenderslacks/work/internal/runner"
)

const (
	e2eAppSecret = "e2e-app-secret"
	e2eOwner     = "15551234567"
	e2eStranger  = "15559998888"
	e2ePhoneID   = "1098765"
)

// recordingNotifier is the local (macOS) half of the daemon's notifier.
type recordingNotifier struct {
	mu   sync.Mutex
	sent []string
}

func (n *recordingNotifier) Send(title, message string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, title+": "+message)
	return nil
}

// e2eEnv is one running daemon with channels, plus the fakes around it.
type e2eEnv struct {
	t        *testing.T
	graph    *e2eGraph
	launcher *fakeACPLauncher
	dc       *daemonChannels
	d        *daemon.Daemon
	audit    *lockedBuffer

	hookURL string // the webhook's URL on its ephemeral port
	root    string // the watched orch root
	state   string // the daemon's snapshot file

	cancel context.CancelFunc
	done   chan struct{}
	a      *audit.Logger
	stopMu sync.Once
}

func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	env := newE2EEnvWith(t, func(env *e2eEnv) string {
		t.Setenv("E2E_WA_TOKEN", "EAAGe2esupersecrettoken1234567890")
		t.Setenv("E2E_WA_SECRET", e2eAppSecret)
		t.Setenv("E2E_WA_VERIFY", "e2e-verify")
		return fmt.Sprintf(`
channels:
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
routes:
  - {topic: wolf,       channel: whatsapp, chat: %q}
  - {topic: workingman, channel: whatsapp, chat: %q}
router:
  thinking_after: 1m
  turn_timeout: 30s
`, e2ePhoneID, env.graph.srv.URL, e2eOwner, e2eOwner)
	})
	ch, ok := env.dc.reg.Get("whatsapp")
	if !ok {
		t.Fatal("no whatsapp channel registered")
	}
	addr := ch.(*whatsapp.Channel).WebhookAddr()
	if addr == "" {
		t.Fatalf("the webhook did not start; audit log:\n%s", env.audit.String())
	}
	env.hookURL = "http://" + addr + whatsapp.DefaultWebhookPath
	return env
}

// newE2EEnvWith starts a daemon with channels from the channels.yaml text that
// config returns (it runs after the fake Graph API exists, so it can point at
// it). The fake wolf and workingman agents are the same for every transport.
func newE2EEnvWith(t *testing.T, config func(env *e2eEnv) string) *e2eEnv {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes, which t.TempDir() can exceed.
	base, err := os.MkdirTemp("", "wm-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	env := &e2eEnv{t: t, graph: newE2EGraph(t), audit: &lockedBuffer{}}
	env.root = filepath.Join(base, "orch")
	sessionsRoot := filepath.Join(base, "sessions")
	env.state = filepath.Join(base, "state", "snapshot.json")
	logDir := filepath.Join(base, "logs")
	for _, dir := range []string{filepath.Join(env.root, "alpha"), sessionsRoot, filepath.Dir(env.state), logDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfgPath := filepath.Join(base, "channels.yaml")
	cfg := config(env)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	env.a = audit.New(env.audit)
	env.dc, err = setupChannels(cfgPath, logDir, filepath.Join(base, "state"), env.a)
	if err != nil {
		t.Fatalf("setupChannels: %v", err)
	}
	if !env.dc.enabled() {
		t.Fatal("channels were not enabled by the config")
	}

	// The workingman agent answers from the daemon's published snapshot, like
	// the real one reads it; a wolf just echoes what it was told.
	env.launcher = &fakeACPLauncher{root: sessionsRoot, script: func(k agent.Kind) func(string) string {
		if k == agent.WolfAgent {
			return func(p string) string { return "wolf heard: " + p }
		}
		return func(p string) string { return workingmanAnswer(env.state, p) }
	}}
	r := &runner.Runner{
		Launcher:     noTmux{},
		AcpLauncher:  env.launcher,
		Kit:          "e2e-kit",
		SessionsRoot: sessionsRoot,
		Audit:        env.a,
	}
	opts := []daemon.Option{
		daemon.WithRunner(r),
		daemon.WithStateFile(env.state, 0),
		daemon.WithRuntimeInfo(daemon.RuntimeInfo{SessionsRoot: sessionsRoot, AuditLog: filepath.Join(logDir, "audit.log"), Headless: true}),
		daemon.WithWorkingmanAgent(daemon.WorkingmanAgentConfig{Dir: filepath.Join(base, "workingman-agent")}),
	}
	opts = append(opts, env.dc.optionsWithLocal(env.a, &recordingNotifier{})...)
	env.d, err = daemon.New([]string{env.root}, env.a, opts...)
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	env.cancel = cancel
	env.done = make(chan struct{})
	env.dc.start(ctx, env.a, env.d) // as runDaemon does, before Run
	go func() {
		defer close(env.done)
		_ = env.d.Run(ctx)
	}()
	t.Cleanup(env.shutdown)

	return env
}

// shutdown stops everything the way runDaemon does on SIGINT and waits for the
// daemon to return. Safe to call twice.
func (e *e2eEnv) shutdown() {
	e.stopMu.Do(func() {
		e.cancel()
		select {
		case <-e.done:
		case <-time.After(10 * time.Second):
			e.t.Error("daemon.Run did not return after its context was cancelled")
		}
		e.dc.stop(e.a)
		e.launcher.stopAll() // the real wrappers outlive the daemon; the fakes must not outlive the test
	})
}

// noTmux is the runner's host-side launcher, which no kind in this test uses.
type noTmux struct{}

func (noTmux) Launch(context.Context, agent.Spec) (agent.Session, error) {
	return nil, fmt.Errorf("e2e: unexpected tmux launch")
}

// workingmanAnswer is what the fake workingman agent says: it answers from the
// snapshot file, and acknowledges its opening prompt.
func workingmanAnswer(snapshotPath, prompt string) string {
	if prompt == router.OpeningPrompt {
		return "ready"
	}
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		return "I can't read the snapshot: " + err.Error()
	}
	var generic struct {
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "the snapshot is not valid JSON"
	}
	var lines []string
	for _, p := range generic.Projects {
		lines = append(lines, fmt.Sprintf("%v is %v", p["work_stream"], p["status"]))
	}
	if len(lines) == 0 {
		return "no projects"
	}
	return "From the snapshot: " + strings.Join(lines, "; ")
}

// --- driving the inbound side ---

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// inboundPayload is the body Meta POSTs for one text message.
func inboundPayload(wamid, from, text, replyTo string) []byte {
	msg := map[string]any{
		"id": wamid, "from": from, "timestamp": fmt.Sprint(time.Now().Unix()),
		"type": "text", "text": map[string]any{"body": text},
	}
	if replyTo != "" {
		msg["context"] = map[string]any{"id": replyTo, "from": "15550001111"}
	}
	b, _ := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry": []any{map[string]any{"id": "WABA-1", "changes": []any{map[string]any{
			"field": "messages",
			"value": map[string]any{
				"messaging_product": "whatsapp",
				"metadata":          map[string]any{"display_phone_number": "15550001111", "phone_number_id": e2ePhoneID},
				"contacts":          []any{map[string]any{"wa_id": from, "profile": map[string]any{"name": "Tester"}}},
				"messages":          []any{msg},
			},
		}}}},
	})
	return b
}

// post delivers body to the webhook with the given signature header ("" omits
// it) and returns the HTTP status.
func (e *e2eEnv) post(body []byte, sig string) int {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.hookURL, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

var wamidSeq int

// say delivers a correctly signed message from `from`; replyTo is the wamid of
// the message being replied to ("" for a plain message).
func (e *e2eEnv) say(from, text, replyTo string) {
	e.t.Helper()
	wamidSeq++
	body := inboundPayload(fmt.Sprintf("wamid.IN%d", wamidSeq), from, text, replyTo)
	if code := e.post(body, sign(e2eAppSecret, body)); code != http.StatusOK {
		e.t.Fatalf("webhook rejected a valid message from %s: HTTP %d", from, code)
	}
}

// waitFor polls cond until it holds; what describes it in the failure message,
// which also carries the audit log.
func (e *e2eEnv) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s\nsent so far: %+v\naudit log:\n%s", what, e.graph.messages(), e.audit.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitMessage waits for a message to the owner containing substr and returns it.
func (e *e2eEnv) waitMessage(substr string) sentMessage {
	e.t.Helper()
	var got []sentMessage
	e.waitFor(fmt.Sprintf("a WhatsApp message containing %q", substr), func() bool {
		got = e.graph.find(substr)
		return len(got) > 0
	})
	if got[0].To != e2eOwner {
		e.t.Errorf("message %q went to %s, want %s", got[0].Body, got[0].To, e2eOwner)
	}
	return got[0]
}

// settle gives work that should NOT happen a chance to (wrongly) happen.
func settle() { time.Sleep(300 * time.Millisecond) }

func TestChannelsEndToEnd(t *testing.T) {
	before := goroutineIDs()
	env := newE2EEnv(t)

	// The workingman agent is up before anyone asks it anything.
	env.waitFor("the workingman agent to launch", func() bool { return env.launcher.ofKind(agent.WorkingmanAgent) != nil })
	env.waitFor("the workingman agent to be tracked", func() bool {
		_, ok := env.d.WorkingmanAgentSession()
		return ok
	})

	// (a) A project becomes blocked → the wolf starts → one "wolf is running"
	// message reaches the owner, exactly once.
	projectPath := filepath.Join(env.root, "alpha", ".project.yaml")
	blocked := &project.Project{Description: "e2e", Branch: "feat/alpha", Status: project.StatusBlocked, BlockedReason: "need a decision on the schema"}
	if err := project.SaveAs(projectPath, blocked, project.WriterAgent); err != nil {
		t.Fatal(err)
	}
	start := env.waitMessage("wolf is running for alpha")
	if !strings.HasPrefix(start.Body, "[wolf] ") {
		t.Errorf("the wolf message is not labelled with the wolf topic: %q", start.Body)
	}
	if !strings.Contains(start.Body, "need a decision on the schema") {
		t.Errorf("the wolf message does not say why the project is blocked: %q", start.Body)
	}
	if env.launcher.ofKind(agent.WolfAgent) == nil {
		t.Fatal("a wolf-start message was sent but no wolf session was launched")
	}
	settle()
	if n := len(env.graph.find("wolf is running")); n != 1 {
		t.Errorf("the wolf-start message was sent %d times, want exactly 1:\n%+v", n, env.graph.messages())
	}

	// (b) The owner replies to that message → the wolf gets the text as a prompt
	// → its answer comes back labelled with the wolf and the work stream.
	env.say(e2eOwner, "what is blocking you?", start.ID)
	answer := env.waitMessage("[wolf alpha]")
	if !strings.Contains(answer.Body, "wolf heard: what is blocking you?") {
		t.Errorf("wolf answer = %q", answer.Body)
	}
	wolf := env.launcher.ofKind(agent.WolfAgent)
	if got := wolf.promptsSeen(); len(got) != 1 || got[0] != "what is blocking you?" {
		t.Errorf("the wolf saw prompts %q, want only the owner's question", got)
	}
	if got := env.launcher.ofKind(agent.WorkingmanAgent).promptsSeen(); len(got) != 0 {
		t.Errorf("a reply to the wolf must not reach the workingman agent, which saw %q", got)
	}

	// (c) A plain question goes to the workingman agent, which answers from the
	// daemon's snapshot — here, wait until the snapshot knows about the block.
	env.waitFor("the snapshot to show alpha as blocked", func() bool {
		return strings.Contains(workingmanAnswer(env.state, "?"), "alpha is blocked")
	})
	env.say(e2eOwner, "which projects are open?", "")
	env.waitMessage("From the snapshot: alpha is blocked")
	if n := len(env.graph.find("[wolf alpha] From the snapshot")); n != 0 {
		t.Errorf("the workingman agent's answer carried the wolf label")
	}
	wm := env.launcher.ofKind(agent.WorkingmanAgent)
	// The headless daemon created the agent's ACP session itself, so the router
	// must have given it its opening prompt before the first question.
	if got := wm.promptsSeen(); len(got) != 2 || got[0] != router.OpeningPrompt || got[1] != "which projects are open?" {
		t.Errorf("the workingman agent saw prompts %q, want its opening prompt then the question", got)
	}

	// (d) A sender outside the allowlist is ignored — no reply, nothing reaches an
	// agent — and (e) a bad or missing signature is rejected outright.
	promptsBefore := len(wm.promptsSeen()) + len(wolf.promptsSeen())
	sentBefore := len(env.graph.messages())

	env.say(e2eStranger, "which projects are open?", "")
	body := inboundPayload("wamid.FORGED", e2eOwner, "delete everything", "")
	if code := env.post(body, sign("not-the-app-secret", body)); code != http.StatusUnauthorized {
		t.Errorf("a message with a bad signature got HTTP %d, want 401", code)
	}
	if code := env.post(body, ""); code != http.StatusUnauthorized {
		t.Errorf("a message with no signature got HTTP %d, want 401", code)
	}
	settle()
	if got := len(wm.promptsSeen()) + len(wolf.promptsSeen()); got != promptsBefore {
		t.Errorf("an agent received a prompt it must not have: workingman %q, wolf %q", wm.promptsSeen(), wolf.promptsSeen())
	}
	// A valid message afterwards still works, which also proves the rejected
	// ones were fully processed (the webhook handles them in order) and dropped.
	env.say(e2eOwner, "still there?", "")
	env.waitFor("the workingman agent to answer a later question", func() bool { return len(env.graph.messages()) > sentBefore })
	for _, m := range env.graph.messages() {
		if m.To == e2eStranger {
			t.Errorf("the daemon replied to a sender that is not on the allowlist: %+v", m)
		}
		if strings.Contains(m.Body, "delete everything") {
			t.Errorf("the daemon acted on a forged message: %+v", m)
		}
	}
	for _, p := range append(wm.promptsSeen(), wolf.promptsSeen()...) {
		if p == "delete everything" {
			t.Errorf("a forged message reached an agent")
		}
	}

	// Secrets never reach the logs.
	if strings.Contains(env.audit.String(), "EAAGe2esupersecrettoken") || strings.Contains(env.audit.String(), e2eAppSecret) {
		t.Errorf("a credential leaked into the audit log:\n%s", env.audit.String())
	}

	// (f) Shutdown is clean: Run returns, the webhook port closes, and no
	// goroutine the daemon, the router or a channel started is left behind.
	env.shutdown()
	client := &http.Client{Timeout: time.Second}
	if resp, err := client.Get(strings.TrimSuffix(env.hookURL, whatsapp.DefaultWebhookPath) + whatsapp.HealthPath); err == nil {
		resp.Body.Close()
		t.Errorf("the webhook still answers after shutdown (HTTP %d)", resp.StatusCode)
	}
	env.graph.srv.CloseClientConnections()
	http.DefaultClient.CloseIdleConnections()
	if leaked := leakedGoroutines(before, 5*time.Second); len(leaked) > 0 {
		t.Errorf("%d goroutine(s) outlived the daemon:\n\n%s", len(leaked), strings.Join(leaked, "\n\n"))
	}
}
