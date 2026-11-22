package acpwrapper

import (
	"context"
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
		secretLsSleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	}
	err := Run(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "sbx secret ls failed after 5 attempts") || !strings.Contains(err.Error(), "docker hub refresh lock") {
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
