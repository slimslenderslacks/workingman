package sbx

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
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

// ExtraCAPath is where the extra CA bundle goes inside a sandbox, beside the
// proxy-ca.crt sbx itself installs there at create time.
const ExtraCAPath = "/usr/local/share/ca-certificates/workingman-extra-ca.crt"

// ExtraCAScript installs the base64-encoded PEM bundle in $1 into the
// sandbox's system trust store (ExtraCAPath + update-ca-certificates), which
// curl, git, node and Python all read (docs/sandbox-custom-ca.md §2). It runs
// as root, or through `sudo -n` when `sbx exec` runs as the agent user. It is
// idempotent: when the file is unchanged and the bundle still holds it, it
// exits 0 with "already installed". Input without a PEM certificate exits 2.
// WORKINGMAN_EXTRA_CA_DST / WORKINGMAN_EXTRA_CA_BUNDLE override the paths for
// tests only.
//
// sbx-create.sh carries a byte-identical copy between its
// WORKINGMAN_EXTRA_CA_EOF markers; a test keeps the two in sync.
const ExtraCAScript = `set -eu
dst=${WORKINGMAN_EXTRA_CA_DST:-/usr/local/share/ca-certificates/workingman-extra-ca.crt}
bundle=${WORKINGMAN_EXTRA_CA_BUNDLE:-/etc/ssl/certs/ca-certificates.crt}
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
printf '%s' "$1" | base64 -d >"$tmp"
grep -q -- '-----BEGIN CERTIFICATE-----' "$tmp" || { echo "workingman-extra-ca: no PEM certificate in input" >&2; exit 2; }
if [ "$(id -u)" = 0 ]; then as_root=; else as_root="sudo -n"; fi
first=$(sed -n 2p "$tmp")
if cmp -s "$tmp" "$dst" && grep -qxF "$first" "$bundle"; then
  echo "workingman-extra-ca: already installed" >&2; exit 0
fi
$as_root install -m 0644 "$tmp" "$dst"
$as_root update-ca-certificates >/dev/null
grep -qxF "$first" "$bundle" || { echo "workingman-extra-ca: $bundle does not contain the extra CA after update-ca-certificates" >&2; exit 1; }
echo "workingman-extra-ca: installed $dst" >&2
`

// ExtraCAInstallArgs is the argv (after the sbx binary) that installs pem
// into sandbox's system trust store: `exec <sandbox> -- sh -c
// <ExtraCAScript> sh <base64 pem>`. The PEM travels as an argv word, so the
// stdin-less RunFunc seams can carry it.
func ExtraCAInstallArgs(sandbox string, pem []byte) []string {
	return []string{"exec", sandbox, "--", "sh", "-c", ExtraCAScript, "sh",
		base64.StdEncoding.EncodeToString(pem)}
}

// EnsureExtraCA installs ca into sandbox with ExtraCAInstallArgs through run,
// on every launch: the script is a no-op when the CA is already there, so a
// reused, repaired or pre-config sandbox converges too. It returns the
// script's diagnostic ("installed ..." / "already installed"). It does
// nothing and returns "" when ca holds no PEM (the setting is off). A
// non-zero exit is an error naming the bundle, the sandbox and sbx's output.
func EnsureExtraCA(ctx context.Context, run RunFunc, sbxPath, sandbox string, ca ExtraCA) (string, error) {
	if len(ca.PEM) == 0 {
		return "", nil
	}
	out, err := run(ctx, sbxPath, ExtraCAInstallArgs(sandbox, ca.PEM)...)
	msg := strings.TrimSpace(string(out))
	if err != nil {
		return msg, fmt.Errorf("install extra CA (%s) into sandbox %q: %w: %s", ca.Path, sandbox, err, msg)
	}
	return msg, nil
}
