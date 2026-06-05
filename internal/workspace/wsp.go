package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// WspManager is the production Manager: it shells out to the `wsp` CLI to
// provision multi-repo workspaces under (by default) ~/dev/workspaces/<branch>/.
// Each method invokes wsp with --json so the orchestrator can read structured
// output rather than scrape free-form text.
//
// Two wsp quirks the wrapper smooths over:
//
//   - `wsp new` exits 0 even when the workspace already exists; the JSON
//     payload carries an `error` field. Create treats "already exists" as
//     a soft signal and resolves the path via Path() instead.
//   - `wsp rm` exits 1 if the workspace is missing. Remove swallows that
//     case so it is idempotent.
type WspManager struct {
	// Binary is the wsp executable. Defaults to "wsp" on PATH.
	Binary string
}

func NewWsp() *WspManager { return &WspManager{} }

func (m *WspManager) binary() string {
	if m.Binary != "" {
		return m.Binary
	}
	return "wsp"
}

func (m *WspManager) Create(ctx context.Context, branch string, repos []Repo) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("workspace: branch is required")
	}
	// Bootstrap: a task or commit agent's workspace cannot be created
	// until its repos are present in wsp's global registry. Auto-register
	// any that aren't already there before calling `wsp new`.
	if err := m.ensureRegistered(ctx, repos); err != nil {
		return "", err
	}
	args := []string{"new", "--json"}
	if len(repos) == 0 {
		args = append(args, "--empty")
	}
	args = append(args, branch)
	for _, r := range repos {
		args = append(args, r.Ref())
	}

	// `wsp new` exits non-zero on errors like "already exists" but still
	// emits the structured error to stdout. Parse the JSON first; only treat
	// a non-zero exit as fatal if we have no usable JSON to act on.
	out, runErr := exec.CommandContext(ctx, m.binary(), args...).Output()
	path, errMsg, parseErr := parseNewResult(out)
	if parseErr != nil {
		if runErr != nil {
			return "", wspExecError("wsp new", runErr)
		}
		return "", parseErr
	}
	if errMsg != "" {
		if isAlreadyExists(errMsg) {
			// Skip base-branch application on re-resolve: the workspace
			// already exists with whatever commits the agent has made on
			// the feature branch, so we must not reset its HEAD.
			return m.Path(branch)
		}
		return "", fmt.Errorf("wsp new %s: %s", branch, errMsg)
	}
	if err := applyBaseBranches(ctx, path, branch, repos); err != nil {
		return "", fmt.Errorf("wsp new %s: base branch setup: %w", branch, err)
	}
	return path, nil
}

// applyBaseBranches resets each repo's feature branch HEAD to its declared
// base branch. Called only after a fresh `wsp new` succeeds — the freshly
// created branch has no commits yet, so `git checkout -B` is a safe reset.
//
// For each repo with BaseBranch set:
//
//	cd <workspace>/<repo-dir>
//	git fetch origin <base_branch>
//	git checkout -B <branch> origin/<base_branch>
//
// Repos with BaseBranch empty are left as wsp configured them (default
// branch or remote-tracking the feature branch if it already exists).
func applyBaseBranches(ctx context.Context, workspaceDir, branch string, repos []Repo) error {
	for _, r := range repos {
		if r.BaseBranch == "" {
			continue
		}
		repoDir := filepath.Join(workspaceDir, r.DirName())
		fetch := exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "origin", r.BaseBranch)
		if out, err := fetch.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: git fetch origin %s: %w: %s",
				r.DirName(), r.BaseBranch, err, strings.TrimSpace(string(out)))
		}
		checkout := exec.CommandContext(ctx, "git", "-C", repoDir,
			"checkout", "-B", branch, "origin/"+r.BaseBranch)
		if out, err := checkout.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: git checkout -B %s origin/%s: %w: %s",
				r.DirName(), branch, r.BaseBranch, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (m *WspManager) Path(branch string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("workspace: branch is required")
	}
	out, err := exec.Command(m.binary(), "ls", "--json").Output()
	if err != nil {
		return "", wspExecError("wsp ls", err)
	}
	entries, parseErr := parseLsResult(out)
	if parseErr != nil {
		return "", parseErr
	}
	for _, e := range entries {
		if e.Name == branch {
			return e.Path, nil
		}
	}
	return "", fmt.Errorf("wsp ls: workspace %q not found", branch)
}

