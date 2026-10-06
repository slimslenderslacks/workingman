package whatsapp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeNodeBinary writes a stand-in `node`: it reports v20 for --version, and
// otherwise acts as the bridge in --pair mode by writing creds.json into the
// --session directory.
func fakeNodeBinary(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake node")
	}
	p := filepath.Join(t.TempDir(), "node")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo v20.11.0; exit 0; fi
session=""
pair=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session) session="$2"; shift ;;
    --pair) pair=1 ;;
  esac
  shift
done
echo "FAKE QR CODE"
if [ -n "$pair" ] && [ -n "$session" ]; then echo '{}' > "$session/creds.json"; fi
exit ${FAKE_NODE_EXIT:-0}
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeNPMBinary writes a stand-in `npm ci` that "installs" Baileys.
func fakeNPMBinary(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "npm")
	script := "#!/bin/sh\nmkdir -p node_modules/@whiskeysockets/baileys\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
