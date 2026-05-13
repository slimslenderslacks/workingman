package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
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
			return m.Path(branch)
		}
		return "", fmt.Errorf("wsp new %s: %s", branch, errMsg)
	}
	return path, nil
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
