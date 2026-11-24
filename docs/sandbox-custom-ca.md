# Sandboxes with an optional, per-laptop custom CA — design

Status: implemented (`internal/sbx` `ExtraCAScript` / `EnsureExtraCA`,
`acpwrapper.gitTLSPreflight`, `check-ca.sh`) by the tasks
`acp-kit-ca-install-helper`, `workingman-extra-ca-config`,
`apply-ca-on-sandbox-create` and `ca-preflight-and-check-script` (checklist
in §9).

## 0. Summary

On a laptop whose VPN does TLS inspection, the network re-signs some HTTPS
traffic with a corporate CA. sbx sandboxes do not trust that CA, so `git` over
HTTPS to github.com fails inside them, and so does the agent when it talks to
`api.anthropic.com` directly.

The fix is:

- **One setting.** The orch flag `--extra-ca-certs <pem-path>`, which falls
  back to `$WORKINGMAN_EXTRA_CA_CERTS`. It is validated at startup and passed
  daemon → runner → `acp-wrapper --extra-ca-certs`. When it is unset nothing
  changes: no extra argv and no extra `sbx` calls.
- **One mechanism.** On every launch, after the sandbox exists and before any
  agent or preflight runs, workingman runs a small idempotent POSIX-sh script
  over `sbx exec`. The script writes the PEM to
  `/usr/local/share/ca-certificates/workingman-extra-ca.crt` and runs
  `update-ca-certificates`. curl, git, node/Claude Code and Python already read
  the system bundle in sbx sandboxes, so **no extra `-e` env vars are needed**
  (§2).
- **workingman owns the script. acp-kit does not change** (§5).
- **Assurance.** The in-sandbox preflight also probes `https://github.com` and
  fails the launch with a hint if the certificate is rejected. A host script,
  `check-ca.sh`, runs the probes in a throwaway sandbox, first without the CA
  and then with it.

**The sample `corporate-ca.cer` will not work.** It is the wrong root, it has
expired, and it is DER-encoded rather than PEM (§1).

## 1. Which CA does the VPN actually use?

All the measurements below were taken **from inside an sbx sandbox on this
laptop** (2026-11-23, Ubuntu 26.04, OpenSSL 3.5.5, curl 8.18.0, git 2.53.0
with libcurl-gnutls, node 22.22.1). Each one ran
`openssl s_client -proxy gateway.docker.internal:3128 -showcerts`, i.e. through
the sbx proxy, the same path the agent's traffic takes.

### 1.1 The sample file

`/Users/jim.clark/orch/sandbox-ca-certs/corporate-ca.cer`:

| | |
|---|---|
| Encoding | **DER** (binary, 730 bytes). It is not PEM. OpenSSL 3 auto-detects DER, which is why `openssl x509 -noout ...` reads it, but `update-ca-certificates`, `NODE_EXTRA_CA_CERTS`, `GIT_SSL_CAINFO` and `curl --cacert` all need PEM. |
| Subject = Issuer | `CN=Okta Inc Root CA` (RSA, sha256WithRSA) |
| Validity | `notBefore=Dec 15 22:30:17 2023 GMT`, **`notAfter=Dec 14 22:30:17 2025 GMT` — expired** |
| SHA-256 | `67:9B:7E:01:00:EF:47:86:64:CA:CC:2B:E6:02:86:E9:CF:75:C7:D1:B4:FF:EF:6B:74:C3:C3:3A:5B:59:57:3C` |

### 1.2 What the VPN presents today

`github.com:443`, `api.anthropic.com:443` and `deb.nodesource.com:443` all
present the same chain:

```
0 s:CN=github.com                         i:CN=Okta Inc Forward Trust CA ECDSA, O=Okta
1 s:CN=Okta Inc Forward Trust CA ECDSA    i:CN=Okta Inc Decryption Root CA, O=Okta   (2025-11-17 .. 2030-11-16)
2 s:CN=Okta Inc Decryption Root CA        i:CN=Okta Inc Decryption Root CA, O=Okta   (2025-11-17 .. 2030-11-16, self-signed)
```

- Root: **`CN=Okta Inc Decryption Root CA, C=US, ST=California, O=Okta`**
  - SHA-256 `A3:9F:E4:9D:3A:F8:98:39:2D:62:A4:62:38:70:50:30:99:DE:1B:C0:7A:20:E5:F8:E1:A7:57:96:F3:57:79:9A`
  - SKI `7D:A1:A4:2B:74:7E:8A:36:C6:3A:F5:9E:3E:CB:80:01:6C:22:B3:FB`
- The intermediate's AKI is the same `7D:A1:...`. The sample root has a
  different subject, a different key (its pubkey sha256 differs) and no SKI.
  **The VPN does not chain to the sample root at all.** "Decryption Root CA"
  is a new hierarchy that was issued on 2025-11-17, a month before the old root
  expired. It is not a renewal of the old root.

Other hops, for completeness:

