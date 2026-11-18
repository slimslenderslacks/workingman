# SSH agent flow and the ssh push failure

Diagnosis of "the commit agent cannot push to ssh remotes". This is a code-reading
diagnosis; nothing here was reproduced against a live sandboxd (the diagnosing
sandbox has no `sbx` and no 1Password service account token). Items marked **unverified** need a check on the host.

## How SSH_AUTH_SOCK flows

1. `start.sh` starts a private `ssh-agent` (`$TMPDIR/orch-ssh.*/agent.sock`),
   loads one key into it from 1Password (`op-read "$ORCH_SSH_KEY_REF" | ssh-add -`),
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

## 1Password usage

1Password is read exactly once, at startup: start.sh's
`op-read "$ORCH_SSH_KEY_REF"` (step 1). The key then lives only in the private
agent's memory until start.sh exits. Every later consumer reuses that agent and
never reaches 1Password:

| Site | When | Touches 1Password? |
|------|------|--------------------|
| `start.sh` `op-read \| ssh-add -` | once per start.sh | **yes — the only call** |
| `start.sh` `sbx daemon stop/start -d`, `check-signing.sh --yes` | startup | no: binds sandboxd to the in-memory agent |
| `check-signing.sh` (manual / cron) restart of sandboxd | on demand | no: re-reads the agent *socket* from start.sh's env, never the key |
| `gitsign.Preflight` / `PushPreflight` (`sbx exec ... ssh-add -l`, `ssh -T`) | every ACP / `:session` launch | no: queries the forwarded agent |
| sandbox `git commit` / `git push` | per commit / push | no: `gpg.ssh.program=ssh-keygen` + forwarded agent |
| host-side `wsp` / `git fetch` (workspace provisioning) | per workspace create / repo add | no: orch sets `GIT_SSH_COMMAND=... -o IdentityAgent=SSH_AUTH_SOCK` and `gpg.ssh.program=ssh-keygen` at startup, overriding a 1Password `IdentityAgent` in `~/.ssh/config` or `op-ssh-sign` in git config |
| `gitsign.Resolve` / `Identity` (`git config --get`) | per launch | no: reads config only |

If the private agent itself is empty or dead, the only fix is restarting
start.sh, which costs one new `op-read`. `test/start-agent.sh` (with a stub
op-read via `ORCH_OP_READ`) asserts start.sh calls it exactly once.

### op-read: Go SDK + service account

`op-read` (`cmd/op-read`, logic in `internal/onepassword`) replaces the `op`
CLI. It uses the 1Password Go SDK (`github.com/1password/onepassword-sdk-go`,
pure Go, no CGO), authenticated as a 1Password **service account**, so there
is no `op signin`, unlocked CLI session or biometric prompt. It writes the
secret to stdout only, for start.sh to pipe into `ssh-add -`; errors name the
reference, never the token or the key.

- **Token.** `OP_SERVICE_ACCOUNT_TOKEN` if set (CI, sandboxes, tests);
  otherwise, on macOS,
  `security find-generic-password -a jim.clark -s workingman:service_account -w`
  (service, account and keychain overridable with `OP_SA_KEYCHAIN_SERVICE`,
  `OP_SA_KEYCHAIN_ACCOUNT`, `OP_SA_KEYCHAIN`). Seed the item once (it prompts
  for the token; `-U` replaces an existing item):

  ```sh
  security add-generic-password -U -a jim.clark -s workingman:service_account -w
  ```

  To keep it in a separate keychain file instead of the login keychain, add it
  there with Keychain Access and set `OP_SA_KEYCHAIN=<name>.keychain-db`.
  standalone-codemode's gateway reads the same item.
- **Reference.** `ORCH_SSH_KEY_REF`, default
  `op://workingman/SSH github/private key?ssh-format=openssh`. A service account
  only sees vaults it has been granted and **never a Personal vault**, so the
  SSH key item has to be moved or copied into a shared vault (here
  `workingman`) that the service account can read.
- **Format.** The SDK's `Secrets().Resolve` honours `?ssh-format=openssh` on an
  SSH key's private key field. As a belt-and-braces fallback, if a reference
  asks for that format but the value comes back as a PKCS#1/PKCS#8/SEC 1 PEM
  key, op-read converts it to OpenSSH with `golang.org/x/crypto/ssh` before
  writing it, so ssh-agent always gets an openssh-format key.

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
   `start.sh` hides failures of `sbx daemon stop/start` (`>/dev/null || true`),
   so a failed restart at startup is silent.
3. **The private agent died or holds the wrong key.** The agent lives and dies
   with `start.sh`. Since commits 88b7e66/eea3cd6 (Oct 6) the only agent is a
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
- Stop swallowing `sbx daemon stop/start` errors in `start.sh`; verify via
  `check-signing.sh --check` after the restart.
- Ensure the loaded key is the one authorised for push, or load several keys.
- If cause 4: allow `github.com:22` in sbx policy and seed `known_hosts`
  (or `GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=accept-new'`).
