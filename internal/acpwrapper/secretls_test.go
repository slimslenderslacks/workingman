package acpwrapper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/session"
)

// When `sbx secret ls` never yields the secrets document, Run fails the
// session with an error naming it, rather than starting the agent against the
// default API (which fails later, on a TLS-inspecting network, as a
// self-signed certificate).
func TestRunFailsWhenSecretLsExhausted(t *testing.T) {
	ws := t.TempDir()
	bin := filepath.Join(t.TempDir(), "sbx")
	count := filepath.Join(t.TempDir(), "secret-ls-calls")
	script := `#!/bin/sh
case "$1" in
ls) echo '[{"name":"acp-x","workspaces":["` + ws + `"]}]' ;;
secret) echo x >> ` + count + `; echo 'WARN: docker hub refresh lock held by another process: context deadline exceeded'; exit 1 ;;
*) exit 0 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "sessions")
	var slept []time.Duration
	c := Config{
		SessionID:    "x",
		SandboxName:  "acp-x",
		SessionsRoot: root,
		KitPath:      "/kits/acp-kit",
		SbxPath:      bin,
		Workspaces:   []string{ws},
		Persistent:   true,
		sbxSleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	}
	err := Run(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "sbx secret ls --json failed after 5 attempts") || !strings.Contains(err.Error(), "docker hub refresh lock") {
		t.Fatalf("Run err = %v, want the sbx secret ls exhaustion failure quoting sbx's output", err)
	}
	if data, _ := os.ReadFile(count); strings.Count(string(data), "x") != 5 {
		t.Errorf("sbx secret ls ran %d times, want 5", strings.Count(string(data), "x"))
	}
	if len(slept) != 4 {
		t.Errorf("slept %v, want 4 backoffs", slept)
	}
	rec, rerr := (session.Store{Root: root}).Read("x")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rec.Status != session.StatusFailed || !strings.Contains(rec.Error, "sbx secret ls") {
		t.Errorf("session.json = %+v, want failed naming sbx secret ls", rec)
	}
}

// The reported failure: `sbx ls` printing a WARN line after its JSON must not
// fail the launch, and the `sbx ls` call is retried like `sbx secret ls`.
func TestEnsureSandboxToleratesSbxLsNoise(t *testing.T) {
	for _, tc := range []struct{ name, out string }{
		{"warn after", `[{"name":"acp-s","workspaces":["/repo"]}]` + "\nWARN: docker hub refresh lock held by another process: context deadline exceeded\n"},
		{"warn before", "WARN: docker hub refresh lock held by another process\n" + `{"sandboxes":[{"name":"acp-s","workspaces":["/repo"]}]}`},
		{"json log lines around", `{"time":"t","level":"WARN","msg":"lock held"}` + "\n" + `[{"name":"acp-s","workspaces":["/repo"]}]` + "\n" + `{"time":"t","level":"WARN","msg":"lock held"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSbx{lsOutput: tc.out}
			c := Config{SessionID: "s", SandboxName: "acp-s", SbxPath: "sbx", Workspaces: []string{"/repo"}}
			if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
				t.Fatalf("ensureSandbox: %v", err)
			}
			if got := subcommands(f.calls); strings.Join(got, ",") != "ls" {
				t.Errorf("sbx calls = %v, want only ls (reuse)", got)
			}
		})
	}
}

func TestEnsureSandboxRetriesSbxLs(t *testing.T) {
	calls := 0
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if args[0] != "ls" {
			return nil, nil
		}
		calls++
		if calls < 3 {
			return []byte("WARN: docker hub refresh lock held by another process"), errors.New("exit status 1")
		}
		return []byte(`[{"name":"acp-s","workspaces":["/repo"]}]`), nil
	}
	var slept int
	c := Config{SessionID: "s", SandboxName: "acp-s", SbxPath: "sbx", Workspaces: []string{"/repo"},
		SessionsRoot: filepath.Join(t.TempDir(), "sessions"),
		sbxSleep:     func(context.Context, time.Duration) error { slept++; return nil }}
	if _, err := ensureSandbox(context.Background(), run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	if calls != 3 || slept != 2 {
		t.Errorf("sbx ls ran %d times with %d backoffs, want 3 and 2", calls, slept)
	}
}

// When `sbx ls` never yields a list, Run fails the session naming it.
func TestRunFailsWhenSbxLsExhausted(t *testing.T) {
	ws := t.TempDir()
	bin := filepath.Join(t.TempDir(), "sbx")
	script := `#!/bin/sh
case "$1" in
ls) echo 'WARN: docker hub refresh lock held by another process: context deadline exceeded' ;;
*) exit 0 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "sessions")
	slept := 0
	c := Config{SessionID: "x", SandboxName: "acp-x", SessionsRoot: root, KitPath: "/kits/acp-kit", SbxPath: bin,
		Workspaces: []string{ws}, Persistent: true,
		sbxSleep: func(context.Context, time.Duration) error { slept++; return nil }}
	err := Run(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "sbx ls --json failed after 5 attempts") || !strings.Contains(err.Error(), "docker hub refresh lock") {
		t.Fatalf("Run err = %v, want the sbx ls exhaustion failure quoting sbx's output", err)
	}
	if slept != 4 {
		t.Errorf("slept %d times, want 4 backoffs", slept)
	}
	rec, rerr := (session.Store{Root: root}).Read("x")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rec.Status != session.StatusFailed || !strings.Contains(rec.Error, "sbx ls --json") {
		t.Errorf("session.json = %+v, want failed naming sbx ls --json", rec)
	}
}