| Host (via the sbx proxy) | Issuer seen in the sandbox | Needs the corporate CA? |
|---|---|---|
| `github.com` (git smart-HTTP, `git ls-remote`/`clone`/`fetch` over https) | Okta Forward Trust CA ECDSA | **yes** |
| `api.anthropic.com` (agent with no LLM-gateway secret) | Okta Forward Trust CA ECDSA | **yes** |
| `deb.nodesource.com` (kit install, only when Node < 18) | Okta Forward Trust CA ECDSA | yes, but only in the kit install step, which runs before workingman can install anything (§3.4) |
| `api.github.com`, `codeload.github.com` | Sectigo (public) | no (the VPN bypasses them) |
| `objects.githubusercontent.com`, `raw.githubusercontent.com` | Let's Encrypt (public) | no |
| `registry.npmjs.org`, `proxy.golang.org` | Google Trust Services (public) | no |
| `llm.es.atko.services` (LLM gateway), `claude.ai`, `host.docker.internal` | **Docker Sandboxes Proxy CA** | no. The sbx proxy terminates TLS, re-signs with its own CA (which the sandbox already trusts) and does credential injection. |

Why git to github.com needs the CA even though sbx injects credentials: the
sbx proxy only re-signs hosts it injects secrets for (the LLM gateway in the
table above). For `github.com` in this sandbox it is a plain CONNECT tunnel.
The proxy trusts the corporate chain upstream and passes it through unchanged,
as `internal/acpwrapper/llmtls.go:28-35` already describes for the API. So the
sandbox's own TLS client sees the Okta chain. git over **ssh** (`git@github.com`,
the push path that `check-signing.sh` probes) does not use TLS and is
unaffected.

### 1.3 How an expired trust anchor is treated

This was tested with a throwaway CA that expired on 2024-01-01 and a valid
leaf it signed, served by `openssl s_server`:

| Client | Result |
|---|---|
| `openssl verify` | `error 10 at 1 depth lookup: certificate has expired` |
| curl 8.18 (OpenSSL) | exit 60, `certificate has expired (10)` |
| git 2.53 (`git-remote-https` → libcurl-**gnutls**) | `SSL certificate verification failed: certificate has expired` |
| node 22 (`NODE_EXTRA_CA_CERTS`) | `CERT_HAS_EXPIRED` |

**An expired anchor is useless.** None of these clients will chain to it. A
bundle that holds *both* the expired old root and the current one works: the
expired one is just dead weight. Measured against github.com, the system bundle
plus the sample (as PEM) plus the Decryption Root gave curl 0, git 0 and
node 200. The system bundle plus the sample alone failed on all three.

Install the **root**, not the intermediate. When only the `Forward Trust CA
ECDSA` intermediate was installed, curl and git succeeded (OpenSSL accepts a
partial chain), but node failed with `UNABLE_TO_GET_ISSUER_CERT`.

### 1.4 Getting the right cert onto a laptop (operator step)

Export the current root from the macOS keychain. Use its exact name, because
`-c "Okta"` also matches the expired `Okta Inc Root CA`:

```bash
# List what is there (label + SHA-256) to pick the right one:
security find-certificate -a -c "Okta" -Z | grep -E '"labl"|SHA-256'
# Export the current inspection root as PEM:
mkdir -p ~/.workingman
security find-certificate -a -c "Okta Inc Decryption Root CA" -p > ~/.workingman/extra-ca.pem
# The broad form from the task description also works, but it bundles the
# expired root as well (harmless, and the validator warns about it):
#   security find-certificate -a -c "Okta" -p > ~/.workingman/extra-ca.pem
openssl x509 -in ~/.workingman/extra-ca.pem -noout -subject -enddate -fingerprint -sha256
```

The fingerprint must match §1.2. To find out what the VPN presents without
trusting the network blindly, run this on the host while on the VPN:
`openssl s_client -connect github.com:443 -servername github.com -showcerts </dev/null | grep -E '^ *[0-9] s:|i:'`.
A DER file, such as the sample, converts with
`openssl x509 -inform DER -in corporate-ca.cer -out extra-ca.pem`.

## 2. Where trust is read inside a sandbox

From the env of PID 1 in a stock acp-kit sandbox (`/proc/1/environ`). sbx sets
these for every process, including `sbx exec`:

```
SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt
REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
PROXY_CA_CERT_B64=<Docker Sandboxes Proxy CA>
```

At create time, sbx writes its proxy CA to
`/usr/local/share/ca-certificates/proxy-ca.crt` and runs
`update-ca-certificates`. The file and the bundle have the same mtime, the
bundle holds 122 certs including the proxy CA, and the `35064f42.0` hash link
points at `proxy-ca.crt`. **sbx already uses exactly the mechanism this design
proposes**, so ours sits beside theirs.

