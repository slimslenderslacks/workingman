# Plan: opencode as an Alternative Agent Backend for workingman

This document proposes adding `opencode` (with a local Docker Model Runner
backend, or any of opencode's hosted providers) as an alternative to the
existing Claude Code agent that workingman currently shells out to. It is
**a plan, not an implementation** — no Go code is changed here. Companion
inputs:

- Investigation of the existing Claude integration:
  `../../notes/workingman.md` in this workspace (the orchestrator wraps the
  `claude` CLI in a tmux window per role; there is no internal agent
  loop). The summary below replays the relevant findings.
- End-to-end validation of opencode against four DMR-hosted local models:
  `../../notes/validation.md` in this workspace. Headline: only `ai/qwen3`
  drives opencode's `build` agent cleanly today.

File paths in this document are relative to the workingman repo root.

## 1. Goal and non-goals

**Goal.** Let an operator point workingman at either the Claude Code CLI
(`claude`) or the opencode CLI (`opencode`) per daemon run, with no
changes to the project schema, task schema, fsnotify state machine, tmux
launcher, or audit log. The same `.orch/instructions.md` and
`.orch/context.yaml` handoff must work for both. Operators who don't pass
a flag get exactly the current behaviour (Claude).

**Non-goals.**

- Re-implementing opencode's agent loop inside workingman. The loop stays
  in the CLI — that is the property that makes this swap trivial.
- Streaming model output through the daemon. workingman ignores stdout
  today; it must keep ignoring it.
- Per-task model selection through workingman config. Routing different
  Kinds to different models is opencode's job (its `agent` / `model`
  config) — see §4.2.
- Mixing CLIs within a single daemon run. The flag is process-wide.
  Per-Kind CLI selection is an extension we explicitly defer (§7.2).
- Touching anything outside workingman. This plan does not change
  opencode, DMR, or the wsp workspace tooling.

## 2. Inputs in one paragraph each

**Existing architecture (`notes/workingman.md`).** workingman is a Go
orchestrator (`orch`) that creates a tmux window per agent role and
launches `claude --dangerously-skip-permissions [--print] "Read
.orch/instructions.md and .orch/context.yaml, then follow the
instructions."` inside it. All model/provider/tool/prompt configuration
is owned by the `claude` binary; workingman owns only the working
directory, the two `.orch/*` handoff files, an optional `.claude/skills/`
tree, and the umbrella tmux session. The exit of the tmux window is the
only signal workingman uses to decide "agent done"; the agent's effects
are read back from `tasks/*.yaml` and `.project.yaml` on disk. The one
hard-coded Claude reference in the entire codebase is
`DefaultCommandBuilder` (`internal/runner/runner.go:62-82`) and the
literal string `"claude"` inside it. A `CommandBuilder` seam is already
defined and is already used by tests.

**Validation results (`notes/validation.md`).** Against DMR, only
`ai/qwen3` (8 B params, 32 K context) completed every test turn —
plain chat, code edit, file-read tool call, shell tool call — at
~6–14 s per warm turn. `ai/llama3.2` emitted tool calls with wrong
argument types (string `"0"` for `number`, `null` for `number |
undefined`) that opencode's strict input schemas reject. `ai/gpt-oss`
produced correct first-turn answers but never stopped — it looped on
opencode's anchored-summary follow-up prompt until the harness killed
it, and in the edit task it hallucinated a successful edit without
emitting an `edit` tool call. `ai/qwen3-coder` failed at the DMR API
layer because the curated `:latest` ships with a 4 K context window and
opencode's `build` system prompt is ~14 K tokens. These four failure
modes are the risk surface for local-model use of this backend.

## 3. Abstraction boundary

The right cut is already in the codebase.

`internal/runner/runner.go:58` declares:

```go
type CommandBuilder func(kind agent.Kind, workingDir string) []string
```

