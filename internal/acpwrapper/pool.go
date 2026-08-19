// Warm-sandbox pool: acp-kit's install step runs fresh on every `sbx create`
// (Node 22 + the ACP client's native binary, pulled over the network), so a
// session that always creates its own sandbox and tears it down on exit pays
// that cost on every single launch. The pool lets ensureSandbox adopt a
// pre-built *idle* sandbox from a prior session instead, and lets
// removeSandboxOnExit donate a sandbox back as a spare instead of `sbx rm
// --force`-ing it, so a soon-to-follow session (the matching commit agent, or
// the project's next task) can skip cold-start entirely.
//
// sbx itself has no pool concept, so membership is tracked in a small local
// metadata store, analogous to the session package's on-disk layout. Entries
// are keyed by a signature of the sandbox's shape (kit + workspace set +
// static MCPs + policies) — see poolSignatureFor — because the existing
// same-name reuse fast path in ensureSandbox relies on "the sandbox name
// encodes the MCP/policy set" to skip reapplying them; a pooled sandbox must
// only ever be adopted when that whole set matches exactly.
package acpwrapper

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/policy"
)

// defaultPoolCap is how many idle spares removeSandboxOnExit keeps per
// signature before falling back to `sbx rm --force`, when Config.PoolCap is
// unset. Small on purpose: a handful of spares is enough to absorb the
// task -> commit handoff and back-to-back tasks without sandboxes piling up.
const defaultPoolCap = 2

// poolStatus is a pooled sandbox's membership state.
type poolStatus string

const (
	poolIdle poolStatus = "idle"
	poolBusy poolStatus = "busy"
)

