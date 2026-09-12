#!/usr/bin/env bash
if tmux has-session -t orch 2>/dev/null; then
    # Session already exists — run orch in a new window in it rather than just
    # re-attaching to whatever is already there. -c "$PWD" keeps the relative
    # ./start-orch.sh resolving against this repo, not the window's own cwd.
    tmux new-window -t orch -c "$PWD" -n orch ./start-orch.sh
else
    tmux new-session -d -s orch -n orch ./start-orch.sh
fi
# new-window/new-session leave the fresh window active, so attaching lands on it.
# Use switch-client when already inside tmux (a bare attach would refuse to nest).
if [ -n "$TMUX" ]; then
    tmux switch-client -t orch
else
    tmux attach -t orch
fi
