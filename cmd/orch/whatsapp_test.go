package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWhatsAppUsageAndUnknown(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runWhatsApp(nil, nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "pair") {
		t.Fatalf("no args: code=%d stderr=%q", code, errb.String())
	}
	errb.Reset()
	if code := runWhatsApp([]string{"bogus"}, nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), `unknown subcommand "bogus"`) {
		t.Fatalf("unknown: code=%d stderr=%q", code, errb.String())
	}
	out.Reset()
	if code := runWhatsApp([]string{"help"}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "pair") {
		t.Fatalf("help: code=%d stdout=%q", code, out.String())
	}
}

// fakeNodeAndNPM writes a node that reports v20 and, in --pair mode, saves a
// session; plus a sibling npm that "installs" Baileys.
func fakeNodeAndNPM(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fakes")
	}
	dir := t.TempDir()
	node := `#!/bin/sh
if [ "$1" = "--version" ]; then echo v20.1.0; exit 0; fi
while [ $# -gt 0 ]; do [ "$1" = "--session" ] && session="$2"; shift; done
echo "SCAN ME"
echo '{}' > "$session/creds.json"
`
	npm := "#!/bin/sh\nmkdir -p node_modules/@whiskeysockets/baileys\n"
	if err := os.WriteFile(filepath.Join(dir, "node"), []byte(node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "npm"), []byte(npm), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "node")
}

func TestWhatsAppPairFlagsOverrideDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WORKINGMAN_CHANNELS_CONFIG", "")
	node := fakeNodeAndNPM(t)
	session, install := filepath.Join(t.TempDir(), "s"), t.TempDir()

	var out, errb bytes.Buffer
	code := runWhatsApp([]string{"pair", "--node", node, "--session-dir", session, "--install-dir", install}, nil, &out, &errb)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "SCAN ME") {
		t.Fatalf("QR output not shown: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(session, "creds.json")); err != nil {
		t.Fatalf("session not stored where asked: %v", err)
	}
	// Nothing leaked into the real default location.
	if _, err := os.Stat(filepath.Join(home, ".workingman")); err == nil {
		t.Fatal("pair wrote to the default ~/.workingman despite flags")
	}
}

func TestWhatsAppPairReadsBridgeOptionsFromChannelsYAML(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	node := fakeNodeAndNPM(t)
	session, install := filepath.Join(t.TempDir(), "yaml-session"), t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "channels.yaml")
	cfg := "channels:\n  whatsapp:\n    type: whatsapp\n    options:\n      mode: bridge\n      bridge:\n        node: " + node +
		"\n        session_dir: " + session + "\n        install_dir: " + install + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runWhatsApp([]string{"pair", "--config", cfgPath}, nil, &out, &errb); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(session, "creds.json")); err != nil {
		t.Fatalf("session_dir from channels.yaml ignored: %v", err)
	}
}

func TestWhatsAppPairErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := runWhatsApp([]string{"pair", "--config", filepath.Join(t.TempDir(), "nope.yaml")}, nil, &out, &errb); code != 1 {
		t.Fatalf("missing explicit config: code=%d", code)
	}
	errb.Reset()
	if code := runWhatsApp([]string{"pair", "--node", "/nonexistent/node"}, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "Node.js 18") {
		t.Fatalf("missing node: code=%d stderr=%q", code, errb.String())
	}
	errb.Reset()
	if code := runWhatsApp([]string{"pair", "extra"}, nil, &out, &errb); code != 2 {
		t.Fatalf("extra arg: code=%d", code)
	}
}