| Reader | What it reads | Effect of installing into `/usr/local/share/ca-certificates` + `update-ca-certificates` |
|---|---|---|
| curl / libcurl-OpenSSL | `SSL_CERT_FILE`, i.e. the system bundle | fixed (measured: github.com 200) |
| git (`git-remote-https`, libcurl-**gnutls**) | its compiled-in CAfile, `/etc/ssl/certs/ca-certificates.crt`. gnutls **ignores `SSL_CERT_FILE`**: measured, `SSL_CERT_FILE=<good bundle> git ls-remote` still fails. Only `GIT_SSL_CAINFO` / `http.sslCAInfo` override it. | fixed (measured: `git ls-remote https://github.com/slimslenderslacks/workingman` ok, with no env) |
| node / Claude Code / the ACP bridge | its built-in Mozilla store plus `NODE_EXTRA_CA_CERTS`, which is the system bundle. It is read **at process start**. | fixed for every process started after the install (measured: `fetch("https://api.anthropic.com")` → HTTP 404 instead of `SELF_SIGNED_CERT_IN_CHAIN`) |
| Python `requests` | `REQUESTS_CA_BUNDLE`, the system bundle | fixed |
| Java (`/etc/ssl/certs/java/cacerts`) | the jks store | regenerated by the ca-certificates-java hook only if one is installed (`/etc/ca-certificates/update.d/` is empty in the image). Out of scope. |

**Decision: no `-e NODE_EXTRA_CA_CERTS` / `-e GIT_SSL_CAINFO`.** Both already
resolve to the system bundle that we update. Setting `GIT_SSL_CAINFO` to a
file holding only the corporate root would be **worse**: git would then stop
trusting the public roots it needs for `api.github.com`, `codeload` and other
non-inspected hosts. Leaving `sbx exec`/`sbx run`/`sbx create` env untouched
also keeps their argv byte-identical with and without the setting.

## 3. Every sandbox creation path, and where the install goes

The install is one step, `ensureExtraCA`, defined in §4.3. **It runs on every
launch, not only on create.** It is idempotent and a no-op takes about 4 ms
(measured), so a sandbox that is reused, repaired, predates the setting, or
was restarted with its filesystem reset always gets the CA before anything
uses the network. No marker file is needed.

### 3.1 acp-wrapper (every ACP session: planning, task, commit, review, wolf, workingman agent)

- `internal/acpwrapper/acpwrapper.go:606` `createSandbox`. It builds
  `sbx create claude --name <n> --kit <kit> [-t <img> --pull missing] [-e MCP_URL=...] <ws...>`
  (607-637) and runs it at 641. Policies are applied at 646-654 and the
  gateway is saved at 655.
  → **Insert `ensureExtraCA` after the policy loop and before
  `c.MCPGateway.Save(gw)` (655).** It returns through the existing
  `fail(...)`, which calls `c.MCPGateway.Discard`, like every other create
  failure.
- `internal/acpwrapper/acpwrapper.go:703` `ensureSandbox`:
  - Reuse fast path (713-716, `reapplyGatewayPolicies`) → **call
    `ensureExtraCA` after it.** A failure here is a launch error. There is no
    gateway to discard, because a reused sandbox's gateway stays recorded and
    `ensureMCPGateway` handles it.
  - Repair path: `sbx rm --force` (721) + `MCPGateway.Release` +
    `removeHarness`, then fall through to `createSandbox` (728), which is
    covered above.
- `internal/acpwrapper/harness.go:230`: the okta-gateway preflight removes
  the sandbox and returns an error, so the *next* launch goes through
  `createSandbox`. Covered.
- `internal/acpwrapper/acpwrapper.go:908` `removeSandboxOnExit`: removal only.
- Ordering in `Run` (`acpwrapper.go:986`): `ensureSandbox` (1023) → [CA is
  now installed] → `ensureMCPGateway` (1039) → `requireOktaGateway` →
  `gatewaySecretConfigured` → `llmTLSPreflight` (1061) → signing/push
  preflights → `sbx exec ... claude-acp-client` (1123). The CA is in the
  bundle before the TLS preflight and before the bridge (node) starts, which
  matters because node reads `NODE_EXTRA_CA_CERTS` only at startup.

### 3.2 Runner — legacy terminal-tab path (no `--acp-kit`)

- `cmd/orch/main.go:238` sets `r.Sandbox = runner.DefaultSandboxCreator`,
  which `internal/runner/runner.go:830` calls.
- `internal/runner/runner.go:354` `DefaultSandboxCreator`:
  - Reuse fast path (365-371) → call `ensureExtraCA`.
  - Repair path: `sbx rm --force` (373) + `Release`, then create.
  - Create: `sbx create claude --name <n> [-e MCP_URL] <ws...>` (391-397),
    then policies (398-407) → **call `ensureExtraCA` here, before
    `spec.MCPGateway.Save(gw)` (408)**, discarding the gateway on failure.
- It shells out with `exec.CommandContext(ctx, "sbx", ...)` and has no
  `commandFunc` seam. Add `SandboxSpec.ExtraCACerts string` (runner.go:308)
  and run the install through a small package var
  (`var sandboxRun = execCombined`) so tests can assert the argv.

### 3.3 Runner — interactive `:session` (`sbx run`)

- `internal/runner/runner.go:978` `LaunchInteractive` →
  `internal/runner/session.go:92` `ensureSessionSandbox`:
  - Repair: `rm --force` (120) + `Release`.
  - Create: `sbx create claude --name <n> <sessionEnv> [--kit] [-t ... --pull missing] <ws...>`
    (135-146), then gateway policies (147-152) → **call `ensureExtraCA` before
    `gw.Save(rec)` (153)**, using `fail(...)`.
  - Reuse (156-162) → call `ensureExtraCA` (a failure is an error).
