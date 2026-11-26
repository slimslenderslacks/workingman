package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/api"
)

func TestAPITokenSubcommand(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), ".workingman", "sessions")
	path := filepath.Join(filepath.Dir(sessions), "api", "token")
	run := func(args ...string) (int, string, string) {
		var out, errs bytes.Buffer
		code := runAPI(append(args, "--sessions-root", sessions), &out, &errs)
		return code, out.String(), errs.String()
	}

	code, out, errs := run("token")
	if code != 0 || !strings.Contains(out, "wrote a new token to "+path) {
		t.Fatalf("first token: %d %q %q", code, out, errs)
	}
	tok, err := api.ReadToken(path)
	if err != nil {
		t.Fatal(err)
	}

	code, out, _ = run("token", "--print")
	if code != 0 || out != tok+"\n" {
		t.Errorf("--print = %d %q, want the token", code, out)
	}
	code, out, _ = run("token")
	if code != 0 || !strings.Contains(out, "already exists") {
		t.Errorf("second token: %d %q", code, out)
	}
	code, out, _ = run("token", "--rotate", "--print")
	if code != 0 || strings.TrimSpace(out) == tok {
		t.Errorf("--rotate --print = %d %q, want a new token", code, out)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("token file %v %v, want 0600", fi.Mode(), err)
	}

	var stderr bytes.Buffer
	if code := runAPI(nil, &bytes.Buffer{}, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: orch api token") {
		t.Errorf("orch api with no subcommand: %d %q", code, stderr.String())
	}
	if code := runAPI([]string{"nope"}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("unknown subcommand exit %d, want 2", code)
	}
}

func TestAPIConfigForUsesTheWorkingmanDir(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "wm", "sessions")
	wm := filepath.Dir(sessions)
	cfg, err := apiConfigFor("unix", "", sessions)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != filepath.Join(wm, "api.sock") || cfg.TokenPath != filepath.Join(wm, "api", "token") {
		t.Errorf("unix config = %+v", cfg)
	}
	if cfg, err := apiConfigFor("off", "", sessions); err != nil || cfg.Enabled() {
		t.Errorf("off config = %+v, %v", cfg, err)
	}
	if _, err := apiConfigFor("loopback", "0.0.0.0:7078", sessions); err == nil {
		t.Error("a non-loopback --api-listen was accepted")
	}
}
