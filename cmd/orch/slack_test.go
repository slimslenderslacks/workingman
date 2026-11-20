package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/slack"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

const slackMe = "U0CC1K1MFNX"

// cliSlack is a Slack MCP for the CLI tests: profile, conversation list,
// send, read_thread and reactions, with a reply that appears in a test
// post's thread after `replyAfter` reads.
type cliSlack struct {
	mu         sync.Mutex
	tools      []string
	replyAfter int
	replyFrom  string
	reads      int
	sent       []map[string]any
	reactions  []string
	dials      []string
}

func newCLISlack() *cliSlack {
	s := &cliSlack{replyAfter: 1, replyFrom: slackMe}
	for _, t := range []string{"slack_send_message", "slack_add_reaction", "slack_read_channel", "slack_read_thread", "slack_read_user_profile", "slack_list_user_channels"} {
		s.tools = append(s.tools, "slack_xaa-"+t)
	}
	return s
}

func (s *cliSlack) ListTools(context.Context) ([]mcpgateway.Tool, error) {
	var out []mcpgateway.Tool
	for _, n := range s.tools {
		out = append(out, mcpgateway.Tool{Name: n})
	}
	return out, nil
}

func text(s string) mcpgateway.ToolResult {
	return mcpgateway.ToolResult{Content: []mcpgateway.Content{{Type: "text", Text: s}}}
}

func wrapResult(s string) mcpgateway.ToolResult {
	b, _ := json.Marshal(map[string]string{"result": s})
	return text(string(b))
}

func (s *cliSlack) CallTool(_ context.Context, name string, args map[string]any) (mcpgateway.ToolResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch strings.TrimPrefix(name, "slack_xaa-") {
	case "slack_read_user_profile":
		return wrapResult("User ID: " + slackMe + "\nUsername: jim.clark\nReal Name: Jim Clark\n"), nil
	case "slack_list_user_channels":
		if args["name_prefix"] == "wm" {
			return wrapResult("## My Channels\n\n### #wm-jim\n- **ID:** G0WMJIM01\n- **Type:** Private Channel\n"), nil
		}
		return wrapResult("## My Channels\n\n### #general\n- **ID:** C0GENERAL\n- **Type:** Public Channel\n\n" +
			"### DM with jim.clark\n- **ID:** D0CBT6QV8NT\n- **Type:** Direct Message\n- **User ID:** " + slackMe + "\n"), nil
	case "slack_send_message":
		s.sent = append(s.sent, args)
		b, _ := json.Marshal(map[string]any{"message_context": map[string]string{"message_ts": "1795200001.000100", "channel_id": "D0CBT6QV8NT"}})
		return text(string(b)), nil
	case "slack_read_thread":
		s.reads++
		body := "=== THREAD PARENT MESSAGE ===\nFrom: Jim Clark (" + slackMe + ")\nMessage TS: 1795200001.000100\n[orch slack test] hi\n"
		if s.reads > s.replyAfter {
			body += "\n=== THREAD REPLIES (1 total) ===\n\n--- Reply 1 of 1 ---\nFrom: Someone (" + s.replyFrom + ")\nMessage TS: 1795200009.000100\npong\n"
		} else {
			body += "\nNo thread messsages\n"
		}
		b, _ := json.Marshal(map[string]string{"messages": body, "pagination_info": ""})
		return text(string(b)), nil
	case "slack_add_reaction":
		s.reactions = append(s.reactions, args["emoji"].(string))
		return wrapResult("ok"), nil
	}
	return mcpgateway.ToolResult{}, &mcpgateway.RPCError{Code: -32602, Message: "unknown tool " + name}
}

// useSlack points the commands at fake (nil: the gateway is unreachable).
func useSlack(t *testing.T, fake *cliSlack) {
	t.Helper()
	old, oldPoll := slackDial, slackTestPoll
	slackTestPoll = time.Millisecond
	slackDial = func(_ context.Context, url string) (*slack.Client, error) {
		if fake == nil {
			return nil, errors.New("connection refused")
		}
		fake.mu.Lock()
		fake.dials = append(fake.dials, url)
		fake.mu.Unlock()
		return slack.NewClient(fake), nil
	}
	t.Cleanup(func() { slackDial, slackTestPoll = old, oldPoll })
}