- It uses `r.sbxRunner()` / `r.sbxBin()`, which is already a test seam.
  `sbx exec` on a stopped `:session` sandbox is already relied on by
  `sessionAgentArgs` (session.go:192), so exec-before-`sbx run`
  (runner.go:1004) is fine.

### 3.4 Scripts

- `sbx-create.sh`: create at 122-125, attach at 139. Add
  `--extra-ca-certs PATH` (default `$WORKINGMAN_EXTRA_CA_CERTS`, `""` for
  none). Validate it up front, before minting the gateway (line 85). After the
  `sbx policy` line (126) run the same script
  (`sbx exec "$NAME" -- sh -c "$SCRIPT" sh "$(base64 < "$CA" | tr -d '\n')"`),
  inside the `ERR` trap so a failure deletes the gateway.
  - To keep a single source of truth, the Go code is canonical and the shell
    copy is guarded by a test. `cmd/orch` (or a Go test) reads `sbx-create.sh`,
    extracts the heredoc between `WORKINGMAN_EXTRA_CA_EOF` markers and compares
    it byte-for-byte with the Go constant, in the same pattern as acp-kit's
    `test/entrypoint-consistency.mjs`.
- `start.sh` (orch invocation at 247-252) and `start-personal.sh` (which execs
  `start.sh`): **no change is required**, because orch reads
  `$WORKINGMAN_EXTRA_CA_CERTS` itself. Add one line to `start.sh`'s status echo
  (244) that prints `extra-ca-certs=${WORKINGMAN_EXTRA_CA_CERTS:-<none>}`.
- `check-signing.sh` (probe create at 119, rm at 111): it probes ssh, which
  does not use TLS, so no change.
- `check-ca.sh` (new): see §6.2.
- Kit install step (acp-kit `spec.yaml:38-176`): it runs **inside `sbx
  create`**, before workingman can install anything. It reaches
  `registry.npmjs.org`, which the VPN does not intercept, and
  `deb.nodesource.com`, which it does, but only when Node < 18, and never with
  the prebuilt image `slimslenderslacks/claude-code-acp`, whose Dockerfile
  bakes Node and the bridge in. This is a known limitation, and it does not
  occur with `--acp-image`.

### 3.5 Survives stop/start?

This could not be measured here, because this task has no `sbx` binary. The
evidence points to yes. sbx sandboxes are persistent containers:
`~/.claude/projects` persists, `:session` relies on `sbx run` restarting a
stopped sandbox (session.go:182-196), and sbx itself installs `proxy-ca.crt`
the same way at create time, which would also be lost if the root filesystem
were not kept. It does not matter either way, because the per-launch
`ensureExtraCA` re-installs the CA if it has gone. `check-ca.sh` measures it
explicitly with `sbx stop` + re-exec when the CLI has `stop` (§6.2).

## 4. Configuration: `--extra-ca-certs` / `WORKINGMAN_EXTRA_CA_CERTS`

### 4.1 sbx capabilities (assumption)

The `sbx` binary is not present in this task's sandbox, and
docs.docker.com is not reachable, so `sbx create --help` / `sbx exec --help`
could not be run. This is the same limitation noted in
`acpwrapper.go:691-702`. The design assumes **no native CA flag**, and relies
only on flags the code already uses: `sbx create -e`, `--kit`, `-t`, `--pull`;
`sbx exec [-e] [-w] <name> -- argv`; `sbx rm --force`.

It deliberately does **not** depend on `sbx exec -u root`. The script
escalates itself: as root it runs directly, otherwise it uses `sudo -n`. In
this sandbox `sbx exec` runs as `agent` (uid 1000, in `sudo`, passwordless).
`sudo -n` was measured working, and the root-owned bundle was rewritten.

A kit option was considered and rejected. The kit schema in use
(`acp-kit/spec.yaml`) has `environment.variables`, `commands.install`
(`user: root`) and `network.allowedDomains`, but no file input. A kit is also
shared across laptops, and the CA is per-laptop, so carrying it would mean
generating a per-laptop kit.

**Follow-up check.** If `sbx create --help` on the laptop shows a native CA
or file option, record it in this doc. Keep the exec mechanism anyway: it
also covers reused sandboxes.

### 4.2 The setting

There is one mechanism: the orch flag `--extra-ca-certs <path>`, defaulting
to `os.Getenv("WORKINGMAN_EXTRA_CA_CERTS")`. This follows the
`--mcp-gateway-url` / `WORKINGMAN_MCP_GATEWAY_URL` pattern at
`cmd/orch/main.go:121`. There is no implicit default file: unset or empty
means off.

- On a laptop that needs it:
  `export WORKINGMAN_EXTRA_CA_CERTS=~/.workingman/extra-ca.pem`, in the shell
  that runs `start.sh`.