held on `Runner.Command` (`runner.go:88`), defaulting to
`DefaultCommandBuilder` when nil. The launcher
(`internal/agent/tmux.go:70-102`) takes a `[]string` argv. So the entire
"which agent CLI runs?" decision lives in a single function value on the
Runner. The Claude vs. opencode swap is, mechanically, just choosing
which `CommandBuilder` to assign at daemon startup.

This boundary is the right one because every other contract is already
agent-agnostic:

1. **Lifecycle.** `Kind.Interactive()`
   (`internal/agent/agent.go:45-47`) cleanly distinguishes one-shot
   Kinds (`Planning`, `Task`, `Commit`) from interactive ones
   (`Project`, `Wolf`). Both opencode (`opencode run <prompt>` vs.
   `opencode` for TUI) and Claude (`claude --print <prompt>` vs.
   `claude`) honour the same one-shot/interactive distinction. The
   tmux-window-disappearance check in `internal/agent/tmux.go:124-155`
   keeps working regardless.
2. **Handoff format.** Both CLIs are told to "Read
   .orch/instructions.md and .orch/context.yaml, then follow the
   instructions." That sentence is the integration contract. Both
   tools resolve the filesystem read tool and follow markdown
   instructions identically.
3. **Communication channel.** workingman reads
   `tasks/<task>.yaml` and `.project.yaml` after the window exits. No
   stdout parsing exists. Any CLI that can do tool-driven file
   write/edit + bash satisfies the contract.
4. **Permission posture.** Both tools support a
   "skip-confirmations" mode (Claude: `--dangerously-skip-permissions`
   flag; opencode: `permission.{edit,bash,external_directory}: "allow"`
   in `opencode.json`). Mechanism differs; effect is identical.

**Decision.** Do **not** introduce a Go-level "agent interface"
abstraction with `type Agent interface { Launch(...); ... }`. That
would invent a layer that has no other clients and obscure the fact
that the actual integration surface is one argv builder. Stick with
`CommandBuilder` and add a sibling `OpencodeCommandBuilder` next to
`DefaultCommandBuilder`. Two functions; selection by flag.

## 4. Invocation: subprocess, not library

opencode ships as a TypeScript/Bun client/server. Three integration
modes are plausible; only one fits workingman.

| Mode | Description | Verdict |
|---|---|---|
| Subprocess (`opencode run`) | Spawn the opencode CLI per role inside a tmux window. Exit semantics, env inheritance, working-directory propagation already match Claude. | **Chosen.** |
| Library (Go binding) | There is no Go SDK. Embedding the Bun runtime would mean either bundling Node/Bun or running an opencode server and talking HTTP to it. Either way, workingman would now own the agent loop. | Rejected. |
| Hosted server + HTTP API | opencode runs in client/server mode and can be driven over its server's API. Workable in principle, but breaks the property that the human can `tmux attach` to inspect a running session. | Rejected for v1. May reconsider for v2 — see §7.4. |

The subprocess mode is the only one that preserves workingman's existing
operational model (one tmux window per role, attachable by the user,
exit detected by window disappearance).

### 4.1 Command line

Two candidate builders, one per CLI, both honouring `Kind.Interactive()`:

```go
// internal/runner/runner.go

func DefaultCommandBuilder(kind agent.Kind, _ string) []string { // existing
    cmd := []string{"claude", "--dangerously-skip-permissions"}
    if !kind.Interactive() {
        cmd = append(cmd, "--print")
    }
    cmd = append(cmd, initialPrompt)
    return cmd
}

func OpencodeCommandBuilder(kind agent.Kind, _ string) []string { // new
    if kind.Interactive() {
        // Drop the user into the TUI; they will attach via tmux.
        return []string{"opencode"}
    }
    return []string{"opencode", "run", initialPrompt}
}
```

`initialPrompt` (`"Read .orch/instructions.md and .orch/context.yaml,
then follow the instructions."`) is unchanged. opencode resolves its
own working directory from the tmux `-c <dir>` the launcher already
sets (`internal/agent/tmux.go:74`).

