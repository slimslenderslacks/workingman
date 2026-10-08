# SSH agent flow and the ssh push failure

Diagnosis of "the commit agent cannot push to ssh remotes". This is a code-reading
diagnosis; nothing here was reproduced against a live sandboxd (the diagnosing
sandbox has no `sbx`/`op`). Items marked **unverified** need a check on the host.

## How SSH_AUTH_SOCK flows

1. `start-orch.sh` starts a private `ssh-agent` (`$TMPDIR/orch-ssh.*/agent.sock`),
   loads one key into it from 1Password (`op read "$ORCH_SSH_KEY_REF" | ssh-add -`),
   and exports `SSH_AUTH_SOCK` to that socket.
2. It then runs `sbx daemon stop; sbx daemon start -d` (output discarded, `|| true`)
   so **sandboxd inherits that SSH_AUTH_SOCK**.
3. sbx exposes an agent in every sandbox at `/run/ssh-agent.sock` (set as the
   sandbox's `SSH_AUTH_SOCK`), proxying to whatever agent *sandboxd* was launched
   with. Not the sbx client's env, not a bind-mounted host socket.
4. `acpwrapper` deliberately leaves `SSH_AUTH_SOCK` alone and only injects git
   signing config (`gitsign.SigningEnvArgs`, `gpg.ssh.program=ssh-keygen`).
5. `git push` over ssh inside the sandbox uses the **same** `/run/ssh-agent.sock`.
   Push and signing share one agent; nothing in the wrapper configures push.
6. `wsp.go:sshURLFromIdentity` turns every wsp identity into `git@host:org/repo.git`,
   so repos cloned/registered by wsp have **ssh** remotes. The sbx proxy only
   injects credentials for HTTPS git, so ssh remotes depend entirely on step 3.
7. `commit.tmpl` push step runs plain `git -C <repo> push`; a rejected push
   (including auth failure) sets the task `blocked`.

## Root-cause candidates (most likely first)

1. **Push is never preflighted.** `gitsign.Preflight` / `signingPreflight` run only
   when `SigningKey != ""`, and check only that `ssh-add -l` lists *a* key. If
   signing is not configured there is no agent check at all, and if signing is
   configured the failure path (`withSigningPreflightResult`) merely drops signing.
   A dead/empty agent therefore still lets the commit succeed (unsigned) and the
   failure surfaces only later, at `git push`, as `Permission denied (publickey)`.
   The preflight protects commits, not pushes.
2. **sandboxd restarted without the private agent.** If Docker Desktop/sbx restarts
   sandboxd (update, crash, sleep/wake) it comes back bound to the launchd system
   agent, so every sandbox, new or reused, sees an empty agent. The forwarded
   agent is live per-call, so it affects existing sandboxes too. `check-signing.sh`
   detects and repairs this, but nothing runs it automatically, and
   `start-orch.sh` hides failures of `sbx daemon stop/start` (`>/dev/null || true`),
   so a failed restart at startup is silent.
3. **The private agent died or holds the wrong key.** The agent lives and dies
   with `start-orch.sh`. Since commits 88b7e66/eea3cd6 (Oct 6) the only agent is a
   private one with a *single* key from `ORCH_SSH_KEY_REF`; the old 1Password
   agent (all keys) was dropped. That key was chosen as the *signing* key. If it
   is not the key authorised for the repo/org (SSO authorisation, deploy key, a
   different account), signing works but push is denied. **Unverified.** This
   timing matches "new failure".
4. **Sandbox network/host-key (unverified).** ssh to github.com needs outbound
   port 22 allowed by the sbx firewall and a `known_hosts` entry/host key accepted
   inside the sandbox. Neither is configured anywhere in this repo (no
   `known_hosts`, `GIT_SSH_COMMAND`, or `StrictHostKeyChecking` handling). A
   firewall block or "Host key verification failed" would look like a push
   failure while the agent is fine.

## How to tell which one (next task)

Run on the host, then inside a sandbox (`sbx exec <name> -- ...`):

- `./check-signing.sh --check`: private agent OK? sandbox agent empty? (cause 2/3)
- `sbx exec <name> -- ssh-add -l`: key listed? compare fingerprint with the key
  GitHub authorises for the repo (cause 3).
- `sbx exec <name> -- ssh -T git@github.com`: shows `Permission denied (publickey)`
  (1-3), `Host key verification failed` or a timeout (4).
- `sbx policy log`: any blocked connection to github.com:22.

## Suggested fixes

- Generalise the preflight to "agent reachable and holds a key" independent of
  signing, log loudly, and record it in `session.json` (e.g. `PushBroken`).
- Have the preflight (or `check-signing.sh`) also run `ssh -T git@github.com` (or
  `git ls-remote` on the target remote) so it proves push, not just a key.
- Stop swallowing `sbx daemon stop/start` errors in `start-orch.sh`; verify via
  `check-signing.sh --check` after the restart.
- Ensure the loaded key is the one authorised for push, or load several keys.
- If cause 4: allow `github.com:22` in sbx policy and seed `known_hosts`
  (or `GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=accept-new'`).
