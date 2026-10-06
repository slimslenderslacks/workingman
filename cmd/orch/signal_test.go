package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/channels"
)

func runSig(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runSignal(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func sigSetupArgs(cfg string, cli *fakeSignalCLI, extra ...string) []string {
	return append([]string{"setup", "--config", cfg, "--non-interactive", "--http-url", cli.srv.URL,
		"--account", sigAccount, "--allow-from", sigOwner}, extra...)
}

func TestSignalSetupWritesConfigIdempotently(t *testing.T) {
	cli := newFakeSignalCLI(t)
	cfg := filepath.Join(t.TempDir(), "channels.yaml")

	code, out, errs := runSig(t, "", sigSetupArgs(cfg, cli)...)
	if code != 0 {
		t.Fatalf("setup exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "signal-cli reachable") || !strings.Contains(out, "is registered") {
		t.Errorf("setup did not report the signal-cli check:\n%s", out)
	}
	if strings.Contains(out, sigOwner) || strings.Contains(out, sigAccount) {
		t.Errorf("setup printed an unmasked number:\n%s", out)
	}
	first, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := channels.Parse(first)
	if err != nil {
		t.Fatalf("written config is invalid: %v\n%s", err, first)
	}
	if parsed.Channels["signal"].Type != "signal" || len(parsed.Routes) != 2 {
		t.Errorf("unexpected config:\n%s", first)
	}
	for _, r := range parsed.Routes {
		if r.Channel != "signal" || r.Chat != sigOwner || (r.Topic != "wolf" && r.Topic != "workingman") {
			t.Errorf("unexpected route %+v", r)
		}
	}

	// Re-running with the same input changes nothing; re-running with nothing
	// new keeps what is there.
	if code, out, errs = runSig(t, "", sigSetupArgs(cfg, cli)...); code != 0 {
		t.Fatalf("second setup exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "already configured") || strings.Contains(out, sigOwner) {
		t.Errorf("re-run did not show a masked summary:\n%s", out)
	}
	second, _ := os.ReadFile(cfg)
	if !bytes.Equal(first, second) {
		t.Errorf("re-running setup changed the config:\n--- first\n%s\n--- second\n%s", first, second)
	}
	if code, out, errs = runSig(t, "", "setup", "--config", cfg, "--non-interactive", "--skip-validate"); code != 0 {
		t.Fatalf("keep-everything setup exit %d\n%s\n%s", code, out, errs)
	}
	if third, _ := os.ReadFile(cfg); !bytes.Equal(first, third) {
		t.Errorf("a setup with no new values changed the config:\n%s", third)
	}
}

func TestSignalSetupPreservesOtherChannels(t *testing.T) {
	cli := newFakeSignalCLI(t)
	cfg := filepath.Join(t.TempDir(), "channels.yaml")
	existing := `# my config
channels:
  whatsapp:
    type: whatsapp
    enabled: false   # off for now
routes:
  - {topic: wolf, channel: whatsapp, chat: "15551234567"}
`
	if err := os.WriteFile(cfg, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := runSig(t, "", sigSetupArgs(cfg, cli)...); code != 0 {
		t.Fatalf("setup exit %d\n%s\n%s", code, out, errs)
	}
	got, _ := os.ReadFile(cfg)
	for _, want := range []string{"# my config", "# off for now", "channel: whatsapp", "channel: signal"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("config lost %q:\n%s", want, got)
		}
	}
	parsed, err := channels.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Routes) != 3 {
		t.Errorf("want the whatsapp route plus two signal routes, got %+v", parsed.Routes)
	}
	if info, _ := os.Stat(cfg); info.Mode().Perm() != 0o600 {
		t.Errorf("config mode changed to %v", info.Mode().Perm())
	}
}

func TestSignalSetupInteractive(t *testing.T) {
	cli := newFakeSignalCLI(t)
	cfg := filepath.Join(t.TempDir(), "channels.yaml")
	// URL, account, owner, groups? yes, group ids.
	in := strings.Join([]string{cli.srv.URL, "+1 555 000 1111", "+1 (555) 123-4567", "y", "abc123=="}, "\n") + "\n"
	code, out, errs := runSig(t, in, "setup", "--config", cfg)
	if code != 0 {
		t.Fatalf("setup exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "signal-cli daemon --http") {
		t.Errorf("setup did not print the signal-cli instructions:\n%s", out)
	}
	got, _ := os.ReadFile(cfg)
	parsed, err := channels.Parse(got)
	if err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	opts := parsed.Channels["signal"].Options
	if opts["account"] != sigAccount {
		t.Errorf("account = %v\n%s", opts["account"], got)
	}
	access, _ := opts["access"].(map[string]any)
	if access["groups"] != true || !strings.Contains(string(got), "abc123==") || !strings.Contains(string(got), sigOwner) {
		t.Errorf("owner or group opt-in not saved:\n%s", got)
	}
}

func TestSignalSetupRefusesUnreachableAndUnregistered(t *testing.T) {
	cli := newFakeSignalCLI(t)
	cfg := filepath.Join(t.TempDir(), "channels.yaml")

	// Unregistered account.
	args := sigSetupArgs(cfg, cli)
	for i, a := range args {
		if a == sigAccount {
			args[i] = "+15557770000"
		}
	}
	if code, out, _ := runSig(t, "", args...); code != 1 || !strings.Contains(out, "not registered") {
		t.Errorf("unregistered account: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(cfg); err == nil {
		t.Error("config was written despite the failed check")
	}

	// Unreachable daemon.
	url := cli.srv.URL
	cli.srv.Close()
	args = sigSetupArgs(cfg, cli)
	_ = url
	if code, out, _ := runSig(t, "", args...); code != 1 || !strings.Contains(out, "not reachable") {
		t.Errorf("unreachable daemon: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(cfg); err == nil {
		t.Error("config was written despite the failed check")
	}
	// --skip-validate writes anyway.
	if code, out, errs := runSig(t, "", append(args, "--skip-validate")...); code != 0 {
		t.Errorf("skip-validate exit %d\n%s\n%s", code, out, errs)
	}
}

func TestSignalSetupValidatesInput(t *testing.T) {
	cli := newFakeSignalCLI(t)
	cfg := filepath.Join(t.TempDir(), "channels.yaml")
	for name, extra := range map[string][]string{
		"bad owner":          {"--allow-from", "not-a-number"},
		"groups without ids": {"--groups"},
	} {
		if code, _, errs := runSig(t, "", sigSetupArgs(cfg, cli, extra...)...); code == 0 {
			t.Errorf("%s: setup succeeded; stderr %q", name, errs)
		}
	}
	if code, _, _ := runSig(t, "", "setup", "--config", cfg, "--non-interactive", "--skip-validate"); code != 1 {
		t.Errorf("setup with no account succeeded")
	}
}

func TestSignalStatusAndTest(t *testing.T) {
	cli := newFakeSignalCLI(t)
	cfg := filepath.Join(t.TempDir(), "channels.yaml")

	if code, out, _ := runSig(t, "", "status", "--config", cfg); code != 1 || !strings.Contains(out, "orch signal setup") {
		t.Errorf("status of an unconfigured channel: exit %d\n%s", code, out)
	}
	if code, _, errs := runSig(t, "", "test", "--config", cfg); code != 1 || !strings.Contains(errs, "not configured") {
		t.Errorf("test of an unconfigured channel: exit %d\n%s", code, errs)
	}
	if code, out, errs := runSig(t, "", sigSetupArgs(cfg, cli)...); code != 0 {
		t.Fatalf("setup: %d\n%s\n%s", code, out, errs)
	}

	code, out, _ := runSig(t, "", "status", "--config", cfg)
	if code != 0 || !strings.Contains(out, "signal-cli reachable") || strings.Contains(out, sigOwner) {
		t.Errorf("status: exit %d\n%s", code, out)
	}
	if code, out, _ = runSig(t, "", "status", "--config", cfg, "--offline"); code != 0 || !strings.Contains(out, "skipped") {
		t.Errorf("offline status: exit %d\n%s", code, out)
	}

	code, out, errs := runSig(t, "", "test", "--config", cfg, "hello", "there")
	if code != 0 || !strings.Contains(out, "sent to ***4567") {
		t.Fatalf("test: exit %d\n%s\n%s", code, out, errs)
	}
	if got := cli.messages(); len(got) != 1 || got[0].To != sigOwner || got[0].Body != "hello there" {
		t.Errorf("fake signal-cli received %+v", got)
	}
	if code, _, errs = runSig(t, "", "test", "--config", cfg, "--to", "+15550009999", "x"); code != 0 {
		t.Errorf("test --to: exit %d %s", code, errs)
	}
	if got := cli.messages(); len(got) != 2 || got[1].To != "+15550009999" {
		t.Errorf("--to not honoured: %+v", got)
	}

	cli.srv.Close()
	if code, _, _ = runSig(t, "", "status", "--config", cfg); code != 1 {
		t.Errorf("status with signal-cli down: exit %d, want 1", code)
	}
	if code, _, errs = runSig(t, "", "test", "--config", cfg); code != 1 || !strings.Contains(errs, "send failed") {
		t.Errorf("test with signal-cli down: exit %d\n%s", code, errs)
	}
}

func TestSignalUsageAndUnknown(t *testing.T) {
	if code, out, _ := runSig(t, "", "--help"); code != 0 || !strings.Contains(out, "setup") || !strings.Contains(out, "status") || !strings.Contains(out, "test") {
		t.Errorf("--help: exit %d\n%s", code, out)
	}
	if code, _, errs := runSig(t, "", "bogus"); code != 2 || !strings.Contains(errs, "unknown subcommand") {
		t.Errorf("unknown subcommand: exit %d\n%s", code, errs)
	}
	if code, _, _ := runSig(t, ""); code != 2 {
		t.Errorf("no subcommand: exit %d", code)
	}
}
