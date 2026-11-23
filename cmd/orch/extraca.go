package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/sbx"
)

// loadExtraCACerts resolves and validates --extra-ca-certs (or
// $WORKINGMAN_EXTRA_CA_CERTS) at startup and returns the absolute path every
// sandbox-creating path is given. Empty means the setting is off: it returns
// "" and logs nothing, so the daemon behaves exactly as without it. A leading
// "~/" is expanded, because a value passed through launchd's ProgramArguments
// never sees a shell. Warnings (expired, not-yet-valid or non-CA certs) go to
// w and the audit log; they are not fatal.
func loadExtraCACerts(path string, w io.Writer, a *audit.Logger) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand %q: %w", path, err)
		}
		path = filepath.Join(home, rest)
	}
	ca, err := sbx.LoadExtraCA(path)
	if err != nil {
		return "", err
	}
	for _, warn := range ca.Warnings {
		fmt.Fprintln(w, "orch: warning:", warn)
		a.Log("extra_ca_warning", "path", ca.Path, "detail", warn)
	}
	a.Log("extra_ca_config", "path", ca.Path, "certs", strconv.Itoa(len(ca.Certs)), "subjects", strings.Join(ca.Subjects(), "; "))
	return ca.Path, nil
}
