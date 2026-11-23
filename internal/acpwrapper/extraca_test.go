package acpwrapper

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
