// Package prompts renders the per-kind instruction text that orch writes to
// .orch/instructions.md for a newly launched agent.
//
// Role: daemon
//
// Each agent gets two files in its working directory:
//
//  1. .orch/context.yaml — structured data (paths, branch, current task, ...),
//     written by internal/setup.
//  2. .orch/instructions.md — rendered here from templates/<kind>.tmpl.
//
// The initial message piped to claude is short and just points it at those two
// files. So the templates hold the role-specific behavior (what to do, which
// files to touch, when to exit), and context.yaml holds the role-specific data.
//
// The templates are embedded with go:embed and parsed once at init. A bad
// template therefore panics at startup, not at launch time. There is one
// template per [agent.Kind], named after [agent.Kind.String]. Templates can call
// a few helpers, most importantly nameBudget (sbx.NameBudget), which gives the
// task-name length limit for a work stream so that prompts never hardcode it.
//
// # Key types
//
//   - [Data] — the template input shared by every kind. Each template uses
//     only the fields that apply to its role; the field comments say which
//     kind reads each one.
//   - [Render] — renders the template for a Kind, or returns an error when the
//     Kind has none.
//
// # Collaborators
//
// Uses internal/agent for Kind and internal/sbx for the sandbox-name budget.
// Used only by internal/runner, which fills a Data from its Plan and passes the
// rendered text to setup.Apply. The behavior each template asks for is described
// in agents.md.
package prompts
