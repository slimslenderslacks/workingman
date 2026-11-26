package api

import (
	"io/fs"
	"net/http"
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