func runSlk(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = runSlack(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func readCfg(t *testing.T, path string) *channels.Config {
	t.Helper()
	cfg, err := channels.Load(path)
	if err != nil {
		data, _ := os.ReadFile(path)
		t.Fatalf("config: %v\n%s", err, data)
	}
	return cfg
}

func TestSlackUsageAndUnknown(t *testing.T) {
	if code, _, errs := runSlk(""); code != 2 || !strings.Contains(errs, "usage: orch slack") {
		t.Errorf("no args: %d %q", code, errs)
	}
	if code, out, _ := runSlk("", "help"); code != 0 || !strings.Contains(out, "setup") {
		t.Errorf("help: %d %q", code, out)
	}
	if code, _, errs := runSlk("", "nope"); code != 2 || !strings.Contains(errs, `unknown subcommand "nope"`) {
		t.Errorf("unknown: %d %q", code, errs)
	}
}

func TestSlackSetupNonInteractiveDefaultsToDMToSelf(t *testing.T) {
	fake := newCLISlack()
	useSlack(t, fake)
	t.Setenv(envSlackGatewayURL, "")
	cfgPath := filepath.Join(t.TempDir(), "channels.yaml")

	code, out, errs := runSlk("", "setup", "--config", cfgPath, "--non-interactive")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "signed in to Slack as Jim Clark ("+slackMe+")") || !strings.Contains(out, "Using your DM-to-self") {
		t.Errorf("output:\n%s", out)
	}
	cfg := readCfg(t, cfgPath)
	spec := cfg.Channels["slack"]
	if spec.Type != "slack" || spec.Options["channel"] != slackMe || spec.Options["gateway_url"] != nil {
		t.Errorf("slack section = %+v", spec)
	}
	if got := fmt.Sprint(spec.Options["allow_from"]); got != "["+slackMe+"]" {
		t.Errorf("allow_from = %s", got)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0] != (channels.Route{Topic: "post-office", Channel: "slack", Chat: slackMe}) {
		t.Errorf("routes = %+v", cfg.Routes)
	}
	if fake.dials[0] != slack.DefaultGatewayURL {
		t.Errorf("dialed %v", fake.dials)
	}

	// Idempotent: a second run writes the same file.
	first, _ := os.ReadFile(cfgPath)
	if code, out, errs = runSlk("", "setup", "--config", cfgPath, "--non-interactive"); code != 0 {
		t.Fatalf("second exit %d\n%s\n%s", code, out, errs)
	}
	if second, _ := os.ReadFile(cfgPath); !bytes.Equal(first, second) {
		t.Errorf("second run changed the file:\n%s\n---\n%s", first, second)
	}
	if strings.Contains(out, "route:") {
		t.Errorf("second run added a route:\n%s", out)
	}
}

func TestSlackSetupFlagsAndEnv(t *testing.T) {
	useSlack(t, newCLISlack())
	t.Setenv(envSlackGatewayURL, "http://gw.example:3333")
	t.Setenv(envSlackChannel, "C0123ABCD")
	cfgPath := filepath.Join(t.TempDir(), "channels.yaml")
	code, out, errs := runSlk("", "setup", "--config", cfgPath, "--non-interactive",
		"--allow-from", slackMe+",U0OTHER01", "--poll-interval", "45s", "--route-topics", "post-office,wolf")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	cfg := readCfg(t, cfgPath)
	o := cfg.Channels["slack"].Options
	if o["gateway_url"] != "http://gw.example:3333" || o["channel"] != "C0123ABCD" || o["poll_interval"] != "45s" || fmt.Sprint(o["allow_from"]) != "["+slackMe+" U0OTHER01]" {
		t.Errorf("options = %v", o)
	}
	if len(cfg.Routes) != 2 || cfg.Routes[1].Topic != "wolf" || cfg.Routes[1].Chat != "C0123ABCD" {
		t.Errorf("routes = %+v", cfg.Routes)
	}
}

func TestSlackSetupValidation(t *testing.T) {
	useSlack(t, newCLISlack())
	cfgPath := filepath.Join(t.TempDir(), "channels.yaml")
	for _, args := range [][]string{
		{"--conversation", "general"},
		{"--allow-from", "*"},
		{"--poll-interval", "1s"},
		{"--gateway-url", "localhost:3333"},
	} {
		code, _, errs := runSlk("", append([]string{"setup", "--config", cfgPath, "--non-interactive"}, args...)...)
		if code != 2 || errs == "" {
			t.Errorf("%v: exit %d %q", args, code, errs)
		}
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Error("a rejected setup wrote the config")
	}
}

func TestSlackSetupGatewayDown(t *testing.T) {
	useSlack(t, nil)
	cfgPath := filepath.Join(t.TempDir(), "channels.yaml")
	code, out, errs := runSlk("", "setup", "--config", cfgPath, "--non-interactive")
	if code != 1 || !strings.Contains(out, "could not mint a gateway") || !strings.Contains(errs, "nothing was written") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Error("config written although the check failed")
	}
	// --skip-validate needs the ids given.
	if code, _, errs = runSlk("", "setup", "--config", cfgPath, "--non-interactive", "--skip-validate"); code != 1 || !strings.Contains(errs, "missing required value: conversation") {
		t.Errorf("skip-validate without ids: %d %q", code, errs)
	}
	code, out, errs = runSlk("", "setup", "--config", cfgPath, "--non-interactive", "--skip-validate", "--conversation", slackMe)
	if code != 0 {
		t.Fatalf("skip-validate: %d\n%s\n%s", code, out, errs)
	}
	// The DM-to-self by user id implies allow_from: that user.
	if o := readCfg(t, cfgPath).Channels["slack"].Options; fmt.Sprint(o["allow_from"]) != "["+slackMe+"]" {
		t.Errorf("options = %v", o)
	}
}

