#!/usr/bin/env bash
if ! tmux has-session -t orch 2>/dev/null; then
    tmux new-session -d -s orch
fi
tmux new-window -t orch -n orch ./start-orch.sh
tmux attach -t orch
