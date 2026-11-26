package api

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The GUI is a small, dependency-free vanilla JS/CSS bundle, checked in under
// web/ and embedded, so `go build` stays the only build step (R.2). It loads
// nothing from anywhere else: the CSP (guiCSP) would refuse it.
//
//go:embed web
var webFS embed.FS

// assetExt are the extensions of bundle files: a missing one is a 404, not
// the app shell. Other paths may carry a dot (project ids do).
var assetExt = map[string]bool{".js": true, ".css": true, ".html": true, ".svg": true, ".png": true, ".ico": true, ".map": true, ".json": true}

// webHandler serves the GUI: the files under web/ by name, and index.html
// for "/" and every other path, so the app shell's routes (/login,
// /projects/{id}, ...) survive a reload. A missing asset is a 404. The
// bundle needs no credentials; the data it shows comes from the API, which
// does.
func webHandler() http.Handler {
	root, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, r.Method+" is not allowed on "+r.URL.Path)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" || name == "index.html" {
			serveIndex(w, r, root)
			return
		}
		if fi, err := fs.Stat(root, name); err == nil && !fi.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		if assetExt[path.Ext(name)] {
			http.NotFound(w, r)
			return
		}
		serveIndex(w, r, root)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, root fs.FS) {
	body, err := fs.ReadFile(root, "index.html")
	if err != nil {
		http.Error(w, "the GUI bundle is missing index.html", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The login page's URL carries the code in its fragment, which the
	// browser never sends; no-store keeps the page itself out of caches.
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}