- Under launchd (`orch service install`), the plist only sets `PATH`
  (`internal/service/service.go:74`), so pass it as a daemon flag:
  `orch service install --root ... -- --extra-ca-certs ~/.workingman/extra-ca.pem`
  (`service.Config.ExtraArgs`, service.go:18).

Plumbing:

1. `cmd/orch/main.go`: parse the flag next to `--acp-image` (111). Resolve it
   to an absolute path and **validate it at startup** (4.4). Exit non-zero
   (`log.Fatalf`) if it is set but invalid. Set `r.ExtraCACerts` on the
   `runner.Runner` literal (199-209), so the ACP, legacy and `:session` paths
   all see it. Audit it as `extra_ca_config path=... certs=N subjects=...`.
   Add `ExtraCACerts` to `daemon.RuntimeInfo` (283) so the state snapshot
   shows it.
2. `internal/runner/runner.go`: add a `Runner.ExtraCACerts string` field
   beside `Kit`/`Image` (529-540).
   - `acpWrapperCommand` (1236) appends `"--extra-ca-certs", path` **only when
     non-empty**, after `--template` (1254).
   - `runner.go:830` passes `ExtraCACerts: r.ExtraCACerts` in `SandboxSpec`.
   - `ensureSessionSandbox` reads `r.ExtraCACerts`.
3. `cmd/acp-wrapper/main.go`: add a `--extra-ca-certs` flag (next to `--kit`,
   123) mapped to `acpwrapper.Config.ExtraCACerts` (156).
   `Config.normalize` (`acpwrapper.go:504`) makes it absolute.
4. The wrapper **re-reads and re-validates the file on every launch** in
   `Run`, before `ensureSandbox`. A refreshed PEM therefore takes effect on
   the next session without a daemon restart. A file that has become
   unreadable fails that launch through `failRecord`.

**Unset = today, byte for byte.** There is no new flag on acp-wrapper's
argv, no `sbx exec` for the CA, and no new `-e`. The existing argv tests must
keep passing unchanged, and new tests assert the argv with the setting off.

### 4.3 The install step (canonical script)

This lives in a new `internal/sbx/extraca.go`, so that acpwrapper, runner and
the consistency test share it:

```go
// ExtraCAPath is where the extra CA bundle goes inside a sandbox.
const ExtraCAPath = "/usr/local/share/ca-certificates/workingman-extra-ca.crt"

// ExtraCAInstallArgs is the argv (after the sbx binary) that installs pem
// into sandbox's system trust store. Idempotent; exits 0 when already present.
func ExtraCAInstallArgs(sandbox string, pem []byte) []string {
	return []string{"exec", sandbox, "--", "sh", "-c", ExtraCAScript, "sh",
		base64.StdEncoding.EncodeToString(pem)}
}
```

`ExtraCAScript` is the script below (as implemented, the two paths are
overridable through the environment so a Go test can run it against a temp
bundle). It was measured in this sandbox: the
first run took 0.21 s, a repeat 0.004 s ("already installed"), and garbage
input exited 2. After installing the Decryption Root, `git ls-remote` succeeded.

```sh
set -eu
dst=${WORKINGMAN_EXTRA_CA_DST:-/usr/local/share/ca-certificates/workingman-extra-ca.crt}
bundle=${WORKINGMAN_EXTRA_CA_BUNDLE:-/etc/ssl/certs/ca-certificates.crt}
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
printf '%s' "$1" | base64 -d >"$tmp"
grep -q -- '-----BEGIN CERTIFICATE-----' "$tmp" || { echo "workingman-extra-ca: no PEM certificate in input" >&2; exit 2; }
if [ "$(id -u)" = 0 ]; then as_root=; else as_root="sudo -n"; fi
first=$(sed -n 2p "$tmp")
if cmp -s "$tmp" "$dst" && grep -qxF "$first" "$bundle"; then
  echo "workingman-extra-ca: already installed" >&2; exit 0
fi
$as_root install -m 0644 "$tmp" "$dst"
$as_root update-ca-certificates >/dev/null
grep -qxF "$first" "$bundle" || { echo "workingman-extra-ca: $bundle does not contain the extra CA after update-ca-certificates" >&2; exit 1; }
echo "workingman-extra-ca: installed $dst" >&2
```

- **The PEM travels as a base64 argv word (`$1`), not on stdin.** This keeps
  it on the existing `commandFunc(ctx, name, args...)` seam
  (`acpwrapper.go:591`), which has no stdin, in the same way
  `mcpgateway.PersistEnvArgs` passes its value as `"$1"`
  (`internal/mcpgateway/manager.go:389`). A few root certs are about 2-6 KB,
  far below `ARG_MAX`. base64 also removes any quoting concerns.
- What the host writes is the **normalized** PEM, i.e. only the
  `CERTIFICATE` blocks, re-encoded by Go. That makes the `cmp` idempotence
  check stable.
- The bundle check (`grep -qxF` of the first base64 line) catches a reset
  bundle even when the `.crt` survived.
- **Removal is out of scope.** If you turn the setting off, existing sandboxes
  keep the CA until they are recreated (`sbx rm` them, or they are recreated
  on drift). The installer does not support removal, because the off path must
  run no extra command.