func TestSlackSetupInteractivePicksFromTheList(t *testing.T) {
	useSlack(t, newCLISlack())
	cfgPath := filepath.Join(t.TempDir(), "channels.yaml")
	// gateway: keep; conversation: filter "wm", then pick 2 (#wm-jim);
	// allow_from: keep (you).
	code, out, errs := runSlk("\nwm\n2\n\n", "setup", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "#general (public channel)") || !strings.Contains(out, "#wm-jim (private channel)") {
		t.Errorf("listing:\n%s", out)
	}
	if strings.Contains(out, "DM with jim.clark") {
		t.Errorf("the DM-to-self is listed twice:\n%s", out)
	}
	cfg := readCfg(t, cfgPath)
	if o := cfg.Channels["slack"].Options; o["channel"] != "G0WMJIM01" || fmt.Sprint(o["allow_from"]) != "["+slackMe+"]" {
		t.Errorf("options = %v", o)
	}
	if cfg.Routes[0].Chat != "G0WMJIM01" {
		t.Errorf("routes = %+v", cfg.Routes)
	}
}

func TestSlackSetupList(t *testing.T) {
	useSlack(t, newCLISlack())
	cfgPath := filepath.Join(t.TempDir(), "channels.yaml")
	code, out, errs := runSlk("", "setup", "--config", cfgPath, "--list")
	if code != 0 || !strings.Contains(out, slackMe+"    DM-to-self (you)") || !strings.Contains(out, "C0GENERAL") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Error("--list wrote the config")
	}
}

