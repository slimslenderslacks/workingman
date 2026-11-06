package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Roles are the values a package comment's "Role:" line may take, in the
// order the index and graph present them.
var Roles = []string{"daemon", "tui", "shared", "wrapper"}

// todo marks a missing synopsis or Role.
const todo = "TODO"

// listedPackage is the subset of 'go list -json' output pkgdoc reads.
type listedPackage struct {
	ImportPath string
	Dir        string
	Name       string
	GoFiles    []string
	CgoFiles   []string
	Imports    []string
	Module     *struct{ Path string }
	Error      *struct{ Err string }
}

// Package is one row of the index: what pkgdoc learned about a package from
// go list and its package comment.
type Package struct {
	ImportPath string   // full import path
	Rel        string   // path relative to the module root, e.g. internal/runner
	Synopsis   string   // first sentence of the package comment, or TODO
	Role       string   // one of Roles, or TODO
	HasDocGo   bool     // whether the package has a doc.go
	Interfaces []string // exported interface types
	Types      []string // other exported types
	Imports    []string // Rel paths of imported internal packages
	ImportedBy []string // Rel paths of internal packages and commands importing this one
	Entry      bool     // a cmd/ binary rather than an internal package
}

func main() {
	out := flag.String("o", "docs/packages.md", "output file, or - for stdout")
	flag.Parse()

	listed, err := goList()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pkgdoc:", err)
		os.Exit(1)
	}
	pkgs, err := collect(listed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pkgdoc:", err)
		os.Exit(1)
	}

	var buf bytes.Buffer
	render(&buf, pkgs)

	if *out == "-" {
		_, err = os.Stdout.Write(buf.Bytes())
	} else {
		err = os.WriteFile(*out, buf.Bytes(), 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pkgdoc:", err)
		os.Exit(1)
	}
}

// goList runs 'go list -json ./...' with a fixed build context so the result
// is the same on every machine.
func goList() ([]listedPackage, error) {
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=")
	cmd.Stderr = os.Stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	var pkgs []listedPackage
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var p listedPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decoding go list output: %w", err)
		}
		if p.Error != nil {
			return nil, fmt.Errorf("%s: %s", p.ImportPath, p.Error.Err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// collect selects the internal packages and cmd/ entry points, reads their
// package docs, and resolves the internal import graph between them.
func collect(listed []listedPackage) ([]*Package, error) {
	byPath := map[string]*Package{}
	var pkgs []*Package
	for _, lp := range listed {
		if lp.Module == nil {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(lp.ImportPath, lp.Module.Path), "/")
		internal := strings.HasPrefix(rel, "internal/") && !strings.HasPrefix(rel, "internal/tools/")
		entry := strings.HasPrefix(rel, "cmd/")
		if !internal && !entry {
			continue
		}
		p := &Package{ImportPath: lp.ImportPath, Rel: rel, Entry: entry}
		if internal {
			if err := readDoc(p, lp); err != nil {
				return nil, err
			}
		}
		byPath[lp.ImportPath] = p
		pkgs = append(pkgs, p)
	}

	for _, lp := range listed {
		p, ok := byPath[lp.ImportPath]
		if !ok {
			continue
		}
		for _, imp := range lp.Imports {
			if dep, ok := byPath[imp]; ok && !dep.Entry {
				p.Imports = append(p.Imports, dep.Rel)
				dep.ImportedBy = append(dep.ImportedBy, p.Rel)
			}
		}
	}

	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Rel < pkgs[j].Rel })
	for _, p := range pkgs {
		sort.Strings(p.Imports)
		sort.Strings(p.ImportedBy)
	}
	return pkgs, nil
}

// readDoc parses a package's non-test sources and fills in its synopsis,
// Role and exported types.
func readDoc(p *Package, lp listedPackage) error {
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range append(append([]string{}, lp.GoFiles...), lp.CgoFiles...) {
		if name == "doc.go" {
			p.HasDocGo = true
		}
		f, err := parser.ParseFile(fset, filepath.Join(lp.Dir, name), nil, parser.ParseComments)
		if err != nil {
			return err
		}
		files = append(files, f)
	}
	dp, err := doc.NewFromFiles(fset, files, lp.ImportPath)
	if err != nil {
		return fmt.Errorf("%s: %w", lp.ImportPath, err)
	}

	p.Synopsis = dp.Synopsis(dp.Doc)
	if p.Synopsis == "" {
		p.Synopsis = todo
	}
	p.Role = parseRole(dp.Doc)

	for _, t := range dp.Types {
		if isInterface(t) {
			p.Interfaces = append(p.Interfaces, t.Name)
		} else {
			p.Types = append(p.Types, t.Name)
		}
	}
	return nil
}

var roleLine = regexp.MustCompile(`(?m)^Role:\s*([A-Za-z]+)\b`)

// parseRole returns the Role named on the package comment's "Role:" line, or
// TODO if there is none or it is not one of Roles.
func parseRole(text string) string {
	m := roleLine.FindStringSubmatch(text)
	if m == nil {
		return todo
	}
	role := strings.ToLower(m[1])
	for _, r := range Roles {
		if role == r {
			return r
		}
	}
	return todo
}

func isInterface(t *doc.Type) bool {
	for _, spec := range t.Decl.Specs {
		if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == t.Name {
			_, ok := ts.Type.(*ast.InterfaceType)
			return ok
		}
	}
	return false
}