The Go wrapper, as implemented
`sbx.EnsureExtraCA(ctx, run, sbxPath, sandbox string, ca ExtraCA) (string, error)`,
is a no-op (no sbx call) when `len(ca.PEM)==0`. Otherwise it runs the argv and, on a non-zero
exit, returns
`fmt.Errorf("install extra CA (%s) into sandbox %q: %w: %s", path, sandbox, err, out)`.
It logs `extra CA installed` / `already installed` from stderr.

### 4.4 Validation (host side, `internal/sbx.LoadExtraCA(path) (pem []byte, certs []*x509.Certificate, warnings []string, err error)`)

It fails loudly (an error) when:

- the file is unreadable or empty;
- it has no `-----BEGIN CERTIFICATE-----` block. If the bytes parse as DER
  (`x509.ParseCertificate`), the error includes the fix:
  `looks like DER; convert with: openssl x509 -inform DER -in <path> -out <path>.pem`;
- a `CERTIFICATE` block fails `x509.ParseCertificate`.

It warns, but does not fail, for:

- each **expired** or not-yet-valid cert, printing the subject and
  `notAfter`. Clients reject an expired anchor (§1.3), so the warning says so.
  If *every* cert is expired, the warning becomes an `ERROR`-prefixed log line
  that names §1.4. It is still not fatal: the follow-up task's contract is
  warn-only, and `check-ca.sh` is the gate that fails;
- a cert without `IsCA` (a leaf was exported by mistake).

Non-certificate PEM blocks (keys, for example) are dropped from the
normalized output and reported as a warning.

## 5. acp-kit vs workingman: workingman owns it, acp-kit is unchanged

**Decision: do it all from workingman, using the §4.3 script. Task
`acp-kit-ca-install-helper` records "no acp-kit change" and exits success.**

Reasons:

1. **Every path needs the inline script anyway.** `DefaultSandboxCreator`
   uses the stock template with no kit (runner.go:391), and older images and
   kits without the helper are still in use. A helper in the image would only
   ever be an *alternative* to the fallback, which means two implementations
   of one idempotent install that can drift.
2. **No image or kit coupling.** Shipping the helper would mean bumping
   `IMAGE_VERSION`, re-pulling the image and adding another heredoc kept in
   sync between `bin/` and `spec.yaml`. It would also change `spec.yaml`, and
   every acp-kit sandbox would be recreated through `harnessDrift`'s
   `KitDigest` check (`internal/acpwrapper/harness.go:131`), all to save about
   15 lines of sh.
3. **The tool set is already there.** `update-ca-certificates`, `base64`,
   `sudo`, `install` and `cmp` exist in the base template
   (`docker/sandbox-templates:claude-code-docker`), and sbx's own
   `proxy-ca.crt` install proves the mechanism works.
4. **acp-kit stays agent-generic.** Corporate trust is a host and laptop
   property, so it belongs with the host config in workingman.

Reconsider this if acp-kit gains a reason to install CAs at kit-install time,
for example if `deb.nodesource.com` becomes a hard dependency (§3.4). That
would need a kit-level `environment` or file input, which sbx would have to
support.

## 6. Verification and assurance

### 6.1 In-sandbox preflight (`internal/acpwrapper/llmtls.go`)

Add `gitTLSPreflight(ctx, run, c) error`, modelled on `llmTLSPreflight`
(llmtls.go:42-67). It runs right after `llmTLSPreflight` (acpwrapper.go:1061)
with the same `failRecord` handling:

```sh
command -v git >/dev/null 2>&1 || { echo "git=missing"; exit 0; }
GIT_TERMINAL_PROMPT=0 timeout 20 git ls-remote "$1" HEAD 2>&1 >/dev/null; echo "git=$?"
```

`$1` is `https://github.com/slimslenderslacks/workingman`, a constant that
tests can override. Parse the trailing `git=<n>` the way `curlStatus` does
(generalize it to `probeStatus(out, key)`).

| Outcome | Result |
|---|---|
| `0` | log `git TLS preflight OK` |
| `missing` | log skipped |
| non-zero, and the text matches `(?i)certificate` (gnutls prints `SSL certificate verification failed: ...`, OpenSSL builds print `SSL certificate problem: ...`) | **error**: `git TLS preflight FAILED for sandbox %q: https://github.com presents a certificate chain the sandbox does not trust (<detail>)` + `gitTLSHint` |
| any other non-zero (policy-denied network, DNS, timeout) | **warning only**, like the LLM probe |

`gitTLSHint`:

- If `c.ExtraCACerts == ""`: `the network re-signs github.com with a CA the
  sandbox does not hold (VPN TLS inspection). Export the current corporate
  root (docs/sandbox-custom-ca.md §1.4) and set --extra-ca-certs /
  WORKINGMAN_EXTRA_CA_CERTS`.
- If it is set: `the configured extra CA (<path>: <subjects>, notAfter
  <dates>) does not validate github.com's chain. It is the wrong root or
  EXPIRED. Re-export it (§1.4) and run ./check-ca.sh <path>`. If the detail
  contains `expired`, say so first.

