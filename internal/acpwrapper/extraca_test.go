package acpwrapper

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/session"
)

func writeTestCA(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Root"},
		NotBefore:             notAfter.AddDate(-2, 0, 0),
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNormalizeExtraCACerts(t *testing.T) {
	base := func(ca string) Config {
		return Config{SessionID: "s", KitPath: "/kits/acp-kit", SessionsRoot: "/tmp/sessions", Workspaces: []string{"/repo"}, ExtraCACerts: ca}
	}
	for _, ca := range []string{"", "  "} {
		c := base(ca)
		if err := c.normalize(); err != nil {
			t.Fatal(err)
		}
		if c.ExtraCACerts != "" {
			t.Errorf("ExtraCACerts %q normalized to %q, want empty", ca, c.ExtraCACerts)
		}
	}
	dir := t.TempDir()
	t.Chdir(dir)
	c := base("ca.pem")
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "ca.pem"); c.ExtraCACerts != want {
		t.Errorf("ExtraCACerts = %q, want %q", c.ExtraCACerts, want)
	}
}

func TestLoadExtraCAPerLaunch(t *testing.T) {
	c := Config{SessionID: "s"}
	var log strings.Builder
	if err := c.loadExtraCA(&log); err != nil || log.Len() != 0 || len(c.extraCA.PEM) != 0 {
		t.Fatalf("unset: err=%v log=%q ca=%+v, want a silent no-op", err, log.String(), c.extraCA)
	}

	c.ExtraCACerts = writeTestCA(t, time.Now().AddDate(-1, 0, 0))
	if err := c.loadExtraCA(&log); err != nil {
		t.Fatalf("expired CA must only warn: %v", err)
	}
	if !strings.Contains(log.String(), "EXPIRED") || !strings.Contains(log.String(), "CN=Test Root") || len(c.extraCA.Certs) != 1 {
		t.Errorf("log = %q, ca = %+v", log.String(), c.extraCA)
	}
}

// An extra CA file that has become invalid since orch validated it fails the
// launch — recorded in session.json — before any sbx command runs.
func TestRunFailsOnInvalidExtraCACerts(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(bad, []byte("not a cert\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := Config{
		SessionID:     "task-x",
		SessionsRoot:  root,
		KitPath:       "/kits/acp-kit",
		SbxPath:       "/nonexistent/sbx", // must never be reached
		Workspaces:    []string{t.TempDir()},
		ExitWhenEmpty: true,
		ExtraCACerts:  bad,
	}
	err := Run(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "no PEM CERTIFICATE block") {
		t.Fatalf("Run err = %v, want the extra CA validation failure", err)
	}
	rec, rerr := (session.Store{Root: root}).Read("task-x")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rec.Status != session.StatusFailed || !strings.Contains(rec.Error, bad) {
		t.Errorf("session.json = %+v, want failed naming %s", rec, bad)
	}
}

// extraCAConfig is a Config whose extra CA has been loaded, as Run does.
func extraCAConfig(t *testing.T, c Config) Config {
	t.Helper()
	c.ExtraCACerts = writeTestCA(t, time.Now().AddDate(1, 0, 0))
	if err := c.loadExtraCA(io.Discard); err != nil {
		t.Fatal(err)
	}
	return c
}

// With the extra CA configured, every ensureSandbox path — create, repair
// (rm --force + create) and reuse — ends with exactly the install exec, after
// the policies; with it unset, the calls are the same minus that exec.
func TestEnsureSandboxInstallsExtraCA(t *testing.T) {
	rule := policy.Rule{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: "api.github.com"}
	base := Config{SessionID: "s", SandboxName: "acp-s", KitPath: "/kits/acp", SbxPath: "sbx",
		Workspaces: []string{"/repo"}, Policies: []policy.Rule{rule}}
	for _, tc := range []struct {
		name, ls string
		want     [][]string // sbx calls with the setting off
	}{
		{"create", `{"sandboxes":[]}`, [][]string{
			{"sbx", "ls", "--json"},
			{"sbx", "create", "claude", "--name", "acp-s", "--kit", "/kits/acp", "/repo"},
			{"sbx", "policy", "allow", "network", "--sandbox", "acp-s", "api.github.com"},
		}},
		{"repair", `{"sandboxes":[{"name":"acp-s","workspaces":["/old"]}]}`, [][]string{
			{"sbx", "ls", "--json"},
			{"sbx", "rm", "--force", "acp-s"},
			{"sbx", "create", "claude", "--name", "acp-s", "--kit", "/kits/acp", "/repo"},
			{"sbx", "policy", "allow", "network", "--sandbox", "acp-s", "api.github.com"},
		}},
		{"reuse", `{"sandboxes":[{"name":"acp-s","workspaces":["/repo"]}]}`, [][]string{
			{"sbx", "ls", "--json"},
			{"sbx", "policy", "allow", "network", "--sandbox", "acp-s", "api.github.com"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			off := &fakeSbx{lsOutput: tc.ls}
			if _, err := ensureSandbox(context.Background(), off.run, base); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(off.calls, tc.want) {
				t.Errorf("off: calls =\n%q\nwant\n%q", off.calls, tc.want)
			}

			c := extraCAConfig(t, base)
			on := &fakeSbx{lsOutput: tc.ls}
			if _, err := ensureSandbox(context.Background(), on.run, c); err != nil {
				t.Fatal(err)
			}
			want := append(slices.Clone(tc.want), append([]string{"sbx"}, sbx.ExtraCAInstallArgs("acp-s", c.extraCA.PEM)...))
			if !reflect.DeepEqual(on.calls, want) {
				t.Errorf("on: calls =\n%q\nwant\n%q", on.calls, want)
			}
		})
	}
}

// A failed install on a new sandbox fails the launch and DELETEs the gateway
// minted for it; on a reused sandbox it fails the launch and keeps the
// sandbox (no rm).
func TestEnsureSandboxExtraCAFailure(t *testing.T) {
	g := newFakeGateway(t)
	c := extraCAConfig(t, gatewayConfig(t, g, "gmail"))
	f := &fakeSbx{lsOutput: `{"sandboxes":[]}`, failCmd: "exec"}
	_, err := ensureSandbox(context.Background(), f.run, c)
	if err == nil || !strings.Contains(err.Error(), `install extra CA (`+c.ExtraCACerts+`) into sandbox "acp-s"`) {
		t.Fatalf("create: err = %v, want the install failure", err)
	}
	if !reflect.DeepEqual(g.deleted, []string{"gw-1"}) {
		t.Errorf("create: deleted = %v, want [gw-1]", g.deleted)
	}
	if _, ok, _ := c.MCPGateway.Store.Load("acp-s"); ok {
		t.Error("create: gateway record left behind")
	}

	reuse := &fakeSbx{lsOutput: `{"sandboxes":[{"name":"acp-s","workspaces":["/repo"]}]}`, failCmd: "exec"}
	if _, err := ensureSandbox(context.Background(), reuse.run, c); err == nil || !strings.Contains(err.Error(), "install extra CA") {
		t.Fatalf("reuse: err = %v, want the install failure", err)
	}
	for _, call := range reuse.calls {
		if call[1] == "rm" || call[1] == "create" {
			t.Errorf("reuse: unexpected %v", call)
		}
	}
}
