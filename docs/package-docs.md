# Package documentation

How the packages under `internal/` are documented, and why. The generated
index of every package — role, synopsis, exported types, and the dependency
graph — is [packages.md](packages.md).

## The approach

1. **Each package's `doc.go` is the source of truth.** Every package under
   `internal/` has a `doc.go` holding only its package comment: what the
   package is for, which process it runs in, its key exported types and
   interfaces, and the packages it works with. It is an ordinary Go package
   comment, so `go doc`, gopls hovers and pkg.go.dev all show it.
2. **The summary is generated, never written by hand.**
   [`internal/tools/pkgdoc`](../internal/tools/pkgdoc) reads
   `go list -json ./...` and parses each package with `go/doc`, then writes
   [packages.md](packages.md): an index table and a Mermaid dependency graph
   of internal imports, grouped by Role. The synopsis column is the first
   sentence of each package comment, the Role column is its `Role:` line, and
   the interface and type columns plus the graph come from the code itself.
3. **CI checks it.** `task docs:check` regenerates the file and fails if the
   committed copy differs, so the index can't fall behind the code.

```sh
task docs         # regenerate docs/packages.md
task docs:check   # regenerate, then fail if docs/packages.md changed
go doc ./internal/runner   # read one package's full doc
```

## Why this over the alternatives

The goal is documentation that costs little to keep up and that summarizes
itself, so an agent (or a person) can find their way around the codebase
quickly.

- **Hand-written markdown per package** (e.g. `docs/runner.md`). It sits far
  from the code, nothing checks it, and it drifts as soon as a type is renamed
  or an import added. The same prose in `doc.go` sits next to the code it
  describes, gets changed in the same diff, and shows up in `go doc`.
- **A hand-maintained index or architecture diagram.** This is the part that
  goes stale fastest: every new package or import makes it wrong. Generating
  it from `go list` makes it correct by construction, and CI catches a stale
  copy.
- **A godoc / pkgsite server.** It renders the same package comments, but it is
  a service to run and browse, it has no "Role" (which process a package belongs
  to) and no whole-module dependency view, and an agent can't `cat` it. The
  `doc.go` approach still works with pkg.go.dev for anyone who wants it.
- **ADR / C4 / architecture tooling** (Structurizr, arc42, adr-tools, …).
  These are good for recording decisions and system context, but they bring a
  separate model to keep in sync with the code, plus new tools and syntax.
  That is too heavy for describing what 30 packages do. Design decisions
  still go in prose docs (`design.md`, `docs/*.md`); package docs link to
  them.

What this gives us:

- **Little to maintain.** You write one paragraph or so per package, in the
  file next to the code. Everything else is derived.
- **Easy for agents.** `docs/packages.md` is a single plain-text file that
  covers every package, its role, its API surface and the import graph. An
  agent can read it before opening any code, then `go doc <pkg>` or open
  `doc.go` for detail.
- **Can't drift.** The derived parts are generated and CI-checked; the prose
  is reviewed alongside the code it describes.
- **No dependencies.** The generator uses only the standard library and the Go
  toolchain.

## The `doc.go` convention

Every package under `internal/` (tooling under `internal/tools/` excepted) has
a `doc.go` containing **only** the package comment and the `package` clause.
A package has exactly one package comment: if another file has one, move it
into `doc.go`.

The comment is laid out in this order:

1. **Synopsis** — the first sentence, of the form `Package foo ...`. It
   becomes the index's synopsis column, so it should say what the package is
   for in one sentence. (`go/doc` takes everything up to the first period
   followed by a space, so avoid abbreviations like "e.g." in it.)
2. **Role line** — a paragraph of its own: `Role: daemon`, `Role: tui`,
   `Role: shared` or `Role: wrapper`.

   | Role | Meaning |
   |---|---|
   | `daemon` | Runs only on the orch daemon side (`orch` without the TUI): watching, dispatch, launching agents, channels. |
   | `tui` | Runs only on the TUI side (`orch` with the terminal UI). |
   | `shared` | Used by both the daemon and the TUI (and possibly acp-wrapper): data models, on-disk formats, small helpers. |
   | `wrapper` | Belongs to the `acp-wrapper` host process (`cmd/acp-wrapper`). |

   Choose the role from the real import graph (`go list -f '{{.ImportPath}}
   {{.Imports}}' ./...` and the graph in packages.md), not from the package
   name. Text after the role word on the same line is allowed and ignored,
   e.g. `Role: shared (daemon and wrapper)`.
3. **Overview** — a few short paragraphs on what the package does, how it is
   used, and anything surprising: files on disk, sockets, processes,
   concurrency, invariants.
4. **Key types** — the exported types and interfaces that matter, one line
   each, saying what each is for and who uses it. The generator already lists
   every exported type name, so focus on the important ones and how they fit
   together rather than listing them all.
5. **Collaborators** — the internal packages it builds on and the ones that
   use it, with a few words on why. Link design docs here too (e.g.
   `docs/channels.md`).

Use godoc syntax: `# Heading` lines for sections, `  - ` for lists, indented
blocks for code, `[Name]` and `[pkg.Name]` to link identifiers. Keep it short.
A package doc is a map of the package, not a manual; detail belongs on the
types and functions.

Exported interfaces and key types also need their own godoc comments. The
index shows only their names; `go doc` shows the comments.

### Template

```go
// Package foo <does one thing, in one sentence>.
//
// Role: daemon
//
// <Overview: what problem it solves, how it is driven, what it reads and
// writes (files, sockets, processes), and any invariants or concurrency rules
// a caller must know.>
//
// # Key types
//
//   - [Foo] — <what it is and who holds one>.
//   - [Bar] — <interface; who implements it, who consumes it>.
//   - [New] — <main constructor / entry point>.
//
// # Collaborators
//
// Uses [github.com/slimslenderslacks/work/internal/bar] for <why>. Used by
// internal/daemon (<how>) and cmd/orch (<how>). See docs/<topic>.md.
package foo
```

When adding a package, write its `doc.go` and run `task docs`. When changing
a package's role, purpose or main types, update its `doc.go` in the same
change and run `task docs`; `task docs:check` fails otherwise.
