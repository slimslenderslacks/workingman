package sbx

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ExtraCAEnv is the environment variable `orch --extra-ca-certs` falls back
// to: the per-laptop PEM bundle of extra CAs (a TLS-inspecting VPN's root)
// that sandboxes must trust. Unset or empty means no extra CA.
const ExtraCAEnv = "WORKINGMAN_EXTRA_CA_CERTS"

// ExtraCA is a validated extra-CA bundle, as loaded by [LoadExtraCA].
type ExtraCA struct {
	// Path is the absolute path the bundle was read from.
	Path string
	// PEM is the normalized bundle: only its CERTIFICATE blocks, re-encoded,
	// so the bytes installed into a sandbox are stable across reloads.
	PEM []byte
	// Certs are the parsed certificates, in file order.
	Certs []*x509.Certificate
	// Warnings are non-fatal problems (expired, not yet valid, not a CA,
	// non-certificate blocks dropped). When every cert has expired, one of
	// them starts with "ERROR: ".
	Warnings []string
}

// Subjects returns the certificates' subjects, in file order.
func (e ExtraCA) Subjects() []string {
	out := make([]string, len(e.Certs))
	for i, c := range e.Certs {
		out[i] = c.Subject.String()
	}
	return out
}

// LoadExtraCA reads and validates the extra-CA bundle at path, relative to
// the current directory when not absolute (docs/sandbox-custom-ca.md §4.4).
// It fails when the file is unreadable or empty, holds no PEM CERTIFICATE
// block (with a conversion hint when it is DER), or holds a CERTIFICATE block
// that does not parse. Expired, not-yet-valid and non-CA certificates are
// reported as warnings only: the launch proceeds and the in-sandbox TLS
// preflight is what fails.
func LoadExtraCA(path string) (ExtraCA, error) {
	return loadExtraCA(path, time.Now())
}

func loadExtraCA(path string, now time.Time) (ExtraCA, error) {
	if strings.TrimSpace(path) == "" {
		return ExtraCA{}, errors.New("extra CA certs: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ExtraCA{}, fmt.Errorf("extra CA certs %q: %w", path, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return ExtraCA{}, fmt.Errorf("extra CA certs: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return ExtraCA{}, fmt.Errorf("extra CA certs %s: file is empty", abs)
	}

	ca := ExtraCA{Path: abs}
	var out bytes.Buffer
	dropped := map[string]int{}
	var droppedOrder []string
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			if dropped[block.Type] == 0 {
				droppedOrder = append(droppedOrder, block.Type)
			}
			dropped[block.Type]++
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return ExtraCA{}, fmt.Errorf("extra CA certs %s: certificate #%d: %w", abs, len(ca.Certs)+1, err)
		}
		ca.Certs = append(ca.Certs, cert)
		_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})
	}
	if len(ca.Certs) == 0 {
		if _, err := x509.ParseCertificate(data); err == nil {
			return ExtraCA{}, fmt.Errorf("extra CA certs %s: no PEM CERTIFICATE block; the file looks like DER; convert with: openssl x509 -inform DER -in %s -out %s.pem", abs, abs, abs)
		}
		return ExtraCA{}, fmt.Errorf("extra CA certs %s: no PEM CERTIFICATE block (-----BEGIN CERTIFICATE-----) found", abs)
	}
	ca.PEM = out.Bytes()

	for _, typ := range droppedOrder {
		ca.Warnings = append(ca.Warnings, fmt.Sprintf("extra CA certs %s: ignoring %d non-certificate PEM block(s) of type %q", abs, dropped[typ], typ))
	}
	expired := 0
	for _, c := range ca.Certs {
		subj, until := c.Subject.String(), c.NotAfter.UTC().Format(time.RFC3339)
		switch {
		case now.After(c.NotAfter):
			expired++
			ca.Warnings = append(ca.Warnings, fmt.Sprintf("extra CA certs %s: certificate %q EXPIRED (notAfter %s); TLS clients will not chain to an expired anchor (docs/sandbox-custom-ca.md §1.3)", abs, subj, until))
		case now.Before(c.NotBefore):
			ca.Warnings = append(ca.Warnings, fmt.Sprintf("extra CA certs %s: certificate %q is not valid yet (notBefore %s, notAfter %s)", abs, subj, c.NotBefore.UTC().Format(time.RFC3339), until))
		}
		if !c.IsCA {
			ca.Warnings = append(ca.Warnings, fmt.Sprintf("extra CA certs %s: certificate %q (notAfter %s) is not a CA certificate; was a leaf exported by mistake?", abs, subj, until))
		}
	}
	if expired == len(ca.Certs) {
		ca.Warnings = append(ca.Warnings, fmt.Sprintf("ERROR: extra CA certs %s: every certificate has expired, so sandboxes will still reject the VPN's chain; re-export the current root (docs/sandbox-custom-ca.md §1.4)", abs))
	}
	return ca, nil
}
