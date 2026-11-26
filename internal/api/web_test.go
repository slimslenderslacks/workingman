package api

import (
	"bytes"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestStaticBundleServed(t *testing.T) {
	s, _ := newTestServer(t, &fakeSource{})
	h := s.Handler("7078")
	for _, tc := range []struct {
		path, ctype, contains string
	}{
		{"/", "text/html", `<script src="/app.js"`},
		{"/login", "text/html", `<script src="/app.js"`},
		{"/projects/p_0.demo", "text/html", `<script src="/app.js"`},
		{"/app.js", "javascript", "wm.stream"},
		{"/app.css", "text/css", "--accent"},
		{"/workload.js", "javascript", "wm.wl"},
		{"/sandboxes.js", "javascript", "sandbox card"},
		{"/agents.js", "javascript", "agents map"},
		{"/sandboxes/step-up", "text/html", `<script src="/sandboxes.js"`},
		{"/projects/p_0.demo/tasks/build", "text/html", `<script src="/agents.js"`},
		{"/transcript.js", "javascript", "wm.tx"},
		{"/watch.js", "javascript", "transcriptView"},
		{"/secondary.js", "javascript", "wm.sv"},
		{"/timeline.js", "javascript", "audit"},
		{"/pulls.js", "javascript", "pull requests"},
		{"/timeline", "text/html", `<script src="/timeline.js"`},
		{"/projects/p_0.demo/pulls", "text/html", `<script src="/pulls.js"`},
		{"/sessions/task-build", "text/html", `<script src="/watch.js"`},
	} {
		// The bundle needs no credentials.
		rec := do(t, h, "GET", tc.path, "127.0.0.1:7078", "", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", tc.path, rec.Code, rec.Body)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.ctype) {
			t.Errorf("%s Content-Type = %q, want %s", tc.path, ct, tc.ctype)
		}
		if !strings.Contains(rec.Body.String(), tc.contains) {
			t.Errorf("%s does not contain %q", tc.path, tc.contains)
		}
	}
	if rec := do(t, h, "GET", "/login", "127.0.0.1:7078", "", nil); rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("index Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	for _, p := range []string{"/missing.js", "/web/app.js", "/../app.css.map"} {
		if rec := do(t, h, "GET", p, "127.0.0.1:7078", "", nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
	// Only the embedded web/ directory is reachable.
	for _, p := range []string{"/../server.go", "/server.go", "/web.go"} {
		if rec := do(t, h, "GET", p, "127.0.0.1:7078", "", nil); strings.Contains(rec.Body.String(), "package api") {
			t.Errorf("%s served Go source", p)
		}
	}
	if rec := do(t, h, "POST", "/", "127.0.0.1:7078", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: %d, want 405", rec.Code)
	}
	// The Host allowlist covers the bundle too: a rebinding page cannot
	// load the login page.
	if rec := do(t, h, "GET", "/login", "evil.example:7078", "", nil); rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("GET /login on a foreign Host: %d, want 421", rec.Code)
	}
	// /api/ is never the bundle.
	if rec := do(t, h, "GET", "/api/nope", "127.0.0.1:7078", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/nope: %d, want 401", rec.Code)
	}
}

// The bundle must load nothing from another origin and run no inline
// script, or the CSP would block it (and a third-party asset would be a
// supply-chain hole).
func TestStaticBundleIsSelfContained(t *testing.T) {
	external := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?\s*(https?:)?//|@import|url\(\s*["']?(https?:)?//|https?://[a-z]`)
	inline := regexp.MustCompile(`(?i)<script(\s[^>]*)?>[^<]|<style|\son[a-z]+\s*=|javascript:`)
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(webFS, p)
		if err != nil {
			return err
		}
		// The SVG namespace is a name, not a fetch: createElementNS needs it.
		b = bytes.ReplaceAll(b, []byte(`"http://www.w3.org/2000/svg"`), nil)
		if m := external.Find(b); m != nil {
			t.Errorf("%s refers to another origin: %q", p, m)
		}
		if strings.HasSuffix(p, ".html") {
			if m := inline.Find(b); m != nil {
				t.Errorf("%s has inline script or style: %q", p, m)
			}
		}
		if strings.HasSuffix(p, ".js") {
			for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
				if strings.Contains(string(b), bad) {
					t.Errorf("%s uses %s", p, bad)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The GUI's models (web/graph.js, web/workload.js, web/transcript.js,
// web/secondary.js) have their own tests, testdata/<model>_test.js (graph_test.js
// for graph.js), and the screens DOM tests
// over a fake browser (testdata/fakedom.js); all run under node when it is
// installed. Every script in the bundle must at least parse.
func TestProjectViewModel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the GUI's JS tests need it")
	}
	dir := t.TempDir()
	err = fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		b, err := fs.ReadFile(webFS, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dir, path.Base(p))
		if err := os.WriteFile(out, b, 0o644); err != nil {
			return err
		}
		if msg, err := exec.Command(node, "--check", out).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s", p, err, msg)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range [][]string{
		{"graph_test.js", filepath.Join(dir, "graph.js")},
		// The project view itself, in a fake DOM over a fake API and
		// stream: it renders the wireframe's parts and follows events.
		{"projects_dom_test.js", dir},
		// The workload screens' model (web/workload.js), and the sandbox
		// card, Sandboxes list and agents map in the fake DOM.
		{"workload_test.js", filepath.Join(dir, "workload.js")},
		{"workload_dom_test.js", dir},
		// The transcript, timeline and pull-request models
		// (web/transcript.js, web/secondary.js), and those screens in the
		// fake DOM, a transcript labelled lost when its session ends
		// included.
		{"transcript_test.js", filepath.Join(dir, "transcript.js")},
		{"secondary_test.js", filepath.Join(dir, "secondary.js")},
		{"secondary_dom_test.js", dir},
	} {
		out, err := exec.Command(node, filepath.Join("testdata", tc[0]), tc[1]).CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v\n%s", tc[0], err, out)
			continue
		}
		t.Logf("%s:\n%s", tc[0], out)
	}
}

// The disabled Review button shows the TUI's own refusal text: keep
// graph.js's copy in step with internal/control.
func TestProjectViewRefusalText(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "control", "review.go"))
	if err != nil {
		t.Fatal(err)
	}
	js, err := fs.ReadFile(webFS, "web/graph.js")
	if err != nil {
		t.Fatal(err)
	}
	const refusal = "review starts on an idle work stream or kicks one already watching a PR (this one is "
	if !bytes.Contains(src, []byte(refusal)) {
		t.Fatalf("internal/control/review.go no longer refuses with %q: update graph.js and this test", refusal)
	}
	if !bytes.Contains(js, []byte(refusal)) {
		t.Errorf("graph.js does not show the :review refusal %q", refusal)
	}
	if !bytes.Contains(js, []byte("has no status yet to stop")) {
		t.Errorf("graph.js does not show the :stop refusal")
	}
}
