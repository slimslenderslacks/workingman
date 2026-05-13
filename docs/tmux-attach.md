# Tmux Attach from the Sessions Pane

Pressing Enter (or clicking) on a session row in the orch TUI suspends the
TUI, foregrounds `tmux attach -t <target>`, and resumes the TUI once the user
detaches. The plumbing rides on `tea.ExecProcess`, which handles releasing
and restoring the alt-screen.

Pre-flight checks short-circuit before suspending the terminal so failures
appear in the footer instead of leaving the user staring at a flicker:

- `tmux` binary not on PATH → footer reads `attach <target>: tmux not found on PATH`
- stdin is not a TTY → `attach <target>: cannot attach without a TTY`
- target session is dead (no `has-session` match) → `attach <target>: tmux session "<target>" is not alive`
- selected row has no `TmuxTarget` → `attach <target>: no tmux target for this session`

The footer error replaces the keybinding hint until the next selection
change, focus change, or attach attempt clears it.

## Manual Test Plan

Build:

```
go build -o orch ./cmd/orch
```

### 1. Happy path: attach and detach

1. From two separate terminals, run:

   ```
   tmux new-session -d -s orch-task-alpha 'top'
   ./orch --root /tmp/orch-roots
   ```

   (Use any `--root` containing a `.project.yaml`; the goal is just to start
   the daemon so the embedded TUI mounts. For a true end-to-end run, point
   `--root` at a directory the daemon will populate with a real agent.)

2. Wait for the sessions pane to show at least one row. Press <kbd>Tab</kbd>
   or <kbd>←</kbd> if needed to focus the sessions pane.
3. Use <kbd>↓</kbd>/<kbd>↑</kbd> to highlight the target row, then press
   <kbd>Enter</kbd>. Expect the TUI to disappear and `top` to take over.
4. Press <kbd>Ctrl</kbd>+<kbd>b</kbd> <kbd>d</kbd> (tmux detach). Expect the
   TUI to redraw with no leftover artifacts and the footer to read the
   normal hint line.

### 2. Mouse click

1. With the TUI running and at least two session rows visible, click a row
   *other than* the currently selected one. Expect the click to: (a) move
   the selection marker to that row, and (b) immediately attach. Detach
   with <kbd>Ctrl</kbd>+<kbd>b</kbd> <kbd>d</kbd> and confirm the TUI
   redraws.
2. Click anywhere outside the sessions pane (the projects gallery on the
   right). Expect no attach.
3. Click on the blank separator line between two session rows. Expect no
   attach.

### 3. Missing tmux

1. Move `tmux` off PATH temporarily (`PATH=/usr/bin ./orch ...` if your
   tmux is in `/opt/homebrew/bin`, or use `which tmux` to confirm the
   path you're stripping).
2. Press <kbd>Enter</kbd> on a session row. Expect the footer to flash red
   with `attach <target>: tmux not found on PATH`. The TUI must remain on
   the alt-screen — no terminal swap should occur.

### 4. Dead session

1. Note the `TmuxTarget` of a row (e.g. `orch-task-alpha`).
2. From another terminal, kill it: `tmux kill-session -t orch-task-alpha`.
3. Within a polling interval the row will disappear from the pane. To
   exercise the race, kill it and immediately press <kbd>Enter</kbd> before
   the next snapshot lands. Expect:
   `attach <target>: tmux session "<target>" is not alive`.

### 5. No TTY

1. Redirect stdin from `/dev/null`:

   ```
   ./orch --root /tmp/orch-roots < /dev/null
   ```

   The TUI may refuse to start (bubbletea also requires a TTY); that is the
   correct outcome.

2. To exercise the attach-time guard specifically, run the daemon under a
   pseudo-terminal harness that leaves stdin as a non-character device, then
   press <kbd>Enter</kbd> on a row. Expect:
   `attach <target>: cannot attach without a TTY`.

### 6. Resume cleanliness

After every attach/detach cycle in §1–§4, verify that:

- The session rows still render with their selection marker on the right row.
- The projects gallery is unchanged.
- Typing <kbd>q</kbd> exits the TUI cleanly and the daemon shuts down with
  the usual `daemon_stop` audit line.
