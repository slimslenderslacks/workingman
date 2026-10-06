package bridge

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExtractWritesBridgeSources(t *testing.T) {
	dir := t.TempDir()
	if err := Extract(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"bridge.js", "helpers.js", "outbound_ids.js", "package.json", "package-lock.json", "NOTICE.md", "LICENSE-hermes-agent"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "helpers.test.mjs")); err == nil {
		t.Error("node tests should not be extracted")
	}
	// The vendored bridge must stay loopback-only and authenticated.
	js, _ := os.ReadFile(filepath.Join(dir, "bridge.js"))
	for _, must := range []string{"'127.0.0.1'", "WORKINGMAN_BRIDGE_TOKEN", "timingSafeEqual"} {
		if !strings.Contains(string(js), must) {
			t.Errorf("bridge.js lost %s", must)
		}
	}
	lic, _ := os.ReadFile(filepath.Join(dir, "LICENSE-hermes-agent"))
	if !strings.Contains(string(lic), "Nous Research") {
		t.Error("license attribution missing")
	}
}

func TestExtractRestoresModifiedFile(t *testing.T) {
	dir := t.TempDir()
	if err := Extract(dir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "bridge.js")
	want, _ := os.ReadFile(p)
	os.WriteFile(p, []byte("tampered"), 0o644)
	if err := Extract(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != string(want) {
		t.Fatal("Extract did not restore bridge.js")
	}
}

// fakeNPM stands in for `npm ci`: it creates node_modules/@whiskeysockets/baileys
// and records that it ran.
func fakeNPM(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake npm")
	}
	p := filepath.Join(dir, "npm")
	script := "#!/bin/sh\necho \"$@\" >> npm-calls.txt\nmkdir -p node_modules/@whiskeysockets/baileys\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallRunsNpmOnceAndMarksReady(t *testing.T) {
	install := t.TempDir()
	npm := fakeNPM(t, t.TempDir())
	if Ready(install) {
		t.Fatal("Ready before Install")
	}
	if err := Install(context.Background(), install, npm, nil); err != nil {
		t.Fatal(err)
	}
	if !Ready(install) {
		t.Fatal("not Ready after Install")
	}
	calls, _ := os.ReadFile(filepath.Join(install, "npm-calls.txt"))
	if !strings.Contains(string(calls), "ci --omit=dev --ignore-scripts") {
		t.Fatalf("npm args = %q", calls)
	}
	// Second Install is a no-op.
	if err := Install(context.Background(), install, npm, nil); err != nil {
		t.Fatal(err)
	}
	calls, _ = os.ReadFile(filepath.Join(install, "npm-calls.txt"))
	if n := strings.Count(string(calls), "\n"); n != 1 {
		t.Fatalf("npm ran %d times, want 1", n)
	}
}

func TestInstallReinstallsWhenMarkerStale(t *testing.T) {
	install := t.TempDir()
	npm := fakeNPM(t, t.TempDir())
	if err := Install(context.Background(), install, npm, nil); err != nil {
		t.Fatal(err)
	}
	markers, _ := filepath.Glob(filepath.Join(install, ".deps-*"))
	for _, m := range markers {
		os.Rename(m, filepath.Join(install, ".deps-old"))
	}
	if Ready(install) {
		t.Fatal("Ready with a stale marker")
	}
	if err := Install(context.Background(), install, npm, nil); err != nil {
		t.Fatal(err)
	}
	if !Ready(install) {
		t.Fatal("not Ready after reinstall")
	}
	if _, err := os.Stat(filepath.Join(install, ".deps-old")); err == nil {
		t.Fatal("stale marker not removed")
	}
}

func TestOpenLogRotatesLargeFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "logs", "whatsapp-bridge.log")
	f, err := OpenLog(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(make([]byte, maxLogBytes+1))
	f.Close()
	f, err = OpenLog(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf("not rotated: %v", err)
	}
	if fi, _ := os.Stat(p); fi.Size() != 0 {
		t.Fatalf("new log size = %d", fi.Size())
	}
}
