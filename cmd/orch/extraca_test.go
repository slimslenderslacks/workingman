package main

import (
	"bytes"
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

	"github.com/slimslenderslacks/work/internal/audit"
)

func writeCA(t *testing.T, path string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Corp Root"},
		NotBefore:             notAfter.AddDate(-2, 0, 0),
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Unset is a silent no-op: no path, no stderr, no audit line.
func TestLoadExtraCACertsUnset(t *testing.T) {
	var stderr, log bytes.Buffer
	got, err := loadExtraCACerts("  ", &stderr, audit.New(&log))
	if err != nil || got != "" || stderr.Len() != 0 || log.Len() != 0 {
		t.Fatalf("got=%q err=%v stderr=%q audit=%q, want all empty", got, err, stderr.String(), log.String())
	}
}

func TestLoadExtraCACertsValidExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".workingman", "extra-ca.pem")
	writeCA(t, want, time.Now().AddDate(1, 0, 0))
	var stderr, log bytes.Buffer
	got, err := loadExtraCACerts("~/.workingman/extra-ca.pem", &stderr, audit.New(&log))
	if err != nil || got != want {
		t.Fatalf("got=%q err=%v, want %q", got, err, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want no warnings", stderr.String())
	}
	if l := log.String(); !strings.Contains(l, "extra_ca_config") || !strings.Contains(l, "certs=1") || !strings.Contains(l, "CN=Corp Root") {
		t.Errorf("audit = %q", l)
	}
}

// An expired cert warns (subject + notAfter) but does not fail startup.
func TestLoadExtraCACertsExpiredWarns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ca.pem")
	writeCA(t, p, time.Now().AddDate(-1, 0, 0))
	var stderr, log bytes.Buffer
	got, err := loadExtraCACerts(p, &stderr, audit.New(&log))
	if err != nil || got != p {
		t.Fatalf("got=%q err=%v, want %q", got, err, p)
	}
	if s := stderr.String(); !strings.Contains(s, "orch: warning:") || !strings.Contains(s, `"CN=Corp Root" EXPIRED (notAfter `) {
		t.Errorf("stderr = %q", s)
	}
	if !strings.Contains(log.String(), "extra_ca_warning") {
		t.Errorf("audit = %q", log.String())
	}
}

func TestLoadExtraCACertsInvalidFails(t *testing.T) {
	var stderr, log bytes.Buffer
	if _, err := loadExtraCACerts(filepath.Join(t.TempDir(), "missing.pem"), &stderr, audit.New(&log)); err == nil {
		t.Fatal("missing file: want an error")
	}
}
