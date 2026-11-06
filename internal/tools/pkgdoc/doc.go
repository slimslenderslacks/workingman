// Command pkgdoc generates docs/packages.md, the package index for this
// module, from the package comments in each internal package's doc.go.
//
// It runs 'go list -json ./...' for the import graph and file lists, parses
// every package's non-test Go files with go/doc, and writes:
//
//   - an index table of every package under internal/ (import path, Role,
//     synopsis, exported interfaces and key exported types);
//   - a Mermaid graph of internal->internal imports, with the cmd/ binaries
//     as entry nodes and packages grouped and styled by Role.
//
// The Role comes from a 'Role: daemon|tui|shared|wrapper' line in the package
// comment (see docs/package-docs.md). A package without a doc.go or a valid
// Role line is listed as TODO rather than failing the run, so the index can be
// generated before every package is documented.
//
// The go list is pinned to GOOS=linux GOARCH=amd64 CGO_ENABLED=0 so the
// output does not depend on the machine that runs it. Packages under
// internal/tools/ (this one included) are build tooling and are left out.
//
// Usage, from the module root:
//
//	go run ./internal/tools/pkgdoc            # writes docs/packages.md
//	go run ./internal/tools/pkgdoc -o -       # writes to stdout
//
// 'task docs' regenerates the file; 'task docs:check' fails if it is stale.
package main