**`--print` analogue in opencode.** opencode's `run` subcommand is
already non-interactive — it executes the prompt, prints to stdout, and
exits. There is no separate `--print` flag. The TUI is invoked by
bare `opencode` (or `opencode tui`). So `Kind.Interactive() == false →
opencode run`, `Kind.Interactive() == true → opencode` is the natural
mapping.

**`--dangerously-skip-permissions` analogue.** opencode has no CLI flag
with that exact effect; the equivalent is the `permission` block in
`opencode.json`. See §5.3.

### 4.2 Per-Kind model selection

`workingman.md` §6.2 raised this as an open question. The recommended
answer is **don't route per-Kind from workingman**. Two routes are
available without any workingman code change:

1. **opencode `agent` config.** opencode lets you define custom agents
   in `opencode.json` (or `.opencode/agent/<name>.md`) with their own
   `model` and tool overrides, and select one via
   `opencode run --agent <name>`. We do *not* want workingman to pass
   `--agent` because that re-introduces per-CLI knowledge into the
   builder. Instead, leave `--agent` unset — opencode will use the
   `default_agent` from config — and let the operator choose what that
   default is per workspace.
2. **`opencode.json` per workspace.** workingman already manages
   workspaces as discrete directories (`internal/workspace/`). Drop a
   different `opencode.json` per workspace if a project needs a
   different model. The merge precedence (project file overrides
   global) makes this work naturally.

Decision: workingman ships exactly one knob — *which CLI binary* — and
delegates *which model* to the CLI's own config. This keeps the new
surface area minimal and avoids leaking opencode-specific concepts (agent
names, model IDs, provider IDs) into workingman.

## 5. Config surface

### 5.1 New CLI flag

Add one flag to `cmd/orch/main.go` (around lines 51-58, alongside
`--workspace-manager`, `--tmux-session`, `--root`, `--audit-log`):

```
--agent-cli=claude|opencode   Which CLI to launch per agent window.
                              Default: claude.
```

The flag is process-wide for the daemon. If unset, `DefaultCommandBuilder`
(Claude) is used — current behaviour preserved.

Wiring at `cmd/orch/main.go:93-101` where the Runner is constructed:

```go
var builder runner.CommandBuilder
switch *agentCLI {
case "", "claude":
    builder = runner.DefaultCommandBuilder
case "opencode":
    builder = runner.OpencodeCommandBuilder
default:
    log.Fatalf("unknown --agent-cli value %q (want claude|opencode)", *agentCLI)
}
r := &runner.Runner{
    Workspaces: wsMgr,
    Launcher:   &agent.TmuxLauncher{...},
    Audit:      a,
    Command:    builder,
}
```

### 5.2 No schema changes

`.project.yaml` and `tasks/*.yaml` schemas (`internal/project/project.go`,
`internal/task/task.go`) get no new fields. The agent-CLI choice is an
operator setting, not a project setting — different operators running
the same project on different hosts may legitimately use different
backends.

If we later want per-project pinning, the field would go on
`.project.yaml` as `agent_cli: claude|opencode`, parsed alongside
`status`, `repos`, `branch`. Mark this as a v2 extension (§7.1). Do
not add it in v1.

### 5.3 Permissions

The opencode equivalent of `--dangerously-skip-permissions` is a
`permission` block in `opencode.json`:

```json
{
  "permission": {
    "edit": "allow",
    "bash": "allow",
    "external_directory": "allow"
  }
}
```

Two ways to deliver this to the agent:

1. **Operator owns it.** Document in the integration doc (the
   opencode_dmr repo) that operators must include the above block in
   their global `~/.config/opencode/opencode.json` or in a per-workspace
   `opencode.json` before pointing workingman at it. Simpler; no new
   code in workingman.
2. **Workingman writes it.** Extend `internal/setup/setup.go` to drop
   an `opencode.json` next to `.orch/` when `--agent-cli=opencode` is
   set, containing only the permission block (merged with whatever
   the operator already has). More integration, more failure modes.

**Decision: option 1 in v1.** The bar to use opencode mode is "you have
`opencode.json` set up to talk to your model of choice"; bundling the
permission block into that config is part of that setup, not workingman's
job. Revisit if operators consistently get this wrong (§7.3).

