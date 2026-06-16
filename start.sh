#!/usr/bin/env bash
tmux new-window -t orch -n orch ./start-orch.sh
tmux attach -t orch
