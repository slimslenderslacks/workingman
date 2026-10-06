package bridge

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

//go:embed assets
var assetFS embed.FS

// Script is the bridge entry point inside an install directory.
const Script = "bridge.js"

// depsMarkerPrefix names the file recording which package-lock.json the
// node_modules next to it were installed from.
const depsMarkerPrefix = ".deps-"

// Extract writes the embedded bridge sources into dir, replacing files whose
// content differs. Node tests are not extracted.
func Extract(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("bridge: create install dir: %w", err)
	}
	return fs.WalkDir(assetFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".test.mjs") {
			return err
		}
		data, err := assetFS.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, "assets/")))
		if cur, err := os.ReadFile(dst); err == nil && string(cur) == string(data) {
			return nil
		}
		return os.WriteFile(dst, data, 0o644)
	})
}

// lockHash identifies the dependency set the embedded bridge needs.
func lockHash() string {
	var h = sha256.New()
	for _, name := range []string{"package.json", "package-lock.json"} {
		data, _ := assetFS.ReadFile(path.Join("assets", name))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Ready reports whether dir holds the current bridge sources and its
// installed dependencies, i.e. whether it can be launched without Install.
func Ready(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, Script)); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "@whiskeysockets", "baileys")); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, depsMarkerPrefix+lockHash()))
	return err == nil
}

// Install makes dir launchable: it extracts the sources and, when the
// dependencies are missing or stale, runs `npm ci` there (progress goes to
// out). It needs network access the first time. Lifecycle scripts are
// disabled: none of the bridge's dependencies need them to run.
func Install(ctx context.Context, dir, npmPath string, out io.Writer) error {
	if err := Extract(dir); err != nil {
		return err
	}
	if Ready(dir) {
		return nil
	}
	if npmPath == "" {
		return errors.New("bridge: npm path required to install dependencies")
	}
	if out == nil {
		out = io.Discard
	}
	cmd := exec.CommandContext(ctx, npmPath, "ci", "--omit=dev", "--ignore-scripts", "--no-audit", "--no-fund")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bridge: installing dependencies with npm ci in %s: %w", dir, err)
	}
	// Drop markers from older dependency sets, then record this one.
	old, _ := filepath.Glob(filepath.Join(dir, depsMarkerPrefix+"*"))
	for _, f := range old {
		_ = os.Remove(f)
	}
	return os.WriteFile(filepath.Join(dir, depsMarkerPrefix+lockHash()), nil, 0o644)
}
