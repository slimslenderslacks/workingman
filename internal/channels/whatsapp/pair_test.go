package whatsapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/channels/whatsapp/bridge"
)

// pairEnv lays out a fake node with a sibling fake npm (FindNPM prefers the
// sibling) and returns options pointing at fresh session/install dirs.
func pairEnv(t *testing.T) PairOptions {
	t.Helper()
	node := fakeNodeBinary(t)
	fakeNPMBinary(t, filepath.Dir(node))
	return PairOptions{
		Bridge: BridgeOptions{Node: node, SessionDir: filepath.Join(t.TempDir(), "session"), InstallDir: t.TempDir()},
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	}
}

func TestPairInstallsRunsBridgeAndStoresSession(t *testing.T) {
	opts := pairEnv(t)
	if err := Pair(context.Background(), opts); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if !Paired(opts.Bridge.SessionDir) {
		t.Fatal("no session stored")
	}
	if !bridge.Ready(opts.Bridge.InstallDir) {
		t.Fatal("bridge not installed")
	}
	out := opts.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "FAKE QR CODE") || !strings.Contains(out, "Paired") {
		t.Fatalf("output = %q (the bridge's QR must reach the terminal)", out)
	}
}

func TestPairLeavesExistingSessionAlone(t *testing.T) {
	opts := pairEnv(t)
	os.MkdirAll(opts.Bridge.SessionDir, 0o700)
	os.WriteFile(filepath.Join(opts.Bridge.SessionDir, "creds.json"), []byte(`{"keep":true}`), 0o600)
	if err := Pair(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(opts.Bridge.SessionDir, "creds.json"))
	if string(data) != `{"keep":true}` {
		t.Fatalf("session was modified: %s", data)
	}
	if !strings.Contains(opts.Stdout.(*bytes.Buffer).String(), "--reset") {
		t.Fatal("no hint about --reset")
	}
}

func TestPairResetReplacesSession(t *testing.T) {
	opts := pairEnv(t)
	os.MkdirAll(opts.Bridge.SessionDir, 0o700)
	os.WriteFile(filepath.Join(opts.Bridge.SessionDir, "creds.json"), []byte(`{"old":true}`), 0o600)
	os.WriteFile(filepath.Join(opts.Bridge.SessionDir, "app-state-sync-key-1.json"), []byte(`{}`), 0o600)
	opts.Reset = true
	if err := Pair(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(opts.Bridge.SessionDir, "app-state-sync-key-1.json")); err == nil {
		t.Fatal("old session files survived --reset")
	}
	data, _ := os.ReadFile(filepath.Join(opts.Bridge.SessionDir, "creds.json"))
	if strings.Contains(string(data), "old") {
		t.Fatal("old creds survived --reset")
	}
}

func TestPairResetRefusesNonSessionDirectory(t *testing.T) {
	opts := pairEnv(t)
	os.MkdirAll(opts.Bridge.SessionDir, 0o700)
	precious := filepath.Join(opts.Bridge.SessionDir, "thesis.docx")
	os.WriteFile(precious, []byte("important"), 0o600)
	opts.Reset = true
	if err := Pair(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if _, err := os.Stat(precious); err != nil {
		t.Fatal("--reset deleted a non-session file")
	}
}

func TestPairFailsClearlyWithoutNode(t *testing.T) {
	opts := pairEnv(t)
	opts.Bridge.Node = "/nonexistent/node"
	err := Pair(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "Node.js 18") {
		t.Fatalf("err = %v", err)
	}
}

func TestPairReportsBridgeFailure(t *testing.T) {
	opts := pairEnv(t)
	t.Setenv("FAKE_NODE_EXIT", "1")
	if err := Pair(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "pairing failed") {
		t.Fatalf("err = %v", err)
	}
}
