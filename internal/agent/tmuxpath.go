package agent

import (
	"fmt"
	"os"
	"os/exec"
)

// ResolveTmux returns the absolute path to a usable tmux binary. It tries
// exec.LookPath first (which honours the current process PATH), then falls
// back to common install locations that don't show up when a daemon is
// launched outside a login shell: NixOS per-user profiles, the current
// system profile, Homebrew, and /usr/local.
//
// Returning an absolute path lets callers shell out to tmux reliably even
// if their PATH is the bare-bones default macOS gives GUI-launched
// processes.
func ResolveTmux() (string, error) {
	if p, err := exec.LookPath("tmux"); err == nil {
		return p, nil
	}
	for _, candidate := range tmuxCandidatePaths() {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("tmux: not found in PATH or known locations")
}

func tmuxCandidatePaths() []string {
	user := os.Getenv("USER")
	paths := []string{
		"/run/current-system/sw/bin/tmux", // NixOS system profile
		"/nix/var/nix/profiles/default/bin/tmux",
		"/opt/homebrew/bin/tmux",
		"/usr/local/bin/tmux",
		"/usr/bin/tmux",
	}
	if user != "" {
		paths = append([]string{"/etc/profiles/per-user/" + user + "/bin/tmux"}, paths...)
	}
	return paths
}

// AugmentSearchPath appends every path-segment from `extra` to the current
// PATH environment variable if it isn't already there. Useful at daemon
// startup so child processes (the agent's claude session, the tmux server,
// etc.) can find binaries installed by tools the daemon's launcher didn't
// know about.
//
// Returns the resulting PATH for caller logging or auditing.
func AugmentSearchPath(extra []string) string {
	current := os.Getenv("PATH")
	parts := splitPath(current)
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		seen[p] = struct{}{}
	}
	for _, p := range extra {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			continue
		}
		parts = append(parts, p)
		seen[p] = struct{}{}
	}
	updated := joinPath(parts)
	_ = os.Setenv("PATH", updated)
	return updated
}

// DefaultPATHCandidates returns the directories typically holding tools the
// orchestrator's child processes need on macOS — Nix profiles, Homebrew,
// the system path. Pass this to AugmentSearchPath at startup.
func DefaultPATHCandidates() []string {
	user := os.Getenv("USER")
	cands := []string{
		"/run/current-system/sw/bin",
		"/nix/var/nix/profiles/default/bin",
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	}
	if user != "" {
		cands = append([]string{"/etc/profiles/per-user/" + user + "/bin"}, cands...)
	}
	return cands
}

func splitPath(p string) []string {
	if p == "" {
		return nil
	}
	out := []string{}
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == ':' {
			if i > start {
				out = append(out, p[start:i])
			}
			start = i + 1
		}
	}
	if start < len(p) {
		out = append(out, p[start:])
	}
	return out
}

func joinPath(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ":" + p
	}
	return out
}
