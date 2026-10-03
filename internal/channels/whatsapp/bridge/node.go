package bridge

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MinNodeMajor is the oldest Node.js the bridge runs on (Baileys needs 18+).
const MinNodeMajor = 18

// ErrNodeUnavailable is wrapped by FindNode's errors, so callers can tell
// "WhatsApp bridge mode cannot run on this machine" from other failures.
var ErrNodeUnavailable = errors.New("node.js >= 18 is required for the WhatsApp bridge")

var nodeVersionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// ParseNodeVersion parses "v22.22.1" style output of `node --version`.
func ParseNodeVersion(s string) (major, minor, patch int, err error) {
	m := nodeVersionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, 0, fmt.Errorf("unrecognised node version %q", strings.TrimSpace(s))
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	patch, _ = strconv.Atoi(m[3])
	return major, minor, patch, nil
}

// FindNode resolves the node binary (configured, or "node" on PATH) and checks
// its version. The error says exactly what is wrong and how to fix it.
func FindNode(configured string) (string, error) {
	name := configured
	if name == "" {
		name = "node"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %q was not found on PATH; install Node.js 18 or newer (https://nodejs.org) or set options.bridge.node in channels.yaml: %v", ErrNodeUnavailable, name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%w: running `%s --version` failed: %v", ErrNodeUnavailable, path, err)
	}
	major, _, _, err := ParseNodeVersion(string(out))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNodeUnavailable, err)
	}
	if major < MinNodeMajor {
		return "", fmt.Errorf("%w: found %s at %s; upgrade Node.js", ErrNodeUnavailable, strings.TrimSpace(string(out)), path)
	}
	return path, nil
}

// FindNPM locates npm next to node, falling back to PATH.
func FindNPM(nodePath string) (string, error) {
	if nodePath != "" {
		sibling := filepath.Join(filepath.Dir(nodePath), "npm")
		if p, err := exec.LookPath(sibling); err == nil {
			return p, nil
		}
	}
	p, err := exec.LookPath("npm")
	if err != nil {
		return "", fmt.Errorf("%w: npm was not found (it ships with Node.js): %v", ErrNodeUnavailable, err)
	}
	return p, nil
}