func (m *WspManager) Remove(ctx context.Context, branch string) error {
	if branch == "" {
		return fmt.Errorf("workspace: branch is required")
	}
	cmd := exec.CommandContext(ctx, m.binary(), "rm", "--json", branch)
	out, runErr := cmd.Output()
	// wsp rm of a missing workspace exits 1 but still writes JSON. Parse first.
	ok, errMsg, parseErr := parseRmResult(out)
	if parseErr != nil {
		if runErr != nil {
			return wspExecError("wsp rm", runErr)
		}
		return parseErr
	}
	if ok {
		return nil
	}
	if isNotFound(errMsg) {
		return nil
	}
	return fmt.Errorf("wsp rm %s: %s", branch, errMsg)
}

// ensureRegistered makes sure every repo in repos is present in wsp's
// global registry. Missing repos are added via `wsp registry add` with an
// SSH URL derived from their identity. Repos given by shortname only are
// trusted to be resolvable by wsp itself.
func (m *WspManager) ensureRegistered(ctx context.Context, repos []Repo) error {
	if len(repos) == 0 {
		return nil
	}
	var needIdent bool
	for _, r := range repos {
		if r.Identity != "" {
			needIdent = true
			break
		}
	}
	if !needIdent {
		return nil
	}
	known, err := m.registeredIdentities(ctx)
	if err != nil {
		return err
	}
	for _, r := range repos {
		if r.Identity == "" {
			continue
		}
		if _, ok := known[r.Identity]; ok {
			continue
		}
		url := sshURLFromIdentity(r.Identity)
		if url == "" {
			return fmt.Errorf("wsp: cannot derive URL from identity %q", r.Identity)
		}
		if err := m.registryAdd(ctx, url); err != nil {
			return err
		}
	}
	return nil
}

func (m *WspManager) registeredIdentities(ctx context.Context) (map[string]struct{}, error) {
	out, runErr := exec.CommandContext(ctx, m.binary(), "registry", "ls", "--json").Output()
	entries, parseErr := parseRegistryListResult(out)
	if parseErr != nil {
		if runErr != nil {
			return nil, wspExecError("wsp registry ls", runErr)
		}
		return nil, parseErr
	}
	known := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		known[e.Identity] = struct{}{}
	}
	return known, nil
}

func (m *WspManager) registryAdd(ctx context.Context, url string) error {
	out, runErr := exec.CommandContext(ctx, m.binary(), "registry", "add", "--json", url).Output()
	ok, errMsg, parseErr := parseRegistryAddResult(out)
	if parseErr != nil {
		if runErr != nil {
			return wspExecError("wsp registry add", runErr)
		}
		return parseErr
	}
	if ok {
		return nil
	}
	if errMsg == "" && runErr != nil {
		return wspExecError("wsp registry add", runErr)
	}
	return fmt.Errorf("wsp registry add %s: %s", url, errMsg)
}

// sshURLFromIdentity converts a wsp identity ("github.com/<org>/<name>") to
// the SSH clone URL wsp's registry-add accepts. Works for any provider that
// uses the standard git@host:org/repo.git format (github, gitlab, bitbucket).
// Returns "" for identities that don't have a host/org/name shape.
func sshURLFromIdentity(identity string) string {
	first := strings.Index(identity, "/")
	if first <= 0 || first == len(identity)-1 {
		return ""
	}
	host := identity[:first]
	rest := identity[first+1:]
	if !strings.Contains(rest, "/") {
		return ""
	}
	return "git@" + host + ":" + rest + ".git"
}

