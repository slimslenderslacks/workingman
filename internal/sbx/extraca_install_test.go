package sbx

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExtraCAInstallArgs(t *testing.T) {
	pem := []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	got := ExtraCAInstallArgs("acp-s", pem)
	want := []string{"exec", "acp-s", "--", "sh", "-c", ExtraCAScript, "sh", base64.StdEncoding.EncodeToString(pem)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtraCAInstallArgs = %q, want %q", got, want)
	}
	if !strings.Contains(ExtraCAScript, ExtraCAPath) {
		t.Errorf("ExtraCAScript does not default to %s", ExtraCAPath)
	}
}

func TestEnsureExtraCA(t *testing.T) {
	ctx := context.Background()
	var calls [][]string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("workingman-extra-ca: already installed\n"), nil
	}

	// Off: no call at all.
	if msg, err := EnsureExtraCA(ctx, run, "sbx", "acp-s", ExtraCA{}); err != nil || msg != "" || len(calls) != 0 {
		t.Fatalf("off: msg %q, err %v, calls %v; want no call", msg, err, calls)
	}

	ca := ExtraCA{Path: "/home/me/ca.pem", PEM: []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")}
	msg, err := EnsureExtraCA(ctx, run, "/bin/sbx", "acp-s", ca)
	if err != nil || msg != "workingman-extra-ca: already installed" {
		t.Fatalf("on: msg %q, err %v", msg, err)
	}
	want := append([]string{"/bin/sbx"}, ExtraCAInstallArgs("acp-s", ca.PEM)...)
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Errorf("calls = %q, want [%q]", calls, want)
	}

	fail := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("sudo: a password is required\n"), errors.New("exit status 1")
	}
	_, err = EnsureExtraCA(ctx, fail, "sbx", "acp-s", ca)
	if err == nil || !strings.Contains(err.Error(), `install extra CA (/home/me/ca.pem) into sandbox "acp-s"`) ||
		!strings.Contains(err.Error(), "sudo: a password is required") {
		t.Errorf("err = %v, want the path, sandbox and sbx output", err)
	}
}

// ExtraCAScript, run by /bin/sh against a temp dst and bundle (with fake
// sudo and update-ca-certificates on PATH), installs, is idempotent,
// reinstalls into a reset bundle, and rejects non-PEM input.
func TestExtraCAScript(t *testing.T) {
	if _, err := exec.LookPath("base64"); err != nil {
		t.Skip("no base64")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "workingman-extra-ca.crt")
	bundle := filepath.Join(dir, "ca-certificates.crt")
	if err := os.WriteFile(bundle, []byte("system roots\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	stubs := map[string]string{
		"sudo": "#!/bin/sh\n[ \"$1\" = -n ] && shift\nexec \"$@\"\n",
		// Rebuilds the bundle from the system roots plus dst, as the real one does.
		"update-ca-certificates": "#!/bin/sh\n{ echo 'system roots'; cat '" + dst + "'; } >'" + bundle + "'\necho updated\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	der := testCert(t, "Script Root", caNow.AddDate(-1, 0, 0), caNow.AddDate(4, 0, 0), true)
	pemBytes := pemCert(der)

	script := func(input []byte) (string, error) {
		cmd := exec.Command("/bin/sh", "-c", ExtraCAScript, "sh", base64.StdEncoding.EncodeToString(input))
		cmd.Env = append(os.Environ(),
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"WORKINGMAN_EXTRA_CA_DST="+dst,
			"WORKINGMAN_EXTRA_CA_BUNDLE="+bundle)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := script(pemBytes)
	if err != nil || !strings.Contains(out, "workingman-extra-ca: installed "+dst) {
		t.Fatalf("first run: %v: %s", err, out)
	}
	if got, _ := os.ReadFile(dst); string(got) != string(pemBytes) {
		t.Errorf("dst = %q, want the PEM", got)
	}
	if got, _ := os.ReadFile(bundle); !strings.Contains(string(got), string(pemBytes)) {
		t.Errorf("bundle does not hold the CA: %q", got)
	}

	if out, err := script(pemBytes); err != nil || strings.TrimSpace(out) != "workingman-extra-ca: already installed" {
		t.Errorf("second run: %v: %q, want already installed", err, out)
	}

	// A bundle regenerated without it (dst survived) is repaired.
	if err := os.WriteFile(bundle, []byte("system roots\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := script(pemBytes); err != nil || !strings.Contains(out, "installed "+dst) {
		t.Errorf("reset bundle: %v: %s", err, out)
	}

	out, err = script([]byte("not a certificate\n"))
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(out, "no PEM certificate in input") {
		t.Errorf("bad input: %v: %s, want exit 2", err, out)
	}
}

// sbx-create.sh carries the same install script, byte for byte, between its
// WORKINGMAN_EXTRA_CA_EOF markers.
func TestSbxCreateScriptMatchesExtraCAScript(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "sbx-create.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const open, end = "<<'WORKINGMAN_EXTRA_CA_EOF'\n", "\nWORKINGMAN_EXTRA_CA_EOF\n"
	s := string(data)
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("sbx-create.sh has no %q heredoc", strings.TrimSpace(open))
	}
	body := s[i+len(open):]
	j := strings.Index(body, end)
	if j < 0 {
		t.Fatal("sbx-create.sh: unterminated WORKINGMAN_EXTRA_CA_EOF heredoc")
	}
	if got := body[:j+1]; got != ExtraCAScript {
		t.Errorf("sbx-create.sh heredoc differs from sbx.ExtraCAScript:\n--- sbx-create.sh\n%s--- ExtraCAScript\n%s", got, ExtraCAScript)
	}
}
