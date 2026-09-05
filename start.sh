#!/usr/bin/env bash
if ! tmux has-session -t orch 2>/dev/null; then
    tmux new-session -d -s orch -n orch ./start-orch.sh
fi
tmux attach -t orch