The `external_directory: "allow"` line is specifically called out in
`notes/validation.md` §6.4 — qwen3 sometimes resolved relative paths
into the parent of the working directory, which opencode auto-rejects
without that flag. The integration doc owns the wording.

### 5.4 Audit log

`internal/audit/` currently records `daemon_start`, `agent_launched`,
`agent_exited`, `task_status_changed`, etc. Add the chosen
`agent_cli` value to the `daemon_start` event payload so historic runs
can be traced back to the CLI flavour. One-line change to the audit
event struct; no schema migration since the audit log is JSONL and
consumers are tolerant to new fields.

## 6. Tool-passing, prompts, and skills

### 6.1 Prompt templates — no changes

`internal/prompts/templates/*.tmpl` describe role behaviour, not model
specifics. They say "the agent" and "this session" rather than naming
Claude or any tool. Verified by re-reading them during the workingman
investigation. They do not need editing for opencode mode.

If we later find a role needs an opencode-specific nudge — for example,
telling the planning agent to switch to the read-only `plan` agent via
Tab — we can add a conditional in the template using `text/template`
introspection of the kind. Defer until a real need surfaces.

### 6.2 Tool capability

The five role behaviours need: file read/write/edit (all of them),
bash (commit agent for `git add` / `git commit`; task agent for builds
and tests), and that is it. Validation confirmed opencode + qwen3
satisfies this set. The macOS notification used by the wolf agent is
emitted by Go's `notify.Osascript` *outside* the agent process, so the
model never has to invoke it.

`notes/validation.md` §6.3 surfaced a sharp landmine: the host's global
`~/.config/opencode/opencode.json` may have *every* built-in tool
disabled (`read`, `write`, `edit`, `glob`, `grep`, `bash`,
`todoread`, `todowrite` all `false`). If that is the case and the
project's `opencode.json` only adds a provider block, every model will
appear broken. Operators must re-enable the tools explicitly:

```json
{
  "tools": {
    "read": true,
    "write": true,
    "edit": true,
    "glob": true,
    "grep": true,
    "bash": true
  }
}
```

This belongs in the integration doc in `opencode_dmr/docs/`, not in
workingman. Mention in workingman's docs only with a pointer to the
opencode_dmr docs.

### 6.3 Skills directory

Today `internal/setup/setup.go:64-81` copies orchestrator-injected
skill trees into `.claude/skills/<Name>/`. opencode reads agent
definitions from `.opencode/agent/<Name>.md` and ancillary
instructions from paths listed in the `instructions` config field —
not from `.claude/skills/`.

**v1 decision.** When `--agent-cli=opencode` is selected and the
project has skills to inject, the setup layer should drop them into
`.opencode/agent/<Name>.md` instead of `.claude/skills/<Name>/`. Add a
`SkillDir` (or `Layout`) field on the `Skill` type, set by `setup.Apply`
based on the active CLI. The opencode agent format is a single
markdown file with frontmatter; the existing Claude skill format is a
directory tree. For now, accept that the v1 opencode mode supports
*flat* (single-file) skills only. Multi-file skills can be flagged at
load time as Claude-only.