Also extend `llmTLSHint` (llmtls.go:70) for the direct-API branch. When the
extra CA is unset, mention `--extra-ca-certs` as the alternative fix to the
global LLM gateway secret. With the Decryption Root installed, the direct
`api.anthropic.com` path validates (measured, §2).

Unit tests use the existing fake `commandFunc`: OK, cert-fail-without-CA
(the hint names the flag and env), cert-fail-with-CA (the hint names the
path, and says expired when applicable), missing git, and non-cert failure
(nil error).

### 6.2 Host script `check-ca.sh` (modelled on `check-signing.sh`)

`./check-ca.sh [PEM]`, where PEM defaults to `$WORKINGMAN_EXTRA_CA_CERTS`.
Add the Taskfile target `check:ca`.

1. Require `sbx`, `openssl` and `base64`. Validate that PEM is PEM (give the
   DER conversion hint otherwise). For each cert, print subject, issuer,
   notAfter and SHA-256. **If any cert is expired, print
   `EXPIRED — clients will not trust it (see docs/sandbox-custom-ca.md §1.3)`**,
   and fail at the end if none is valid.
2. Create the probe sandbox `ca-probe-$$` the same way the daemon does:
   `sbx create claude --name "$PROBE" [--kit "$ACP_KIT"] [-t "$ACP_IMAGE" --pull missing] "$PROBE_WS"`.
   Use the same defaults as `sbx-create.sh:41-46`. Use
   `trap cleanup EXIT` → `sbx rm --force "$PROBE"`, as in
   check-signing.sh:110-114.
3. **Without** the CA, inside the probe, run
   `git ls-remote https://github.com/slimslenderslacks/workingman HEAD`,
   `curl -sS -o /dev/null https://github.com` and
   `curl -sS -o /dev/null https://api.anthropic.com`. Expect a cert failure on
   a TLS-inspecting network. If they pass, report "this network does not need
   an extra CA".
4. Install the CA with **the exact daemon step**: extract the script from
   `sbx-create.sh`'s `WORKINGMAN_EXTRA_CA_EOF` heredoc, or have `orch` print
   it. Run it twice, and the second run must say `already installed`.
5. **With** the CA, run the same three probes. All must pass (curl exit 0 and
   any HTTP status, git exit 0).
6. If `sbx stop --help` works: `sbx stop "$PROBE"`, then re-run the probes via
   `sbx exec` (which starts the sandbox) **without** re-installing. Report
   whether the install survived stop/start. This is informational, because
   the daemon re-ensures on every launch anyway.
7. Print a summary table (probe × without/with), and exit non-zero unless
   every "with" probe passed.

The task agent cannot run sbx. **The operator must run
`./check-ca.sh ~/.workingman/extra-ca.pem` on the laptop** for the
end-to-end proof. Run it once with the sample cert converted to PEM, which
should fail as expired and wrong-root, and once with the keychain export,
which should pass.

### 6.3 setup.md

Add a "Custom CA (VPN / TLS inspection)" section covering: when you need it
(symptoms: `git ... SSL certificate verification failed: certificate signer
not trusted`, or the LLM preflight's "Self-signed certificate detected");
§1.4's export commands; how to enable it per laptop (the env var for
`start.sh`, `-- --extra-ca-certs` for launchd); running `check-ca.sh`; and a
note that the sample `corporate-ca.cer` is DER, the wrong root and expired.

## 7. Failure behaviour

| Situation | Behaviour |
|---|---|
| Setting unset | Today's behaviour, byte for byte |
| Set, but the file is missing, unreadable, DER or not a cert | orch exits at startup with the reason. acp-wrapper (re-validating per launch) fails that launch through `failRecord`, and the daemon retries or blocks with the reason |
| Set, cert expired or not a CA | Startup warning (subject + notAfter). The launch proceeds. The git TLS preflight then fails with the "configured CA is wrong/EXPIRED" hint |
| Install exits non-zero on a new sandbox | `createSandbox` / `DefaultSandboxCreator` / `ensureSessionSandbox` return through `fail()` → `MCPGateway.Discard`. The sandbox is left for the next launch, which re-ensures, or for repair |
| Install exits non-zero on a reused sandbox | Launch error `install extra CA ... into sandbox ...: <stderr>`. The sandbox and gateway are kept |
| No passwordless sudo and not root | The script prints a `sudo: a password is required` error → the launch fails with it. If `sbx exec -u root` exists, the remedy is to switch the argv to it |

## 8. Doc updates in the follow-up tasks

- `docs/sandbox-provider-design.md` §2.1 table: add a row
  `sbx exec <n> -- sh -c <ExtraCAScript> sh <b64>` (Who: `acpwrapper.createSandbox`,
  `acpwrapper.ensureSandbox` reuse, `runner.DefaultSandboxCreator`,
  `runner.ensureSessionSandbox`, `sbx-create.sh`, `check-ca.sh`; only when
  `--extra-ca-certs` is set; failure is fatal). Also note the git TLS preflight
  next to the LLM one.