// wspExecError unwraps exec.ExitError so the message includes wsp's stderr.
func wspExecError(prefix string, err error) error {
	if ee, ok := err.(*exec.ExitError); ok {
		return fmt.Errorf("%s: %w: %s", prefix, err, strings.TrimSpace(string(ee.Stderr)))
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

func isAlreadyExists(msg string) bool { return strings.Contains(msg, "already exists") }

// "opening /…/.wsp.yaml" is wsp's specific not-found signature for `rm`.
func isNotFound(msg string) bool { return strings.Contains(msg, "opening") }

// ---- JSON shapes ----

type wspNewResult struct {
	OK    bool   `json:"ok"`
	Path  string `json:"path"`
	Error string `json:"error"`
}

type wspListResult struct {
	Workspaces []wspListEntry `json:"workspaces"`
}

type wspListEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type wspRmResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

type wspRegistryListResult struct {
	Repos []wspRegistryEntry `json:"repos"`
}

type wspRegistryEntry struct {
	Identity  string `json:"identity"`
	Shortname string `json:"shortname"`
	URL       string `json:"url"`
}

type wspRegistryAddResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// parseNewResult returns (path, errMsg, parseErr).
//
// `wsp new --json` prints a human "Creating workspace…" line to stdout before
// the JSON body, so we scan for the first '{' and decode from there.
func parseNewResult(out []byte) (string, string, error) {
	body := jsonBody(out)
	if body == nil {
		return "", "", fmt.Errorf("wsp new: no JSON in output: %s", strings.TrimSpace(string(out)))
	}
	var r wspNewResult
	if err := json.Unmarshal(body, &r); err != nil {
		return "", "", fmt.Errorf("wsp new: parse JSON: %w: %s", err, body)
	}
	return r.Path, r.Error, nil
}

func parseLsResult(out []byte) ([]wspListEntry, error) {
	body := jsonBody(out)
	if body == nil {
		return nil, fmt.Errorf("wsp ls: no JSON in output: %s", strings.TrimSpace(string(out)))
	}
	var r wspListResult
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("wsp ls: parse JSON: %w", err)
	}
	return r.Workspaces, nil
}

// parseRegistryListResult extracts the {identity, shortname, url} entries
// from `wsp registry ls --json` output.
func parseRegistryListResult(out []byte) ([]wspRegistryEntry, error) {
	body := jsonBody(out)
	if body == nil {
		return nil, fmt.Errorf("wsp registry ls: no JSON in output: %s", strings.TrimSpace(string(out)))
	}
	var r wspRegistryListResult
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("wsp registry ls: parse JSON: %w", err)
	}
	return r.Repos, nil
}

// parseRegistryAddResult returns (ok, errMsg, parseErr) — same shape as the
// other `{ok, error}` wsp responses.
func parseRegistryAddResult(out []byte) (bool, string, error) {
	body := jsonBody(out)
	if body == nil {
		return false, "", fmt.Errorf("wsp registry add: no JSON in output")
	}
	var r wspRegistryAddResult
	if err := json.Unmarshal(body, &r); err != nil {
		return false, "", fmt.Errorf("wsp registry add: parse JSON: %w", err)
	}
	return r.OK, r.Error, nil
}

// parseRmResult returns (ok, errMsg, parseErr).
func parseRmResult(out []byte) (bool, string, error) {
	body := jsonBody(out)
	if body == nil {
		// Empty stdout is plausible when wsp ran but produced nothing on the
		// captured stream; let the caller fall back to exec error.
		return false, "", fmt.Errorf("wsp rm: no JSON in output")
	}
	var r wspRmResult
	if err := json.Unmarshal(body, &r); err != nil {
		return false, "", fmt.Errorf("wsp rm: parse JSON: %w", err)
	}
	return r.OK, r.Error, nil
}

// jsonBody returns the slice of out starting at the first '{' character, or
// nil if there isn't one. wsp prefixes some commands with a human progress
// line before emitting the JSON body.
func jsonBody(out []byte) []byte {
	for i, b := range out {
		if b == '{' {
			return out[i:]
		}
	}
	return nil
}
