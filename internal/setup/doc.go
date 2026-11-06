// Package setup writes the .orch handoff files and installs skills in an
// agent's working directory before the agent is launched.
//
// Role: daemon
//
// Every file setup writes lives under one of two directories in the working
// directory:
//
//	.orch/                        # orchestrator-owned state
//	  context.yaml                # structured data (paths, branch, kind, ...)
//	  instructions.md             # rendered prompt text (from internal/prompts)
//	.claude/skills/<name>/        # one directory per installed skill
//
// [Apply] is not transactional: a partial failure can leave some files behind.
// The orchestrator treats a workspace as disposable on error. The files are
// rewritten on every launch, so a relaunched agent always reads fresh context.
//
// # Key types
//
//   - [Context] — the structured half of the handoff, marshalled to
//     context.yaml. Fields that don't apply to a role are omitted. It mirrors
//     prompts.Data field for field.
//   - [Skill] — a source directory copied verbatim into .claude/skills/<Name>/.
//   - [Apply] — writes context.yaml, instructions.md and the skills into a
//     working directory that already exists.
//
// # Collaborators
//
// No internal imports (it uses only yaml.v3). Used only by internal/runner,
// which calls Apply after rendering instructions with internal/prompts and
// before launching the agent. See agents.md for what each kind reads from
// these files.
package setup