If no current operator uses orchestrator-injected skills in autonomous
mode — investigate before implementing — this can be reduced to a
silent no-op for opencode mode in v1 and revisited later. (The
investigation flagged this as "skills, if any are passed by the
planning step today, would need a translation layer — or we accept that
opencode mode starts with no skill injection.") The conservative answer:
skip skills in v1 unless they exist; if they exist, refuse to run with
opencode and tell the operator which file to translate.

## 7. Streaming and output handling

workingman never reads model stdout today and must not start. The full
agent loop is observed through filesystem state after the tmux window
exits. opencode's `run` subcommand prints assistant text to stdout and
exits; that stream goes into the tmux window's scrollback, which is
exactly the same place Claude's output goes. The user can `tmux attach`
to inspect it; the daemon does not.

One concrete consequence: **opencode's anchored-summary follow-up turn**
(see `notes/validation.md` §4.3) can produce a very long trailing stream
for some models. With `ai/qwen3` this is benign. With `ai/gpt-oss` it
loops indefinitely. Because workingman uses the window-exit signal,
this manifests as the tmux window simply not closing, and the daemon
will wait forever (the `Wait` poll has no timeout —
`internal/agent/tmux.go:124-155`).

**Mitigation.** Add a per-Kind timeout on the launcher's `Wait`. The
investigation didn't surface this gap because the Claude CLI doesn't
exhibit the looping pathology. With opencode in the loop, a `--task-timeout=15m`
(or per-Kind override) becomes operationally important. Default to
`unlimited` to preserve current behaviour; set a finite default only when
`--agent-cli=opencode`.

This is one of the few places where adopting opencode adds a real
requirement to workingman code, not just config. It is small (a select
on `time.After` in the wait goroutine) but should be in the v1 PR, not
deferred — because without it, a misbehaving model can stall the
entire daemon.

## 8. Testing approach

Three layers, ordered from cheapest to most expensive.

### 8.1 Unit tests on the builder

Pattern is already in the tree:
`internal/runner/runner_test.go:49-71` —
`TestDefaultCommandBuilderModes` exercises the Claude builder per Kind.
Duplicate it for `OpencodeCommandBuilder`:

```go
func TestOpencodeCommandBuilderModes(t *testing.T) {
    cases := []struct {
        kind agent.Kind
        want []string
    }{
        {agent.PlanningAgent, []string{"opencode", "run", initialPrompt}},
        {agent.TaskAgent,     []string{"opencode", "run", initialPrompt}},
        {agent.CommitAgent,   []string{"opencode", "run", initialPrompt}},
        {agent.ProjectAgent,  []string{"opencode"}},
        {agent.WolfAgent,     []string{"opencode"}},
    }
    for _, c := range cases {
        got := runner.OpencodeCommandBuilder(c.kind, "/ignored")
        if !reflect.DeepEqual(got, c.want) {
            t.Errorf("Kind=%v: got %q, want %q", c.kind, got, c.want)
        }
    }
}
```

Fast, no external dependencies.

### 8.2 Runner-level integration with a fake launcher

The runner already accepts a `Launcher` interface and the tests use a
stub launcher that records the argv (`internal/runner/runner_test.go:93`).
Add a Run-with-opencode test that drives a TaskAgent through the
runner with `Command = OpencodeCommandBuilder` and a stub launcher,
and asserts the launcher was called with `["opencode", "run", initialPrompt]`
in the workspace dir. Verifies that the new CommandBuilder integrates
cleanly with the existing Runner without needing opencode installed.

### 8.3 End-to-end against a real opencode + DMR

CI cannot run this — DMR is local-host-specific and the validation
tests took ~3 minutes per model. Do it manually as part of v1
acceptance: pick `ai/qwen3` as the model, point `opencode.json` at DMR,
run the daemon against a tiny demo project with one task, confirm:

- The tmux window opens, named `Task-opencode-dmr` (or similar).
- `opencode run` exits after the model completes the task.
- `tasks/<task>.yaml` shows `status: success` and `updated_by: agent`.
- The daemon detects window exit and proceeds to the commit agent.
- The commit agent (also opencode) runs, stages files, commits, sets
  `status: committed`.

This is the validation regression that catches "did we break the
contract by switching CLIs?" Encode it as a script in
`docs/manual-tests/opencode-smoke.md` (a sibling of
`tui-survey.md`), not in CI.

### 8.4 What we are **not** testing in CI

- Specific opencode CLI flags / version behaviour. opencode is moving
  fast (1.14 → 1.15 during the validation period). Pin to a known-good
  version in the operator docs, not in Go tests. The Go side only
  cares that we shell out to *some* `opencode` binary.
- Model output quality. That is the operator's responsibility when
  choosing a model.

## 9. Phased rollout

A four-phase rollout that keeps Claude as the default the whole way.

### Phase 0 — landed already

Investigation (`notes/workingman.md`) and validation
(`notes/validation.md`) complete. This plan is the artefact.

### Phase 1 — minimal swap (1 PR)

In scope:

1. Add `OpencodeCommandBuilder` in `internal/runner/runner.go` next to
   `DefaultCommandBuilder`.
2. Add `--agent-cli=claude|opencode` flag to `cmd/orch/main.go`. Default
   `claude`.
3. Wire builder selection in the Runner construction
   (`cmd/orch/main.go:93-101`).
4. Add per-Kind wait timeout (`--task-timeout`, etc.) to the launcher to
   defend against the gpt-oss looping pathology (§7). Default
   `unlimited` for backwards compatibility; surface a recommended value
   in the docs for opencode mode.
5. Extend `daemon_start` audit event to record the chosen
   `agent_cli`.
6. Unit tests: `TestOpencodeCommandBuilderModes` plus a runner-level
   integration test with a stub launcher.
7. Manual smoke test against `ai/qwen3` + DMR per §8.3, written up in
   `docs/manual-tests/opencode-smoke.md`.

Out of scope (defer to later phases): skill translation, custom config
generation, per-Kind CLI selection, per-project `agent_cli` pinning.

Acceptance: a project that uses no skills and provides its own
`opencode.json` runs end-to-end on opencode + qwen3 + DMR via
`orch --agent-cli=opencode`. Claude path is untouched and continues to
pass existing tests.

### Phase 2 — skill translation (separate PR, only if used)

Only triggers if Phase 1 surfaces a real project that uses
orchestrator-injected skills. If yes:

1. Add `SkillLayout` field to the `Skill` type
   (`internal/setup/setup.go`). Values: `claude` (directory tree under
   `.claude/skills/<Name>/`), `opencode` (single file at
   `.opencode/agent/<Name>.md`).
2. Make `setup.Apply` pick the layout based on the active
   `CommandBuilder` (pass it through the call chain or, simpler, add a
   `CLI` field to the Runner that gets propagated).
3. Refuse to run when a Claude-format multi-file skill is asked for in
   opencode mode; print an error pointing the operator at a
   translation recipe in the docs.

Acceptance: a project with a single-file skill works on both CLIs; a
project with a multi-file skill works on Claude and fails fast on
opencode.

### Phase 3 — operator ergonomics (low priority)

Bundle the opencode permission block into a workspace `opencode.json` if
the operator passes a new flag like `--opencode-write-permissions`.
Bundle DMR provider config if `--opencode-provider=dmr` is passed.
Defer indefinitely; the operator-owns-config approach in Phase 1 is
sufficient.

### Phase 4 — opencode server mode (speculative)

If we later want to share an opencode session across multiple roles
(for example, the planning agent should have remembered what the
project agent said), switch from `opencode run` to long-lived
`opencode server` plus REST calls. This breaks the tmux-attach
property and is a much bigger change. Not on the roadmap; recorded
here only so the v1 boundary doesn't accidentally preclude it. The
existing `CommandBuilder` seam is flexible enough that a future
`OpencodeServerCommandBuilder` could fit alongside.

## 10. Risks

### 10.1 Risks specific to local models (from validation)

| Risk | Source | Mitigation |
|---|---|---|
| Tool-call schema rejections (`null` vs. `undefined`, string-typed numbers) | `ai/llama3.2` — `notes/validation.md` §4.1 | Document a minimum model bar in operator docs: must complete the four validation prompts. Recommend `ai/qwen3` as the floor. Defer schema-coercion adapter to a future opencode feature. |
| Silent edit hallucination (model claims success but never invokes `edit`) | `ai/gpt-oss` — `notes/validation.md` §4.3 | Treat as a model-quality issue, not a workingman bug. workingman's filesystem check (`tasks/*.yaml` says `status: success` but the actual diff is empty) will catch the *self-report-of-success* but **not** the underlying no-op. Add a commit-agent guardrail: if `status: success` but `git diff` shows zero changes for the task agent's repos, fail the commit agent and let the wolf agent investigate. This guardrail is independent of which CLI is in use and benefits Claude mode too. |
| Loop-on-compaction (model never stops responding) | `ai/gpt-oss` — `notes/validation.md` §4.3 | Per-Kind wait timeout in the launcher (§7). Default to a finite value (e.g. 15 min for autonomous Kinds) when `--agent-cli=opencode`. |
| Context window too small for opencode's 14 K-token system prompt | `ai/qwen3-coder` (4 K) — `notes/validation.md` §4.4 | Document 16 K minimum / 32 K recommended in operator docs. Not a workingman concern; surfaced here only so reviewers don't recommend qwen3-coder as a default. |
| Cold-start latency (~30 s) | All models — `notes/validation.md` §5 | Operator's choice. Optionally pre-warm via `docker model run` before starting the daemon. Document but do not paper over. |
| External-directory rejection on relative paths | `ai/qwen3` — `notes/validation.md` §6.4 | Tell operators to set `permission.external_directory: "allow"` in their `opencode.json`. Workingman could also write absolute paths into the prompt templates, but that couples the prompts to the runtime path. Prefer the config fix. |
| Global "tools-denied" config landmine | Host config — `notes/validation.md` §6.3 | Document in the integration doc loudly. Workingman cannot detect this — opencode runs, produces no tool calls, the agent finishes "successfully" with no work done. Same mitigation as the edit hallucination above: empty-diff commit guardrail. |

### 10.2 Risks specific to workingman

| Risk | Description | Mitigation |
|---|---|---|
| Process-wide CLI selection | `--agent-cli` is set once at daemon start. An operator who wants Claude for planning and opencode for task cannot have both without restarting the daemon. | Acknowledge as a v1 limitation. Per-project pinning (§5.2) is the natural extension; per-Kind selection is the more ambitious v2 (§7.2). |
| Operator confusion between Claude and opencode auth | `claude` reads `~/.claude/`, env vars like `ANTHROPIC_API_KEY`. `opencode` reads `~/.local/share/opencode/auth.json` and `opencode.json`. An operator may set one and expect the other to work. | Document side-by-side in the operator docs. Add a `--dry-run-cli` flag (defer to Phase 3) that runs `claude --version` or `opencode --version` and reports both auth locations so the operator can verify. |
| Tmux scrollback noise | opencode's `build` agent emits verbose intermediate output (tool calls, reasoning summaries). Tmux scrollback fills faster than with Claude's `--print` mode. | Tmux scrollback is already operator-configurable. Mention in docs; no code change. |
| Pinning opencode versions | opencode 1.14 vs. 1.15 schema differences are minor today, but the CLI is moving quickly. A breaking flag change would silently break this integration. | Document the supported opencode version in the workingman README (e.g. ">=1.14, <2.0"). Add a startup version check (`opencode --version`) gated behind `--agent-cli=opencode`. |

### 10.3 Risk we are accepting

Local-model agents are slower (warm 6–14 s/turn vs. ~1–3 s/turn for
hosted Claude) and less reliable (only qwen3 of the four local models
tested today). Operators choosing opencode + DMR over Claude are
trading correctness and speed for offline operation, cost, or
provider-independence. This trade is explicit and the integration doc
in the `opencode_dmr` repo will say so. Workingman's job is not to
hide the trade-off; it is to make the swap possible without lock-in.

## 11. Summary

The Claude-specific surface area in workingman is one function and one
literal string. The plan above turns that into two functions, one flag,
one audit field, one launcher timeout, and a short list of operator-facing
docs. Everything that makes the agent loop work — the model, the
prompt, the tools, the permissions — already lives outside workingman in
the CLI's own config and stays there. The v1 phase is small enough to
ship in one PR; later phases (skills, ergonomics, server mode) plug into
the same `CommandBuilder` seam without re-architecting.

The principal risk is not the integration itself — it is the local-model
ecosystem. Validation showed that exactly one of four DMR-hosted models
drives opencode's `build` agent today. Operators must know this. The
workingman side of the change does not make that risk smaller; it only
makes the trade-off available.