// poolEntry is the on-disk record for one pooled sandbox.
type poolEntry struct {
	SandboxName string     `json:"sandbox_name"`
	Signature   string     `json:"signature"`
	Status      poolStatus `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Pool is a warm-sandbox metadata store rooted at Root. Layout:
//
//	<Root>/<signature>/idle/<sandboxName>.json
//	<Root>/<signature>/busy/<sandboxName>.json
//
// idle/busy are separate directories (rather than a status field alone) so
// claiming an idle entry is a single os.Rename: the first caller to move a
// file out of idle/ into busy/ wins it, and every loser just sees the file is
// already gone. That makes claim() safe under concurrent callers (e.g. a task
// and its commit agent racing to adopt the same spare) without a lock file.
type Pool struct {
	Root string
}

func (p Pool) sigDir(sig string) string  { return filepath.Join(p.Root, sig) }
func (p Pool) idleDir(sig string) string { return filepath.Join(p.sigDir(sig), "idle") }
func (p Pool) busyDir(sig string) string { return filepath.Join(p.sigDir(sig), "busy") }

// poolSignatureFor computes the signature key two sandboxes must share to be
// considered interchangeable. Workspaces and StaticMCPs are sorted before
// hashing because sbx exposes no ordering semantics for either (mirrors
// sameWorkspaceSet's set comparison); Policies are hashed in declaration
// order because they apply in that order (see ensureSandbox) and a
// deny-all-then-allow-host rule set is not equivalent to the reverse.
func poolSignatureFor(kitPath string, workspaces, staticMCPs []string, policies []policy.Rule) string {
	ws := append([]string(nil), workspaces...)
	sort.Strings(ws)
	mcps := append([]string(nil), staticMCPs...)
	sort.Strings(mcps)

	h := sha256.New()
	fmt.Fprintf(h, "kit=%s\n", kitPath)
	for _, w := range ws {
		fmt.Fprintf(h, "ws=%s\n", w)
	}
	for _, m := range mcps {
		fmt.Fprintf(h, "mcp=%s\n", m)
	}
	for _, r := range policies {
		fmt.Fprintf(h, "policy=%s\n", r.Encode())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// poolSpareName synthesizes a name for a sandbox the pool creates on its own
// (background pre-warming), independent of any session id. It carries the
// signature's prefix purely as a debugging aid — sbx ls/inspect output makes
// it obvious which spares belong to which shape.
func poolSpareName(sig string) string {
	short := sig
	if len(short) > 8 {
		short = short[:8]
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		// crypto/rand failing is effectively unrecoverable, but a collision-prone
		// name is still better than crashing the background pre-warmer.
		return fmt.Sprintf("acp-pool-%s-%d", short, time.Now().UnixNano())
	}
	return fmt.Sprintf("acp-pool-%s-%x", short, suffix)
}

// claim atomically takes one idle sandbox for sig, marking it busy, and
// returns its name. ok is false when no idle entry exists.
func (p Pool) claim(sig string) (name string, ok bool, err error) {
	idleDir := p.idleDir(sig)
	entries, err := os.ReadDir(idleDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("pool: list %s: %w", idleDir, err)
	}
	busyDir := p.busyDir(sig)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if err := os.MkdirAll(busyDir, 0o755); err != nil {
			return "", false, fmt.Errorf("pool: create %s: %w", busyDir, err)
		}
		src := filepath.Join(idleDir, e.Name())
		dst := filepath.Join(busyDir, e.Name())
		if err := os.Rename(src, dst); err != nil {
			// Another caller claimed it first (or it was reconciled/discarded
			// out from under us); try the next candidate rather than failing.
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		now := time.Now()
		entry := poolEntry{SandboxName: name, Signature: sig, Status: poolBusy, CreatedAt: now, UpdatedAt: now}
		if data, mErr := json.Marshal(entry); mErr == nil {
			_ = writePoolFileAtomic(dst, data) // best-effort refresh; the rename alone already claimed it
		}
		return name, true, nil
	}
	return "", false, nil
}

// release records name (currently believed idle, e.g. a session donating its
// sandbox back on a clean exit) as an idle spare for sig, unless doing so
// would push the signature's idle count above cap — in which case released is
// false and the caller should fall back to `sbx rm --force` instead of
// growing the pool unbounded. Any stale busy record for name is dropped
// either way, since a released sandbox is by definition no longer in use.
func (p Pool) release(sig, name string, maxIdle int) (released bool, err error) {
	idleDir := p.idleDir(sig)
	count, err := countPoolEntries(idleDir)
	if err != nil {
		return false, err
	}
	_ = os.Remove(filepath.Join(p.busyDir(sig), name+".json"))
	if count >= maxIdle {
		return false, nil
	}
	if err := os.MkdirAll(idleDir, 0o755); err != nil {
		return false, fmt.Errorf("pool: create %s: %w", idleDir, err)
	}
	now := time.Now()
	entry := poolEntry{SandboxName: name, Signature: sig, Status: poolIdle, CreatedAt: now, UpdatedAt: now}
	data, err := json.Marshal(entry)
	if err != nil {
		return false, fmt.Errorf("pool: marshal %s: %w", name, err)
	}
	if err := writePoolFileAtomic(filepath.Join(idleDir, name+".json"), data); err != nil {
		return false, err
	}
	return true, nil
}

// discard drops any record of name under sig without adding it back anywhere
// — used when an adopted pool entry turns out to be stale (the sandbox it
// named no longer exists, or its workspace set no longer matches) so it is
// never offered again.
func (p Pool) discard(sig, name string) {
	_ = os.Remove(filepath.Join(p.idleDir(sig), name+".json"))
	_ = os.Remove(filepath.Join(p.busyDir(sig), name+".json"))
}

// idleCount reports how many idle spares sig currently has, used to decide
// whether background pre-warming should fire (see Run).
func (p Pool) idleCount(sig string) (int, error) {
	return countPoolEntries(p.idleDir(sig))
}

func countPoolEntries(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("pool: list %s: %w", dir, err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n, nil
}

// reconcile demotes every busy entry whose sandbox name is not in live back
// to idle. It exists so a crashed acp-wrapper — one that claimed a pool entry
// and then exited without ever reaching removeSandboxOnExit — doesn't leave
// that sandbox stuck "busy" forever; mirrors how the daemon's
// reconcileSessions re-adopts on-disk session records after a restart. live
// should be the set of sandbox names backing sessions currently recorded as
// starting/running in the session store; anything else claiming to be busy is
// necessarily stale.
func (p Pool) reconcile(live map[string]bool) error {
	sigDirs, err := os.ReadDir(p.Root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("pool: list %s: %w", p.Root, err)
	}
	for _, sd := range sigDirs {
		if !sd.IsDir() {
			continue
		}
		sig := sd.Name()
		busyDir := p.busyDir(sig)
		entries, err := os.ReadDir(busyDir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("pool: list %s: %w", busyDir, err)
		}
		idleDir := p.idleDir(sig)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".json")
			if live[name] {
				continue
			}
			if err := os.MkdirAll(idleDir, 0o755); err != nil {
				return fmt.Errorf("pool: create %s: %w", idleDir, err)
			}
			src := filepath.Join(busyDir, e.Name())
			dst := filepath.Join(idleDir, e.Name())
			if err := os.Rename(src, dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("pool: reconcile %s: %w", src, err)
			}
		}
	}
	return nil
}

// writePoolFileAtomic writes data to path via write-temp-then-rename, so a
// concurrent claim()/reconcile() never observes a half-written entry.
// Mirrors session.atomicWrite; duplicated locally because that helper is
// unexported from the session package and this store's entries are a
// different shape.
func writePoolFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("pool: create dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".pool-*")
	if err != nil {
		return fmt.Errorf("pool: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("pool: write temp %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pool: close temp %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("pool: rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}
