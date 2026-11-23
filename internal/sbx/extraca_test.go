package sbx

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
)

var caNow = time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)

// testCert returns a self-signed certificate's DER.
func testCert(t *testing.T, cn string, notBefore, notAfter time.Time, isCA bool) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Test"}},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  isCA,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadExtraCA(t *testing.T) {
	valid := testCert(t, "Valid Root", caNow.AddDate(-1, 0, 0), caNow.AddDate(4, 0, 0), true)
	expired := testCert(t, "Expired Root", caNow.AddDate(-3, 0, 0), caNow.AddDate(-1, 0, 0), true)
	future := testCert(t, "Future Root", caNow.AddDate(1, 0, 0), caNow.AddDate(5, 0, 0), true)
	leaf := testCert(t, "github.com", caNow.AddDate(-1, 0, 0), caNow.AddDate(1, 0, 0), false)
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not really a key")})

	cases := []struct {
		name      string
		data      []byte
		wantErr   string
		wantCerts int
		wantWarn  []string // substrings, one warning each, in order
	}{
		{name: "empty", data: []byte("  \n"), wantErr: "file is empty"},
		{name: "garbage", data: []byte("hello\n"), wantErr: "no PEM CERTIFICATE block"},
		{name: "der", data: expired, wantErr: "looks like DER; convert with: openssl x509 -inform DER"},
		{name: "bad cert block", data: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")}), wantErr: "certificate #1"},
		{name: "valid", data: pemCert(valid), wantCerts: 1},
		{name: "expired only", data: pemCert(expired), wantCerts: 1, wantWarn: []string{
			`"CN=Expired Root,O=Test" EXPIRED (notAfter 2025-11-23T00:00:00Z)`,
			"ERROR: ",
		}},
		{name: "expired plus valid", data: append(pemCert(expired), pemCert(valid)...), wantCerts: 2, wantWarn: []string{
			"EXPIRED",
		}},
		{name: "not yet valid", data: pemCert(future), wantCerts: 1, wantWarn: []string{"not valid yet"}},
		{name: "leaf", data: pemCert(leaf), wantCerts: 1, wantWarn: []string{"not a CA certificate"}},
		{name: "key dropped", data: append(append([]byte("comment\n"), key...), pemCert(valid)...), wantCerts: 1, wantWarn: []string{
			`non-certificate PEM block(s) of type "PRIVATE KEY"`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, "ca.pem", tc.data)
			ca, err := loadExtraCA(path, caNow)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ca.Path != path || len(ca.Certs) != tc.wantCerts {
				t.Fatalf("path=%q certs=%d, want %q %d", ca.Path, len(ca.Certs), path, tc.wantCerts)
			}
			if len(ca.Warnings) != len(tc.wantWarn) {
				t.Fatalf("warnings = %q, want %d", ca.Warnings, len(tc.wantWarn))
			}
			for i, w := range tc.wantWarn {
				if !strings.Contains(ca.Warnings[i], w) {
					t.Errorf("warning[%d] = %q, want it to contain %q", i, ca.Warnings[i], w)
				}
			}
			if bytes.Contains(ca.PEM, []byte("PRIVATE KEY")) || bytes.Count(ca.PEM, []byte("-----BEGIN CERTIFICATE-----")) != tc.wantCerts {
				t.Errorf("normalized PEM = %s", ca.PEM)
			}
		})
	}
}

func TestLoadExtraCAMissingAndRelative(t *testing.T) {
	if _, err := LoadExtraCA(filepath.Join(t.TempDir(), "nope.pem")); err == nil {
		t.Fatal("missing file: want an error")
	}
	if _, err := LoadExtraCA(""); err == nil {
		t.Fatal("empty path: want an error")
	}
	dir := t.TempDir()
	der := testCert(t, "Rel Root", caNow, time.Now().AddDate(1, 0, 0), true)
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), pemCert(der), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	ca, err := LoadExtraCA("ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	if got := ca.Subjects(); !filepath.IsAbs(ca.Path) || len(got) != 1 || got[0] != "CN=Rel Root,O=Test" {
		t.Fatalf("path=%q subjects=%q", ca.Path, ca.Subjects())
	}
}
