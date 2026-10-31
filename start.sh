#!/usr/bin/env bash
# Launches ./start-orch.sh in a tmux session. start-orch.sh runs a private
# ssh-agent (key loaded from 1Password item "SSH github" in vault "Personal"
# via non-interactive `op`; override with ORCH_SSH_KEY_REF) whose lifetime is
# tied to that script, so keep this tmux window alive while orch runs.
#
# If start-orch.sh fails, keep the window open on its error. Otherwise tmux
# closes the window the moment the script exits and the error (e.g. a lapsed
# `op` session) is never seen. A clean exit still closes the window.
RUN='./start-orch.sh; rc=$?; if [ "$rc" -ne 0 ]; then printf "\nstart-orch.sh exited with status %s; press enter to close.\n" "$rc"; read -r _; fi; exit "$rc"'
if tmux has-session -t orch 2>/dev/null; then
    # Session already exists — run orch in a new window in it rather than just
    # re-attaching to whatever is already there. -c "$PWD" keeps the relative
    # ./start-orch.sh resolving against this repo, not the window's own cwd.
    tmux new-window -t orch -c "$PWD" -n orch bash -c "$RUN"
else
    tmux new-session -d -s orch -c "$PWD" -n orch bash -c "$RUN"
fi
# new-window/new-session leave the fresh window active, so attaching lands on it.
# Use switch-client when already inside tmux (a bare attach would refuse to nest).
if [ -n "$TMUX" ]; then
    tmux switch-client -t orch
else
    tmux attach -t orch
fi