func writeSlackConfig(t *testing.T, routes string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "channels.yaml")
	cfg := "channels:\n  slack:\n    type: slack\n    options:\n      channel: " + slackMe + "\n      allow_from: [" + slackMe + "]\n" + routes
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func TestSlackStatus(t *testing.T) {
	fake := newCLISlack()
	useSlack(t, fake)
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	cfgPath := writeSlackConfig(t, "routes:\n  - {topic: post-office, channel: slack, chat: "+slackMe+"}\n")

	stateDir := filepath.Join(home, "slack")
	_ = os.MkdirAll(stateDir, 0o700)
	watch := `{"version":1,"channel_id":"D0CBT6QV8NT","top_cursor":"1795200000.000100","watches":[` +
		`{"id":"D0CBT6QV8NT:1795200001.000100","opened_at":"2026-11-20T10:00:00Z","reply_count":1,"thread_cursor":"1795200002.000100"},` +
		`{"id":"D0CBT6QV8NT:1795200003.000100","opened_at":"2026-11-20T10:05:00Z","reply_count":0,"closed":{"outcome":"answered","via":"tui","at":"2026-11-20T10:06:00Z"}}]}`
	if err := os.WriteFile(filepath.Join(stateDir, slack.WatchFile), []byte(watch), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errs := runSlk("", "status", "--config", cfgPath, "--sessions-root", sessions)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{
		"serves every tool the channel uses",
		"daemon gateway: none recorded",
		"Conversation id:     D0CBT6QV8NT",
		"Watched requests:    2",
		"D0CBT6QV8NT:1795200001.000100", "open",
		"closed: answered via tui",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}

	// A gateway without the read tools is a problem.
	fake.tools = append(fake.tools[:1:1], fake.tools[2:]...) // no slack_add_reaction
	if code, out, _ = runSlk("", "status", "--config", cfgPath, "--sessions-root", sessions); code != 1 || !strings.Contains(out, "does not serve slack_add_reaction") {
		t.Errorf("missing tools: %d\n%s", code, out)
	}
}

func TestSlackStatusOfflineAndWarnings(t *testing.T) {
	useSlack(t, nil) // never dialled offline
	cfgPath := writeSlackConfig(t, "")
	code, out, _ := runSlk("", "status", "--config", cfgPath, "--offline", "--sessions-root", filepath.Join(t.TempDir(), "sessions"))
	if code != 0 || !strings.Contains(out, "skipped (--offline)") || !strings.Contains(out, "No post-office route") {
		t.Errorf("exit %d\n%s", code, out)
	}
	// A wildcard route counts when there is no post-office route.
	cfgPath = writeSlackConfig(t, "routes:\n  - {topic: \"*\", channel: slack, chat: "+slackMe+"}\n")
	if _, out, _ = runSlk("", "status", "--config", cfgPath, "--offline", "--sessions-root", filepath.Join(t.TempDir(), "sessions")); strings.Contains(out, "No post-office route") {
		t.Errorf("wildcard route not counted:\n%s", out)
	}
	if code, out, _ = runSlk("", "status", "--config", filepath.Join(t.TempDir(), "none.yaml")); code != 1 || !strings.Contains(out, "not configured") {
		t.Errorf("unconfigured: %d %s", code, out)
	}
}

func TestSlackTestRoundTrip(t *testing.T) {
	fake := newCLISlack()
	useSlack(t, fake)
	cfgPath := writeSlackConfig(t, "")
	code, out, errs := runSlk("", "test", "--config", cfgPath, "--wait", "5s", "ping")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "sent to D0CBT6QV8NT: D0CBT6QV8NT:1795200001.000100") || !strings.Contains(out, "reply from Someone ("+slackMe+"): pong") || !strings.Contains(out, "round trip works") {
		t.Errorf("output:\n%s", out)
	}
	if m := fake.sent[0]["message"].(string); !strings.HasPrefix(m, slack.TestPrefix+" ping") || fake.sent[0]["channel_id"] != slackMe {
		t.Errorf("sent %v", fake.sent[0])
	}
	if strings.Join(fake.reactions, ",") != "white_check_mark" {
		t.Errorf("reactions = %v", fake.reactions)
	}
}

func TestSlackTestReplyFromOutsideAllowFrom(t *testing.T) {
	fake := newCLISlack()
	fake.replyFrom = "U0STRANGER"
	useSlack(t, fake)
	code, out, _ := runSlk("", "test", "--config", writeSlackConfig(t, ""), "--wait", "5s")
	if code != 1 || !strings.Contains(out, "not in allow_from") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSlackTestTimeoutNoWaitAndErrors(t *testing.T) {
	fake := newCLISlack()
	fake.replyAfter = 1 << 30
	useSlack(t, fake)
	cfgPath := writeSlackConfig(t, "")
	if code, _, errs := runSlk("", "test", "--config", cfgPath, "--wait", "20ms"); code != 1 || !strings.Contains(errs, "no reply within") {
		t.Errorf("timeout: %d %q", code, errs)
	}
	if code, out, _ := runSlk("", "test", "--config", cfgPath, "--no-wait", "--to", "C0123ABCD"); code != 0 || strings.Contains(out, "waiting") {
		t.Errorf("no-wait: %d %s", code, out)
	}
	if got := fake.sent[len(fake.sent)-1]; got["channel_id"] != "C0123ABCD" || strings.Contains(got["message"].(string), "Reply in this thread") {
		t.Errorf("no-wait sent %v", got)
	}
	if code, _, _ := runSlk("", "test", "--config", cfgPath, "--to", "nope"); code != 2 {
		t.Errorf("bad --to: %d", code)
	}
	if code, _, errs := runSlk("", "test", "--config", filepath.Join(t.TempDir(), "none.yaml")); code != 1 || !strings.Contains(errs, "not configured") {
		t.Errorf("unconfigured: %d %q", code, errs)
	}
	useSlack(t, nil)
	if code, _, errs := runSlk("", "test", "--config", cfgPath); code != 1 || !strings.Contains(errs, "orch slack status") {
		t.Errorf("gateway down: %d %q", code, errs)
	}
}