// render writes the whole of docs/packages.md.
func render(w io.Writer, pkgs []*Package) {
	fmt.Fprint(w, `<!-- Code generated by internal/tools/pkgdoc; DO NOT EDIT. -->

# Package index

Generated from the package comments (doc.go) of every package under
`+"`internal/`"+` and from `+"`go list -json ./...`"+`. Do not edit by hand: change
the package's doc.go and run `+"`task docs`"+` (or `+"`go run ./internal/tools/pkgdoc`"+`).
`+"`task docs:check`"+` fails if this file is out of date. The conventions are in
[package-docs.md](package-docs.md).

Roles: **daemon** — the orch daemon process; **tui** — the TUI; **shared** —
used by both; **wrapper** — the acp-wrapper host. **TODO** means the package has
no doc.go or no valid `+"`Role:`"+` line yet.

`)

	renderIndex(w, pkgs)
	renderGraph(w, pkgs)
	renderDeps(w, pkgs)
}

func renderIndex(w io.Writer, pkgs []*Package) {
	fmt.Fprintln(w, "## Packages")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Package | Role | Synopsis | Interfaces | Key types |")
	fmt.Fprintln(w, "|---|---|---|---|---|")
	for _, p := range pkgs {
		if p.Entry {
			continue
		}
		synopsis := cell(p.Synopsis)
		if !p.HasDocGo && p.Synopsis != todo {
			synopsis += " (TODO: move package comment to doc.go)"
		}
		fmt.Fprintf(w, "| `%s` | %s | %s | %s | %s |\n",
			p.Rel, p.Role, synopsis, codeList(p.Interfaces), codeList(p.Types))
	}
	fmt.Fprintln(w)
}

func renderGraph(w io.Writer, pkgs []*Package) {
	fmt.Fprintln(w, "## Dependency graph")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Direct imports between internal packages; an arrow points from importer to")
	fmt.Fprintln(w, "imported. The `cmd/` binaries are the entry points.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "```mermaid")
	fmt.Fprintln(w, "graph LR")

	groups := map[string][]*Package{}
	for _, p := range pkgs {
		key := p.Role
		if p.Entry {
			key = "entry"
		}
		groups[key] = append(groups[key], p)
	}
	for _, g := range append([]string{"entry"}, append(Roles, todo)...) {
		if len(groups[g]) == 0 {
			continue
		}
		fmt.Fprintf(w, "  subgraph grp_%s[\"%s\"]\n", g, g)
		for _, p := range groups[g] {
			fmt.Fprintf(w, "    %s[\"%s\"]\n", nodeID(p.Rel), label(p))
		}
		fmt.Fprintln(w, "  end")
	}
	fmt.Fprintln(w)

	for _, p := range pkgs {
		for _, dep := range p.Imports {
			fmt.Fprintf(w, "  %s --> %s\n", nodeID(p.Rel), nodeID(dep))
		}
	}
	fmt.Fprintln(w)

	styles := map[string]string{
		"entry":   "fill:#e8e8e8,stroke:#555,stroke-width:2px,color:#000",
		"daemon":  "fill:#dbe9ff,stroke:#2f6fd6,color:#000",
		"tui":     "fill:#e4f6df,stroke:#3c9a2c,color:#000",
		"shared":  "fill:#fff1cc,stroke:#c99a06,color:#000",
		"wrapper": "fill:#f3e2fb,stroke:#8e44ad,color:#000",
		todo:      "fill:#fff,stroke:#c0392b,stroke-dasharray:4 3,color:#000",
	}
	for _, g := range append([]string{"entry"}, append(Roles, todo)...) {
		if len(groups[g]) == 0 {
			continue
		}
		ids := make([]string, len(groups[g]))
		for i, p := range groups[g] {
			ids[i] = nodeID(p.Rel)
		}
		fmt.Fprintf(w, "  classDef %s %s\n", g, styles[g])
		fmt.Fprintf(w, "  class %s %s\n", strings.Join(ids, ","), g)
	}
	fmt.Fprintln(w, "```")
	fmt.Fprintln(w)
}

func renderDeps(w io.Writer, pkgs []*Package) {
	fmt.Fprintln(w, "## Dependencies")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The same graph as text: each package's internal imports and importers.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Package | Imports | Imported by |")
	fmt.Fprintln(w, "|---|---|---|")
	for _, p := range pkgs {
		fmt.Fprintf(w, "| `%s` | %s | %s |\n", p.Rel, codeList(trim(p.Imports)), codeList(trim(p.ImportedBy)))
	}
}

// nodeID turns a module-relative path into a Mermaid node ID.
func nodeID(rel string) string {
	return strings.NewReplacer("/", "_", "-", "_", ".", "_").Replace(rel)
}

// label is a node's display text: the path without the internal/ prefix.
func label(p *Package) string {
	return strings.TrimPrefix(p.Rel, "internal/")
}

func trim(rels []string) []string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = strings.TrimPrefix(r, "internal/")
	}
	return out
}

func codeList(names []string) string {
	if len(names) == 0 {
		return "—"
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + n + "`"
	}
	return strings.Join(quoted, ", ")
}

// cell escapes text for a Markdown table cell.
func cell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
}