- `docs/mcp-gateway.md`: in the create sequence, show the CA install step
  between the policies and recording the gateway.
- `README.md` / `setup.md` flag tables: `--extra-ca-certs` /
  `WORKINGMAN_EXTRA_CA_CERTS`.
- Package comments for `internal/sbx` (if `extraca.go` adds surface), then
  `task docs` to regenerate `docs/packages.md` (`task docs:check`).

## 9. Implementation checklist

### `acp-kit-ca-install-helper`
- [ ] Per §5, **no acp-kit change**. Record in the task result that the design
      keeps the install in workingman (stock-template path, no image bump, no
      second implementation), and exit success. Do not bump `IMAGE_VERSION`.

### `workingman-extra-ca-config`
- [ ] `internal/sbx/extraca.go`: `LoadExtraCA(path)`. It validates and
      normalizes (§4.4): errors for missing, empty or non-PEM files (with the
      DER hint) and for bad certs; warnings for expired, not-yet-valid and
      non-CA certs, and an `ERROR`-prefixed line when all are expired. Add
      table tests, including a DER fixture and an expired fixture generated in
      the test with `crypto/x509`.
- [ ] `cmd/orch/main.go`: add `--extra-ca-certs` (default
      `$WORKINGMAN_EXTRA_CA_CERTS`). Make it absolute, `LoadExtraCA` it at
      startup and `log.Fatalf` on error. Print warnings to stderr and audit
      `extra_ca_config`. Set `Runner.ExtraCACerts` and
      `RuntimeInfo.ExtraCACerts`.
- [ ] `runner.Runner.ExtraCACerts`, `SandboxSpec.ExtraCACerts`.
      `acpWrapperCommand` emits `--extra-ca-certs <path>` only when set.
- [ ] `cmd/acp-wrapper`: add the `--extra-ca-certs` flag →
      `acpwrapper.Config.ExtraCACerts` (made absolute in `normalize`).
      `Run` re-loads it before `ensureSandbox` and fails the launch if it is
      invalid.
- [ ] Tests: the acp-wrapper argv and every existing sbx argv are unchanged
      when unset, and the flag is present when set.
- [ ] `setup.md` / `README.md` flag tables. Add the `start.sh` status echo
      line (§3.4). Document `orch service install -- --extra-ca-certs`.
      Run `go test ./...` and `task docs:check`.

### `apply-ca-on-sandbox-create`
- [x] `internal/sbx`: `ExtraCAScript`, `ExtraCAPath`, `ExtraCAInstallArgs`
      (§4.3). There is no acp-kit helper, so the §4.3 script is the only
      mechanism. Do **not** add `-e NODE_EXTRA_CA_CERTS/GIT_SSL_CAINFO` (§2).
- [x] acpwrapper: `ensureExtraCA` in `createSandbox` (after policies, before
      `Save`, via `fail`) and on `ensureSandbox`'s reuse path. Repair
      (`rm --force` + create) is covered by `createSandbox`.
- [x] runner: `DefaultSandboxCreator` (create path before `Save` with
      `Discard` on failure; reuse path), with a `sandboxRun` seam for tests.
      `ensureSessionSandbox` (create before `gw.Save` via `fail`; reuse).
- [x] Existing and pre-config sandboxes are covered by the per-launch ensure.
      No marker is needed (§3).
- [x] `sbx-create.sh --extra-ca-certs` (default `$WORKINGMAN_EXTRA_CA_CERTS`)
      with the script in a `WORKINGMAN_EXTRA_CA_EOF` heredoc, and a Go test
      asserting it is byte-identical to `sbx.ExtraCAScript`.
- [x] Tests (fake `commandFunc` / `sbxRunner` / `sandboxRun`): the exact argv
      with the setting on (`exec <n> -- sh -c <script> sh <b64>`) and off (no
      such call); a failure discards the gateway on create and errors on
      reuse.
- [x] Optional: run the §4.3 script in a Go test against `/bin/sh` with a temp
      `dst`/`bundle` (parametrize the paths through env defaults) to cover
      idempotence and bad input.
- [x] Docs: §8. Run `go test ./...` and `task docs:check`.

### `ca-preflight-and-check-script`
- [x] `gitTLSPreflight` + `gitTLSHint` in `llmtls.go`, run after
      `llmTLSPreflight`. Generalize `curlStatus` → `probeStatus`. Extend
      `llmTLSHint` with the extra-CA alternative. Add tests: OK,
      cert-fail-without-CA, cert-fail-with-CA (expired wording), missing git,
      non-cert warning.
- [x] `check-ca.sh` per §6.2 (without/with probes, double install for
      idempotence, optional stop/start persistence check, trap cleanup), plus
      the Taskfile `check:ca` target. Check it with `bash -n` / shellcheck.
- [x] `setup.md` "Custom CA (VPN / TLS inspection)" section (§6.3), including
      the expected fingerprint of `Okta Inc Decryption Root CA` from §1.2.
- [x] Result note: the operator must run `./check-ca.sh
      ~/.workingman/extra-ca.pem` on the laptop for the end-to-end proof.
